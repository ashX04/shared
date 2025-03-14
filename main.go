package main

import (
    "fmt"
    "html/template"
    "io"
    "log"
    "mime"
    "mime/multipart"  // Add this line
    "net"
    "net/http"
    "os"
    "path/filepath"
    "strings"
    "time"
    "github.com/skip2/go-qrcode"
    "path"
    "net/url"
    "bufio"
    //"bytes"
    "runtime"
    "sync"
    "sync/atomic"
    "fileserver/moodboard"
    "encoding/json"
)

const uploadDir = "./uploads"

const (
    copyBufferSize = 4 * 1024 * 1024 // 4MB buffer size
)

var (
    bufferPool = sync.Pool{
        New: func() interface{} {
            return make([]byte, copyBufferSize)
        },
    }
    
    // Performance metrics
    uploadedBytes int64
    uploadCount   int64
    uploadErrors  int64
    avgSpeed     float64
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
    Stats      FolderStats    // Add this line
}

type BreadcrumbItem struct {
    Name string
    Path string
}

type FolderStats struct {
    FileCount int64
    TotalSize int64
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

    os.MkdirAll(moodboard.MoodBoardDir, 0755)
    os.MkdirAll(moodboard.ThumbnailDir, 0755)
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
    
    // Add new moodboard handlers
    http.HandleFunc("/moodboard/create", handleMoodboardCreate)
    http.HandleFunc("/moodboard/list", handleMoodboardList)
    http.HandleFunc("/moodboard/view/", handleMoodboardView)
    http.HandleFunc("/moodboard/update", handleMoodboardUpdate)
    http.HandleFunc("/moodboard/delete", handleMoodboardDelete)
    
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

    // Get folder statistics
    stats := getFolderStats(fullPath)

    viewData := ViewData{
        Files:      fileInfos,
        Path:       currentPath,
        Breadcrumb: breadcrumb,
        Parent:     filepath.Dir(currentPath),
        Stats:      stats,          // Add this line
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
            case strings.Contains(mimeType, "word"):
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

    // Enable CORS for XHR requests
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

    if r.Method == "OPTIONS" {
        w.WriteHeader(http.StatusOK)
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
    
    log.Printf("Upload started to directory: %s", targetDir)

    // Ensure target directory exists
    if err := os.MkdirAll(targetDir, 0755); err != nil {
        log.Printf("Error creating upload directory: %v", err)
        http.Error(w, "Upload failed", http.StatusInternalServerError)
        return
    }

    if err := r.ParseMultipartForm(32 << 20); err != nil {
        log.Printf("Error parsing multipart form: %v", err)
        http.Error(w, "Error processing upload", http.StatusBadRequest)
        return
    }

    files := r.MultipartForm.File["files"]
    log.Printf("Received %d files for upload", len(files))

    // Create channels for work distribution and results
    jobs := make(chan *multipart.FileHeader, len(files))
    results := make(chan error, len(files))
    
    // Create worker pool (50 workers)
    for i := 0; i < runtime.NumCPU(); i++ {
        go func(workerID int) {
            for fileHeader := range jobs {
                start := time.Now()
                
                // Get a buffer from the pool
                buf := bufferPool.Get().([]byte)
                defer bufferPool.Put(buf)

                // Create a pipe for streaming
                pr, pw := io.Pipe()
                
                // Start the writer goroutine
                go func() {
                    defer pw.Close()
                    file, err := fileHeader.Open()
                    if err != nil {
                        log.Printf("Worker %d: Error opening file: %v", workerID, err)
                        atomic.AddInt64(&uploadErrors, 1)
                        pw.CloseWithError(err)
                        return
                    }
                    defer file.Close()

                    // Use buffered writer for better performance
                    bw := bufio.NewWriterSize(pw, copyBufferSize)
                    _, err = io.CopyBuffer(bw, file, buf)
                    if err != nil {
                        atomic.AddInt64(&uploadErrors, 1)
                        pw.CloseWithError(err)
                        return
                    }
                    bw.Flush()
                }()

                // Create the destination file
                dst, err := os.Create(filepath.Join(targetDir, fileHeader.Filename))
                if err != nil {
                    log.Printf("Worker %d: Error creating file: %v", workerID, err)
                    atomic.AddInt64(&uploadErrors, 1)
                    results <- err
                    continue
                }

                // Use buffered writer for the destination
                bw := bufio.NewWriterSize(dst, copyBufferSize)
                written, err := io.CopyBuffer(bw, pr, buf)
                
                if err != nil {
                    log.Printf("Worker %d: Error writing file: %v", workerID, err)
                    atomic.AddInt64(&uploadErrors, 1)
                    results <- err
                } else {
                    bw.Flush()
                    duration := time.Since(start)
                    recordMetrics(written, duration)
                    log.Printf("Worker %d: Successfully uploaded %s (%s) in %v, speed: %.2f MB/s",
                        workerID,
                        fileHeader.Filename,
                        formatFileSize(written),
                        duration,
                        float64(written)/(1024*1024)/duration.Seconds(),
                    )
                    results <- nil
                }
                dst.Close()
            }
        }(i)
    }

    // Send jobs to workers
    go func() {
        for _, fileHeader := range files {
            jobs <- fileHeader
        }
        close(jobs)
    }()

    // Collect results
    var uploadErrors []error
    for i := 0; i < len(files); i++ {
        if err := <-results; err != nil {
            uploadErrors = append(uploadErrors, err)
        }
    }

    if len(uploadErrors) > 0 {
        log.Printf("Upload completed with %d errors", len(uploadErrors))
        http.Error(w, fmt.Sprintf("Upload completed with %d errors", len(uploadErrors)), http.StatusInternalServerError)
        return
    }

    log.Printf("Upload completed successfully for %d files", len(files))
    log.Printf("Upload session completed. %s", getMetrics())
    w.WriteHeader(http.StatusOK)
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

func getFolderStats(path string) FolderStats {
    var stats FolderStats
    filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
        if err != nil {
            return filepath.SkipDir
        }
        if !info.IsDir() {
            stats.FileCount++
            stats.TotalSize += info.Size()
        }
        return nil
    })
    return stats
}

func recordMetrics(size int64, duration time.Duration) {
    atomic.AddInt64(&uploadedBytes, size)
    atomic.AddInt64(&uploadCount, 1)
    
    // Calculate speed in MB/s
    speed := float64(size) / (1024 * 1024) / duration.Seconds()
    // Update moving average
    avgSpeed = (avgSpeed*0.9 + speed*0.1)
}

func getMetrics() string {
    return fmt.Sprintf(
        "Total: %s, Count: %d, Errors: %d, Avg Speed: %.2f MB/s",
        formatFileSize(atomic.LoadInt64(&uploadedBytes)),
        atomic.LoadInt64(&uploadCount),
        atomic.LoadInt64(&uploadErrors),
        avgSpeed,
    )
}

// Add these new handler functions
func handleMoodboardCreate(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    name := r.FormValue("name")
    description := r.FormValue("description")
    
    board, err := moodboard.NewMoodBoard(name, description)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    if err := board.Save(); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(board)
}

func handleMoodboardList(w http.ResponseWriter, r *http.Request) {
    boards, err := moodboard.ListMoodBoards()
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(boards)
}

func handleMoodboardView(w http.ResponseWriter, r *http.Request) {
    id := strings.TrimPrefix(r.URL.Path, "/moodboard/view/")
    
    if strings.HasSuffix(r.URL.Path, ".json") {
        // Return JSON data for API requests
        board, err := moodboard.GetMoodBoard(id)
        if err != nil {
            http.Error(w, err.Error(), http.StatusNotFound)
            return
        }
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(board)
        return
    }

    // Serve the moodboard template for browser requests
    tmpl := template.Must(template.ParseFiles("templates/moodboard.html"))
    if err := tmpl.Execute(w, nil); err != nil {
        log.Printf("Template error: %v", err)
        http.Error(w, "Error rendering template", http.StatusInternalServerError)
    }
}

func handleMoodboardUpdate(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    id := r.FormValue("id")
    board, err := moodboard.GetMoodBoard(id)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }

    // Update moodboard items
    if err := json.NewDecoder(r.Body).Decode(&board.Items); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    if err := board.Save(); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    w.WriteHeader(http.StatusOK)
}

func handleMoodboardDelete(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodDelete {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    id := r.FormValue("id")
    if err := moodboard.DeleteMoodBoard(id); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    w.WriteHeader(http.StatusOK)
}
