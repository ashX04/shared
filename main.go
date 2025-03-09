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
)

const (
    uploadDir = "./uploads"
    maxWorkers = 10
    chunkSize = 1024 * 1024 * 32 // 32MB chunks
    tempDir = "./uploads/temp"   // Add temporary directory for chunks
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

var uploadQueue chan UploadTask
var activeUploads = make(map[string]*ActiveUpload)

type ActiveUpload struct {
    TargetPath  string
    TotalChunks int
    Received    map[int]bool
    TempFile    *os.File
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
    
    if !strings.HasPrefix(absFullPath, absUploadDir) {
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

    for _, fi := range fileInfos {
        if (fi.IsDir) {
            stats.TotalFolders++
        } else {
            stats.TotalFiles++
            stats.TotalSize += fi.Size
        }
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

func handleUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    currentPath := r.FormValue("path")
    if currentPath == "" {
        currentPath = "."
    }

    // Clean and validate path
    currentPath = filepath.Clean(currentPath)
    currentPath = strings.TrimPrefix(currentPath, "/")
    targetDir := filepath.Join(uploadDir, currentPath)
    
    // Ensure target directory exists
    if err := os.MkdirAll(targetDir, 0755); err != nil {
        log.Printf("Error creating upload directory: %v", err)
        http.Error(w, "Upload failed", http.StatusInternalServerError)
        return
    }

    absUploadDir, _ := filepath.Abs(uploadDir)
    absTargetDir, _ := filepath.Abs(targetDir)
    if !strings.HasPrefix(absTargetDir, absUploadDir) {
        http.Error(w, "Invalid path", http.StatusBadRequest)
        return
    }

    r.ParseMultipartForm(32 << 20)
    files := r.MultipartForm.File["files"]
    
    results := make([]uploadResult, 0, len(files))
    resultChan := make(chan error, 1)

    for _, fileHeader := range files {
        task := UploadTask{
            File:      fileHeader,
            TargetDir: targetDir,
            Result:    resultChan,
        }
        
        // Send task to upload queue
        uploadQueue <- task
        
        // Wait for result
        err := <-resultChan
        result := uploadResult{
            Filename: fileHeader.Filename,
            Success:  err == nil,
        }
        if err != nil {
            result.Error = err.Error()
            log.Printf("Error uploading %s: %v", fileHeader.Filename, err)
        }
        results = append(results, result)
    }

    // Return JSON response with upload results
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "total":   len(files),
        "success": len(files) - countErrors(results),
        "results": results,
    })
}

func countErrors(results []uploadResult) int {
    count := 0
    for _, r := range results {
        if !r.Success {
            count++
        }
    }
    return count
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

    http.ServeFile(w, r, filePath)
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

func handleChunkUpload(w http.ResponseWriter, r *http.Request) {
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

    upload, exists := activeUploads[uploadID]
    if (!exists) {
        // Create temp file with proper permissions
        tempFile, err := os.OpenFile(
            filepath.Join(tempDir, fmt.Sprintf("%s-%s", uploadID, fileName)),
            os.O_CREATE|os.O_RDWR,
            0644,
        )
        if err != nil {
            log.Printf("Error creating temp file: %v", err)
            http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
            return
        }

        upload = &ActiveUpload{
            TargetPath:  targetPath,
            TotalChunks: totalChunks,
            Received:    make(map[int]bool),
            TempFile:    tempFile,
        }
        activeUploads[uploadID] = upload
        log.Printf("Created new upload for %s", fileName)
    }

    // Get chunk data
    chunk, _, err := r.FormFile("chunk")
    if err != nil {
        log.Printf("Error getting chunk file: %v", err)
        http.Error(w, "No chunk data received", http.StatusBadRequest)
        return
    }
    defer chunk.Close()

    // Write chunk data at correct offset
    offset := int64(chunkNumber * chunkSize)
    data, err := io.ReadAll(chunk)
    if err != nil {
        log.Printf("Error reading chunk data: %v", err)
        http.Error(w, "Failed to read chunk", http.StatusInternalServerError)
        return
    }

    if _, err := upload.TempFile.WriteAt(data, offset); err != nil {
        log.Printf("Error writing chunk: %v", err)
        http.Error(w, "Failed to write chunk", http.StatusInternalServerError)
        return
    }

    upload.Received[chunkNumber] = true
    log.Printf("Successfully wrote chunk %d/%d for %s", chunkNumber+1, totalChunks, fileName)

    // Check if upload is complete
    if len(upload.Received) == totalChunks {
        upload.TempFile.Close()
        tempPath := upload.TempFile.Name()
        
        log.Printf("Upload complete, moving %s to %s", tempPath, targetPath)
        
        if err := os.Rename(tempPath, targetPath); err != nil {
            log.Printf("Error moving file: %v", err)
            http.Error(w, "Failed to finalize upload", http.StatusInternalServerError)
            return
        }

        delete(activeUploads, uploadID)
        
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
        "chunksReceived": len(upload.Received),
        "totalChunks":   totalChunks,
    })
}

func handleBatchDelete(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var paths []string
    if err := json.NewDecoder(r.Body).Decode(&paths); err != nil {
        http.Error(w, "Invalid request", http.StatusBadRequest)
        return
    }

    results := make(map[string]string)
    for _, path := range paths {
        fullPath := filepath.Join(uploadDir, path)
        
        // Validate path
        absUploadDir, _ := filepath.Abs(uploadDir)
        absPath, _ := filepath.Abs(fullPath)
        if !strings.HasPrefix(absPath, absUploadDir) {
            results[path] = "Invalid path"
            continue
        }

        info, err := os.Stat(fullPath)
        if err != nil {
            results[path] = "Not found"
            continue
        }

        if err := os.RemoveAll(fullPath); err != nil {
            results[path] = "Failed to delete"
            continue
        }

        logOperation("DELETE", path, info.Size())
        results[path] = "success"
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(results)
}

func getMimeType(filepath string) string {
    ext := path.Ext(filepath)
    mimeType := mime.TypeByExtension(ext)
    if mimeType == "" {
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
