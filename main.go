package main

import (
    "fmt"
    "html/template"
    "io"
    "log"
    "mime"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "strings"
    "time"
    "github.com/skip2/go-qrcode"
    "path"
    "net/url"
    "mime/multipart"
    "encoding/json"
    //"encoding/base64"
    "strconv"
    "sync"
)

const (
    uploadDir = "./uploads"
    maxWorkers = 50               // Increased from 10 to 20
    maxConcurrentUploads = 50     // New: Maximum concurrent upload handlers
    maxRetries = 3              // Maximum number of retry attempts
    chunkSize = 32 * 1024 * 1024  // Increased from 8MB to 16MB chunks
    bufferSize = 512 * 1024       // Increased from 32KB to 256KB buffer
    tempDir = "./uploads/temp"   // Add temporary directory for chunks
    maxNetworkRetries = 3
    networkRetryDelay = 100 * time.Millisecond
    videoChunkSize = 64 * 1024 * 1024  // 64MB chunks for videos
    maxVideoWorkers = 8                 // Maximum parallel video chunk processors
)

type FileInfo struct {
    Name      string
    Path      string
    Size      int64
    ModTime   time.Time
    IsImage   bool
    MimeType  string
    IsDir     bool
    Parent    string
}

type ViewData struct {
    Files      []FileInfo
    Path       string
    Breadcrumb []BreadcrumbItem
    Parent     string
    Stats      struct {
        TotalFiles    int
        TotalFolders  int
        TotalSize     int64
    }
}

type BreadcrumbItem struct {
    Name string
    Path string
}

type UploadTask struct {
    File     *multipart.FileHeader
    TargetDir string
    Result   chan error
}

type uploadResult struct {
    Filename string `json:"filename"`
    Success  bool   `json:"success"`
    Error    string `json:"error,omitempty"`
}

type ChunkInfo struct {
    UploadID    string `json:"uploadId"`
    ChunkNumber int    `json:"chunkNumber"`
    TotalChunks int    `json:"totalChunks"`
    FileName    string `json:"fileName"`
    Chunk       string `json:"chunk"`  // Base64 encoded chunk data
    CurrentPath string `json:"currentPath"` // Add current path
}

type UploadWorkerPool struct {
    workers chan struct{}
    wg      sync.WaitGroup
}

func NewUploadWorkerPool(maxWorkers int) *UploadWorkerPool {
    return &UploadWorkerPool{
        workers: make(chan struct{}, maxWorkers),
    }
}

func (p *UploadWorkerPool) Submit(task func()) {
    p.workers <- struct{}{} // Acquire worker
    p.wg.Add(1)
    go func() {
        defer func() {
            <-p.workers // Release worker
            p.wg.Done()
        }()
        task()
    }()
}

func (p *UploadWorkerPool) Wait() {
    p.wg.Wait()
}

var (
    uploadQueue chan UploadTask
    activeUploads = make(map[string]ActiveUploader)
    activeUploadsMutex sync.RWMutex // Add mutex for map access
    uploadSemaphore = make(chan struct{}, maxConcurrentUploads) // Add rate limiter for uploads
)

type ActiveUploader interface {
    GetTempFile() *os.File
    GetReceived() map[int]bool
    GetTargetPath() string
    IsVideoUpload() bool
    ProcessChunk(chunk io.Reader, chunkNumber int) error
}

// Modify ActiveUpload to implement ActiveUploader
type ActiveUpload struct {
    TargetPath  string
    TotalChunks int
    Received    map[int]bool
    TempFile    *os.File
}

func (a *ActiveUpload) GetTempFile() *os.File { return a.TempFile }
func (a *ActiveUpload) GetReceived() map[int]bool { return a.Received }
func (a *ActiveUpload) GetTargetPath() string { return a.TargetPath }
func (a *ActiveUpload) IsVideoUpload() bool { return false }
func (a *ActiveUpload) ProcessChunk(chunk io.Reader, chunkNumber int) error {
    buffer := make([]byte, bufferSize)
    offset := int64(chunkNumber * chunkSize)
    
    writer := &offsetWriter{
        file:   a.TempFile,
        offset: offset,
        buffer: buffer,
    }
    
    _, err := io.CopyBuffer(writer, chunk, buffer)
    return err
}

// Modify VideoUpload to implement ActiveUploader
type VideoUpload struct {
    *ActiveUpload
    ChunkPool   *sync.Pool
    Workers     chan struct{}
}

func (v *VideoUpload) IsVideoUpload() bool { return true }
func (v *VideoUpload) ProcessChunk(chunk io.Reader, chunkNumber int) error {
    select {
    case v.Workers <- struct{}{}:
        defer func() { <-v.Workers }()
    default:
        time.Sleep(100 * time.Millisecond)
        v.Workers <- struct{}{}
        defer func() { <-v.Workers }()
    }

    buffer := v.ChunkPool.Get().([]byte)
    defer v.ChunkPool.Put(buffer)
    
    writer := &offsetWriter{
        file:   v.TempFile,
        offset: int64(chunkNumber * videoChunkSize),
        buffer: buffer,
    }

    _, err := io.CopyBuffer(writer, chunk, buffer)
    return err
}

func init() {
    // Configure logging
    log.SetFlags(log.LstdFlags | log.Lshortfile)
    f, err := os.OpenFile("server.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err == nil {
        log.SetOutput(f)
    }

    // Add MIME type mapping for common file types
    mime.AddExtensionType(".pdf", "application/pdf")
    mime.AddExtensionType(".doc", "application/msword")
    mime.AddExtensionType(".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
    mime.AddExtensionType(".xls", "application/vnd.ms-excel")
    mime.AddExtensionType(".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")

    uploadQueue = make(chan UploadTask, 100) // Buffer for up to 100 pending uploads
    // Start upload workers
    for i := 0; i < maxWorkers; i++ {
        go uploadWorker()
    }

    // Create temp directory for uploads
    os.MkdirAll(tempDir, 0755)

    // Add cleanup routine
    go func() {
        for {
            time.Sleep(1 * time.Hour)
            cleanupTempFiles()
        }
    }()
}

func main() {
    os.MkdirAll(uploadDir, 0755)
    
    // Wrap handlers with logging middleware
    http.Handle("/", logRequest(handleHome))
    http.Handle("/upload", logRequest(handleUpload))
    http.Handle("/files/", logRequest(handleFileServing))
    http.Handle("/qr", logRequest(handleQR))
    http.Handle("/dav/", logRequest(handleWebDAV))
    http.HandleFunc("/folder", handleCreateFolder)
    http.HandleFunc("/move", handleMove)
    http.HandleFunc("/delete", handleDelete)
    http.HandleFunc("/chunk-upload", handleChunkUpload)
    http.HandleFunc("/batch-delete", handleBatchDelete)
    
    ip := getLocalIP()
    port := ":8080"
    fmt.Printf("Server running at http://%s%s\n", ip, port)
    http.ListenAndServe(port, nil)
}

func logRequest(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        log.Printf("Started %s %s", r.Method, r.URL.Path)
        next.ServeHTTP(w, r)
        log.Printf("Completed %s %s in %v", r.Method, r.URL.Path, time.Since(start))
    }
}

func logOperation(operation, filename string, size int64) {
    fmt.Printf("[%s] %s: %s (%s)\n", 
        time.Now().Format("2006-01-02 15:04:05"),
        operation,
        filename,
        formatFileSize(size))
}

func handleHome(w http.ResponseWriter, r *http.Request) {
    currentPath := r.URL.Query().Get("path")
    if currentPath == "" {
        currentPath = "."
    }
    
    // Clean and validate the path
    currentPath = filepath.Clean(currentPath)
    currentPath = strings.TrimPrefix(currentPath, "/")
    
    if strings.Contains(currentPath, "..") {
        http.Redirect(w, r, "/", http.StatusFound)
        return
    }
    
    fullPath := filepath.Join(uploadDir, currentPath)
    absUploadDir, _ := filepath.Abs(uploadDir)
    absFullPath, _ := filepath.Abs(fullPath)
    
    if (!strings.HasPrefix(absFullPath, absUploadDir)) {
        http.Redirect(w, r, "/", http.StatusFound)
        return
    }

    // Check if directory exists
    info, err := os.Stat(fullPath)
    if err != nil || !info.IsDir() {
        log.Printf("Invalid directory: %v", err)
        http.Redirect(w, r, "/", http.StatusFound)
        return
    }

    // Read directory contents
    files, err := os.ReadDir(fullPath)
    if err != nil {
        log.Printf("Error reading directory: %v", err)
        http.Error(w, "Error reading directory", http.StatusInternalServerError)
        return
    }

    var fileInfos []FileInfo
    for _, f := range files {
        info, err := f.Info()
        if err != nil {
            continue
        }

        filePath := filepath.Join(currentPath, f.Name())
        mimeType := getMimeType(f.Name())
        fi := FileInfo{
            Name:     f.Name(),
            Path:     filePath,
            Size:     info.Size(),
            ModTime:  info.ModTime(),
            IsImage:  !f.IsDir() && strings.HasPrefix(mimeType, "image/"),
            MimeType: mimeType,
            IsDir:    f.IsDir(),
            Parent:   currentPath,
        }
        fileInfos = append(fileInfos, fi)
    }

    // Generate breadcrumb with proper path handling
    var breadcrumb []BreadcrumbItem
    breadcrumb = append(breadcrumb, BreadcrumbItem{Name: "Home", Path: ""})
    
    if currentPath != "." {
        parts := strings.Split(currentPath, string(os.PathSeparator))
        currentBreadcrumb := ""
        for _, part := range parts {
            if part != "." {
                currentBreadcrumb = path.Join(currentBreadcrumb, part)
                breadcrumb = append(breadcrumb, BreadcrumbItem{
                    Name: part,
                    Path: currentBreadcrumb,
                })
            }
        }
    }

    // Calculate stats
    stats := struct {
        TotalFiles    int
        TotalFolders  int
        TotalSize     int64
    }{}

    // Calculate stats including nested directories
    err = filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return nil // Skip files we can't access
        }
        if info.IsDir() {
            if path != fullPath { // Don't count current directory
                stats.TotalFolders++
            }
        } else {
            stats.TotalFiles++
            stats.TotalSize += info.Size()
        }
        return nil
    })

    if err != nil {
        log.Printf("Error calculating directory stats: %v", err)
    }

    viewData := ViewData{
        Files:      fileInfos,
        Path:       currentPath,
        Breadcrumb: breadcrumb,
        Parent:     filepath.Dir(currentPath),
        Stats:      stats,
    }

    funcMap := template.FuncMap{
        "basename": path.Base,
        "formatSize": func(size int64) string {
            return formatFileSize(size)
        },
        "formatTime": func(t time.Time) string {
            return t.Format("2006-01-02 15:04:05")
        },
        "getFileIcon": func(mimeType string) string {
            switch {
            case strings.HasPrefix(mimeType, "image/"):
                return "fas fa-image"
            case strings.HasPrefix(mimeType, "video/"):
                return "fas fa-video"
            case strings.HasPrefix(mimeType, "audio/"):
                return "fas fa-music"
            case strings.Contains(mimeType, "pdf"):
                return "fas fa-file-pdf"
            case strings.Contains(mimeType, "word"):  // Fixed contains to Contains
                return "fas fa-file-word"
            case strings.Contains(mimeType, "excel"):
                return "fas fa-file-excel"
            case strings.Contains(mimeType, "zip") || strings.Contains(mimeType, "compressed"):
                return "fas fa-file-archive"
            default:
                return "fas fa-file"
            }
        },
    }

    tmpl := template.Must(template.New("index.html").Funcs(funcMap).ParseFiles("templates/index.html"))
    if err := tmpl.Execute(w, viewData); err != nil {
        log.Printf("Template error: %v", err)
    }
}

// Modify handleUpload to use worker pool
func handleUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    currentPath := filepath.Clean(r.FormValue("path"))
    if currentPath == "" {
        currentPath = "."
    }

    targetDir := filepath.Join(uploadDir, currentPath)
    if err := os.MkdirAll(targetDir, 0755); err != nil {
        http.Error(w, "Failed to create directory", http.StatusInternalServerError)
        return
    }

    r.ParseMultipartForm(32 << 20)
    files := r.MultipartForm.File["files"]
    
    pool := NewUploadWorkerPool(maxWorkers)
    results := make([]uploadResult, len(files))
    
    for i, fileHeader := range files {
        i, fileHeader := i, fileHeader // Create new variables for goroutine
        pool.Submit(func() {
            result := processUpload(fileHeader, targetDir)
            results[i] = result
        })
    }

    pool.Wait() // Wait for all uploads to complete

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "success": true,
        "results": results,
    })
}

func processUpload(fileHeader *multipart.FileHeader, targetDir string) uploadResult {
    result := uploadResult{Filename: fileHeader.Filename}
    
    // Try upload with retries
    var err error
    for attempt := 0; attempt < maxRetries; attempt++ {
        if err = uploadFile(fileHeader, targetDir); err == nil {
            result.Success = true
            return result
        }
        time.Sleep(time.Duration(attempt*100) * time.Millisecond)
    }
    
    result.Success = false
    result.Error = err.Error()
    return result
}

func uploadFile(fileHeader *multipart.FileHeader, targetDir string) error {
    return withRetry(func() error {
        src, err := fileHeader.Open()
        if err != nil {
            return fmt.Errorf("failed to open source: %v", err)
        }
        defer src.Close()

        dst, err := os.OpenFile(
            filepath.Join(targetDir, fileHeader.Filename),
            os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
            0644,
        )
        if err != nil {
            return fmt.Errorf("failed to create destination: %v", err)
        }
        defer dst.Close()

        // Use buffered copy for better performance
        buf := make([]byte, bufferSize)
        written, err := io.CopyBuffer(dst, src, buf)
        if err != nil {
            return fmt.Errorf("failed to copy: %v", err)
        }

        // Ensure data is written to disk
        if err := dst.Sync(); err != nil {
            return fmt.Errorf("failed to sync: %v", err)
        }

        logOperation("UPLOAD", filepath.Join(targetDir, fileHeader.Filename), written)
        return nil
    })
}

func uploadWorker() {
    for task := range uploadQueue {
        file, err := task.File.Open()
        if err != nil {
            task.Result <- err
            continue
        }

        dst, err := os.Create(filepath.Join(task.TargetDir, task.File.Filename))
        if err != nil {
            file.Close()
            task.Result <- err
            continue
        }

        written, err := io.Copy(dst, file)
        file.Close()
        dst.Close()
        
        if err == nil {
            logOperation("UPLOAD", filepath.Join(task.TargetDir, task.File.Filename), written)
        }
        
        task.Result <- err
    }
}

// Modify handleFileServing to include retries
func handleFileServing(w http.ResponseWriter, r *http.Request) {
    // Clean the requested path
    requestPath := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/files/"))
    filePath := filepath.Join(uploadDir, requestPath)
    
    // Validate the path is within upload directory
    absUploadDir, _ := filepath.Abs(uploadDir)
    absFilePath, _ := filepath.Abs(filePath)
    if !strings.HasPrefix(absFilePath, absUploadDir) {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    // Set content disposition for download
    if r.URL.Query().Get("download") == "true" {
        w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(filePath)))
    }

    // Log download
    info, _ := os.Stat(filePath)
    if info != nil {
        logOperation("DOWNLOAD", requestPath, info.Size())
    }

    err := withRetry(func() error {
        f, err := os.Open(filePath)
        if err != nil {
            return err
        }
        defer f.Close()

        info, err := f.Stat()
        if err != nil {
            return err
        }

        http.ServeContent(w, r, filepath.Base(filePath), info.ModTime(), f)
        return nil
    })

    if err != nil {
        log.Printf("Error serving file: %v", err)
        http.Error(w, "Failed to serve file", http.StatusInternalServerError)
        return
    }
}

func handleQR(w http.ResponseWriter, r *http.Request) {
    ip := getLocalIP()
    url := fmt.Sprintf("http://%s:8080", ip)
    qr, _ := qrcode.Encode(url, qrcode.Medium, 256)
    w.Header().Set("Content-Type", "image/png")
    w.Write(qr)
}

func handleWebDAV(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("DAV", "1, 2")
    w.Header().Set("MS-Author-Via", "DAV")
    http.StripPrefix("/dav/", http.FileServer(http.Dir(uploadDir))).ServeHTTP(w, r)
}

func handleCreateFolder(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    parentPath := r.FormValue("path")
    if parentPath == "" {
        parentPath = "."
    }
    
    folderName := r.FormValue("name")
    if folderName == "" {
        http.Error(w, "Folder name is required", http.StatusBadRequest)
        return
    }

    // Clean and validate path
    parentPath = filepath.Clean(parentPath)
    parentPath = strings.TrimPrefix(parentPath, "/")
    fullPath := filepath.Join(uploadDir, parentPath, folderName)
    
    absUploadDir, _ := filepath.Abs(uploadDir)
    absFullPath, _ := filepath.Abs(fullPath)
    if !strings.HasPrefix(absFullPath, absUploadDir) {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    if err := os.MkdirAll(fullPath, 0755); err != nil {
        log.Printf("Error creating folder: %v", err)
        http.Error(w, "Error creating folder", http.StatusInternalServerError)
        return
    }

    http.Redirect(w, r, "/?path="+url.QueryEscape(parentPath), http.StatusSeeOther)
}

func handleMove(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    src := filepath.Clean(filepath.Join(uploadDir, r.FormValue("src")))
    dst := filepath.Clean(filepath.Join(uploadDir, r.FormValue("dst")))

    // Handle moves to root directory
    if r.FormValue("dst") == "." {
        dst = filepath.Join(uploadDir, filepath.Base(src))
    }
    
    // Clean and validate paths
    absUploadDir, _ := filepath.Abs(uploadDir)
    absSrc, _ := filepath.Abs(src)
    absDst, _ := filepath.Abs(dst)
    
    if !strings.HasPrefix(absSrc, absUploadDir) || !strings.HasPrefix(absDst, absUploadDir) {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    // Check if source exists and is not a directory
    srcInfo, err := os.Stat(src)
    if err != nil {
        log.Printf("Source file not found: %v", err)
        http.Error(w, "Source file not found", http.StatusNotFound)
        return
    }
    if srcInfo.IsDir() {
        http.Error(w, "Cannot move directories", http.StatusBadRequest)
        return
    }

    // If destination is a directory, put the file inside it
    dstInfo, err := os.Stat(dst)
    if err == nil && dstInfo.IsDir() {
        dst = filepath.Join(dst, filepath.Base(src))
    }

    // Create parent directories if needed
    if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
        log.Printf("Error creating destination directory: %v", err)
        http.Error(w, "Error creating directory", http.StatusInternalServerError)
        return
    }

    if err := os.Rename(src, dst); err != nil {
        log.Printf("Error moving file: %v", err)
        http.Error(w, "Error moving file", http.StatusInternalServerError)
        return
    }

    w.WriteHeader(http.StatusOK)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    path := filepath.Clean(r.FormValue("path"))
    fullPath := filepath.Join(uploadDir, path)

    // Validate path
    absUploadDir, _ := filepath.Abs(uploadDir)
    absPath, _ := filepath.Abs(fullPath)
    if !strings.HasPrefix(absPath, absUploadDir) {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    info, err := os.Stat(fullPath)
    if err != nil {
        http.Error(w, "File not found", http.StatusNotFound)
        return
    }

    err = os.RemoveAll(fullPath)
    if err != nil {
        log.Printf("Error deleting: %v", err)
        http.Error(w, "Error deleting item", http.StatusInternalServerError)
        return
    }

    logOperation("DELETE", path, info.Size())
    w.WriteHeader(http.StatusOK)
}

// Modify handleChunkUpload for better performance
func handleChunkUpload(w http.ResponseWriter, r *http.Request) {
    select {
    case uploadSemaphore <- struct{}{}:
        defer func() { <-uploadSemaphore }()
    default:
        // If we can't get a slot, return 429 Too Many Requests
        w.WriteHeader(http.StatusTooManyRequests)
        return
    }

    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    // Parse the multipart form data with larger buffer
    if err := r.ParseMultipartForm(64 << 20); err != nil {
        log.Printf("Error parsing form: %v", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    // Get chunk info from form
    uploadID := r.FormValue("uploadId")
    fileName := r.FormValue("fileName")
    currentPath := r.FormValue("currentPath")
    chunkNumber, _ := strconv.Atoi(r.FormValue("chunkNumber"))
    totalChunks, _ := strconv.Atoi(r.FormValue("totalChunks"))

    log.Printf("Receiving chunk %d/%d for %s", chunkNumber+1, totalChunks, fileName)

    if currentPath == "" {
        currentPath = "."
    }

    // Create target directory
    targetDir := filepath.Join(uploadDir, currentPath)
    if err := os.MkdirAll(targetDir, 0755); err != nil {
        log.Printf("Error creating directory %s: %v", targetDir, err)
        http.Error(w, "Failed to create directory", http.StatusInternalServerError)
        return
    }

    targetPath := filepath.Join(targetDir, fileName)
    
    // Validate target path
    absUploadDir, _ := filepath.Abs(uploadDir)
    absTargetPath, _ := filepath.Abs(targetPath)
    if !strings.HasPrefix(absTargetPath, absUploadDir) {
        log.Printf("Invalid target path: %s", targetPath)
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    // Check if file is video
    isVideo := isVideoFile(fileName)
    actualChunkSize := chunkSize
    if isVideo {
        actualChunkSize = videoChunkSize
    }

    // Use RLock for reading
    activeUploadsMutex.RLock()
    upload, exists := activeUploads[uploadID]
    activeUploadsMutex.RUnlock()

    if !exists {
        // Switch to write lock for modification
        activeUploadsMutex.Lock()
        // Check again in case another goroutine created it
        upload, exists = activeUploads[uploadID]
        if !exists {
            tempFile, err := os.OpenFile(
                filepath.Join(tempDir, fmt.Sprintf("%s-%s", uploadID, fileName)),
                os.O_CREATE|os.O_RDWR|os.O_SYNC,
                0644,
            )
            if err != nil {
                activeUploadsMutex.Unlock()
                log.Printf("Error creating temp file: %v", err)
                http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
                return
            }

            if totalChunks > 0 {
                tempFile.Truncate(int64(totalChunks * actualChunkSize))
            }

            baseUpload := &ActiveUpload{
                TargetPath:  targetPath,
                TotalChunks: totalChunks,
                Received:    make(map[int]bool),
                TempFile:    tempFile,
            }

            if isVideo {
                upload = &VideoUpload{
                    ActiveUpload: baseUpload,
                    ChunkPool:   &sync.Pool{New: func() interface{} { return make([]byte, videoChunkSize) }},
                    Workers:     make(chan struct{}, maxVideoWorkers),
                }
            } else {
                upload = baseUpload
            }
            
            activeUploads[uploadID] = upload
        }
        activeUploadsMutex.Unlock()
    }

    // Get chunk data with buffered read
    chunk, _, err := r.FormFile("chunk")
    if err != nil {
        http.Error(w, "No chunk data received", http.StatusBadRequest)
        return
    }
    defer chunk.Close()

    // Process chunk using interface
    err = upload.ProcessChunk(chunk, chunkNumber)
    if err != nil {
        log.Printf("Error writing chunk: %v", err)
        http.Error(w, "Failed to write chunk", http.StatusInternalServerError)
        return
    }

    upload.GetReceived()[chunkNumber] = true

    // Check if upload is complete
    if len(upload.GetReceived()) == totalChunks {
        upload.GetTempFile().Sync()
        upload.GetTempFile().Close()
        
        // Move file with retry
        var moveErr error
        for i := 0; i < maxRetries; i++ {
            moveErr = os.Rename(upload.GetTempFile().Name(), upload.GetTargetPath())
            if moveErr == nil {
                break
            }
            time.Sleep(100 * time.Millisecond)
        }

        if moveErr != nil {
            log.Printf("Error moving file after %d retries: %v", maxRetries, moveErr)
            http.Error(w, "Failed to finalize upload", http.StatusInternalServerError)
            return
        }

        activeUploadsMutex.Lock()
        delete(activeUploads, uploadID)
        activeUploadsMutex.Unlock()
        
        if info, err := os.Stat(targetPath); err == nil {
            logOperation("UPLOAD", fileName, info.Size())
        }

        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "file":    fileName,
        })
        return
    }

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]interface{}{
        "chunksReceived": len(upload.GetReceived()),
        "totalChunks":   totalChunks,
    })
}

// Enhanced offsetWriter with buffering
type offsetWriter struct {
    file   *os.File
    offset int64
    buffer []byte
}

func (w *offsetWriter) Write(p []byte) (n int, err error) {
    return w.file.WriteAt(p, w.offset)
}

func handleBatchDelete(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var paths []string
    err := json.NewDecoder(r.Body).Decode(&paths)
    if err != nil {
        http.Error(w, "Invalid request body", http.StatusBadRequest)
        return
    }

    results := make(map[string]string)
    for _, path := range paths {
        cleanPath := filepath.Clean(path)
        fullPath := filepath.Join(uploadDir, cleanPath)
        
        // Validate path
        absUploadDir, _ := filepath.Abs(uploadDir)
        absPath, _ := filepath.Abs(fullPath)
        if !strings.HasPrefix(absPath, absUploadDir) {
            results[path] = "Invalid path"
            continue
        }

        info, err := os.Stat(fullPath)
        if err != nil {
            results[path] = fmt.Sprintf("Not found: %v", err)
            continue
        }

        var size int64
        if info.IsDir() {
            size, _ = calculateDirSize(fullPath)
        } else {
            size = info.Size()
        }

        if err := os.RemoveAll(fullPath); err != nil {
            results[path] = fmt.Sprintf("Failed to delete: %v", err)
            continue
        }

        logOperation("DELETE", cleanPath, size)
        results[path] = "success"
    }

    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(results); err != nil {
        log.Printf("Error encoding response: %v", err)
    }
}

func getMimeType(filepath string) string {
    ext := path.Ext(filepath)
    mimeType := mime.TypeByExtension(ext)
    if (mimeType == "") {
        mimeType = "application/octet-stream"
    }
    return mimeType
}

func isImage(filename string) bool {
    ext := strings.ToLower(path.Ext(filename))
    return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif"
}

func formatFileSize(size int64) string {
    const unit = 1024
    if size < unit {
        return fmt.Sprintf("%d B", size)
    }
    div, exp := int64(unit), 0
    for n := size / unit; n >= unit; n /= unit {
        div *= unit
        exp++
    }
    return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func getLocalIP() string {
    addrs, _ := net.InterfaceAddrs()
    for _, addr := range addrs {
        if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
            if ipnet.IP.To4() != nil {
                return ipnet.IP.String()
            }
        }
    }
    return "127.0.0.1"
}

// Add retry utility function
func withRetry(operation func() error) error {
    var lastErr error
    for attempt := 0; attempt < maxNetworkRetries; attempt++ {
        if err := operation(); err == nil {
            return nil
        } else {
            lastErr = err
            // Only retry on network/IO errors
            if !isRetryableError(err) {
                return err
            }
            time.Sleep(networkRetryDelay * time.Duration(attempt+1))
        }
    }
    return fmt.Errorf("failed after %d attempts: %v", maxNetworkRetries, lastErr)
}

func isRetryableError(err error) bool {
    if err == nil {
        return false
    }
    // Check for common network errors
    if netErr, ok := err.(net.Error); ok {
        return netErr.Temporary() || netErr.Timeout()
    }
    // Check for specific error strings
    errStr := err.Error()
    return strings.Contains(errStr, "connection reset") ||
           strings.Contains(errStr, "broken pipe") ||
           strings.Contains(errStr, "connection refused") ||
           strings.Contains(errStr, "no such host") ||
           strings.Contains(errStr, "timeout")
}

func cleanupTempFiles() {
    activeUploadsMutex.RLock()
    defer activeUploadsMutex.RUnlock()

    files, err := os.ReadDir(tempDir)
    if err != nil {
        log.Printf("Error reading temp directory: %v", err)
        return
    }

    for _, file := range files {
        // Skip if file is part of active upload
        isActive := false
        for _, upload := range activeUploads {
            if strings.Contains(file.Name(), filepath.Base(upload.GetTempFile().Name())) {
                isActive = true
                break
            }
        }

        if !isActive {
            if err := os.Remove(filepath.Join(tempDir, file.Name())); err != nil {
                log.Printf("Error removing temp file %s: %v", file.Name(), err)
            }
        }
    }
}

// Add this new helper function
func calculateDirSize(path string) (int64, error) {
    var size int64
    err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        if !info.IsDir() {
            size += info.Size()
        }
        return nil
    })
    return size, err
}

// Add new helper function
func isVideoFile(filename string) bool {
    ext := strings.ToLower(path.Ext(filename))
    videoExts := map[string]bool{
        ".mp4":  true,
        ".avi":  true,
        ".mkv":  true,
        ".mov":  true,
        ".wmv":  true,
        ".flv":  true,
        ".webm": true,
        ".m4v":  true,
        ".3gp":  true,
    }
    return videoExts[ext]
}
