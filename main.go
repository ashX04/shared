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
)

const uploadDir = "./uploads"

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
}

type BreadcrumbItem struct {
    Name string
    Path string
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

    viewData := ViewData{
        Files:      fileInfos,
        Path:       currentPath,
        Breadcrumb: breadcrumb,
        Parent:     filepath.Dir(currentPath),
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

    // Use 32MB max memory before writing to disk
    if err := r.ParseMultipartForm(32 << 20); err != nil {
        log.Printf("Error parsing multipart form: %v", err)
        http.Error(w, "Error processing upload", http.StatusBadRequest)
        return
    }

    files := r.MultipartForm.File["files"]
    log.Printf("Received %d files for upload", len(files))

    for i, fileHeader := range files {
        file, err := fileHeader.Open()
        if err != nil {
            log.Printf("Error opening uploaded file %s: %v", fileHeader.Filename, err)
            continue
        }

        dst, err := os.Create(filepath.Join(targetDir, fileHeader.Filename))
        if err != nil {
            log.Printf("Error creating file %s: %v", fileHeader.Filename, err)
            file.Close()
            continue
        }

        written, err := io.Copy(dst, file)
        if err != nil {
            log.Printf("Error writing file %s: %v", fileHeader.Filename, err)
        } else {
            log.Printf("Successfully uploaded file %d/%d: %s (%d bytes)", i+1, len(files), fileHeader.Filename, written)
        }

        file.Close()
        dst.Close()
    }

    log.Printf("Upload completed for %d files", len(files))
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
