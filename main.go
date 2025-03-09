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
)

const uploadDir = "./uploads"

type FileInfo struct {
    Name      string
    Path      string
    Size      int64
    ModTime   time.Time
    IsImage   bool
    MimeType  string
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
    files, err := filepath.Glob(filepath.Join(uploadDir, "*"))
    if err != nil {
        log.Printf("Error reading files: %v", err)
        http.Error(w, "Error reading files", http.StatusInternalServerError)
        return
    }

    var fileInfos []FileInfo
    for _, f := range files {
        info, err := os.Stat(f)
        if err != nil {
            continue
        }

        mimeType := getMimeType(f)
        fi := FileInfo{
            Name:     path.Base(f),
            Path:     f,
            Size:     info.Size(),
            ModTime:  info.ModTime(),
            IsImage:  strings.HasPrefix(mimeType, "image/"),
            MimeType: mimeType,
        }
        fileInfos = append(fileInfos, fi)
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
    if err := tmpl.Execute(w, fileInfos); err != nil {
        log.Printf("Template error: %v", err)
    }
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    r.ParseMultipartForm(32 << 20)
    files := r.MultipartForm.File["files"]

    for _, fileHeader := range files {
        file, _ := fileHeader.Open()
        defer file.Close()

        dst, _ := os.Create(filepath.Join(uploadDir, fileHeader.Filename))
        defer dst.Close()
        io.Copy(dst, file)
    }

    http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleFileServing(w http.ResponseWriter, r *http.Request) {
    fs := http.FileServer(http.Dir(uploadDir))
    http.StripPrefix("/files/", fs).ServeHTTP(w, r)
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
