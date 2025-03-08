package main

import (
    "fmt"
    "html/template"
    "io"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "github.com/skip2/go-qrcode"
    "golang.org/x/net/webdav"
)

const (
    uploadDir = "./uploads"
    username  = "admin"    // You can change these credentials
    password  = "admin123" // You can change these credentials
)

type WebDAVHandler struct {
    *webdav.Handler
}

func (h *WebDAVHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Basic auth check
    user, pass, ok := r.BasicAuth()
    if !ok || user != username || pass != password {
        w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK")
    w.Header().Set("Access-Control-Allow-Headers", "Depth, Authorization")
    w.Header().Set("DAV", "1,2")
    
    if r.Method == "OPTIONS" {
        return
    }
    
    h.Handler.ServeHTTP(w, r)
}

func main() {
    // Create uploads directory if it doesn't exist
    os.MkdirAll(uploadDir, 0755)

    // Get local IP address
    ip := getLocalIP()
    serverAddr := fmt.Sprintf("%s:8080", ip)
    qrCodeURL := fmt.Sprintf("http://%s", serverAddr)
    
    // Generate QR code
    qrcode.WriteFile(qrCodeURL, qrcode.Medium, 256, "static/qr.png")

    // WebDAV handler
    webdavHandler := &WebDAVHandler{
        &webdav.Handler{
            FileSystem: webdav.Dir(uploadDir),
            LockSystem: webdav.NewMemLS(),
        },
    }

    // Routes
    http.HandleFunc("/", handleHome)
    http.HandleFunc("/upload", handleUpload)
    http.Handle("/webdav/", http.StripPrefix("/webdav", webdavHandler))
    http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(uploadDir))))
    http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

    fmt.Printf("Server running at http://%s\n", serverAddr)
    fmt.Printf("WebDAV URL: http://%s/webdav\n", serverAddr)
    http.ListenAndServe(serverAddr, nil)
}

func handleHome(w http.ResponseWriter, r *http.Request) {
    files, _ := filepath.Glob(filepath.Join(uploadDir, "*"))
    fileNames := make([]string, 0)
    for _, f := range files {
        fileNames = append(fileNames, filepath.Base(f))
    }
    
    tmpl, _ := template.ParseFiles("templates/index.html")
    tmpl.Execute(w, fileNames)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Redirect(w, r, "/", http.StatusSeeOther)
        return
    }

    r.ParseMultipartForm(32 << 20) // 32MB max memory
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

func getLocalIP() string {
    addrs, _ := net.InterfaceAddrs()
    for _, addr := range addrs {
        if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
            if ipnet.IP.To4() != nil {
                return ipnet.IP.String()
            }
        }
    }
    return "localhost"
}
