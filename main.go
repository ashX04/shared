package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/grandcat/zeroconf"
)

const (
	PORT       = 8000
	UPLOAD_DIR = "uploads"
	USERNAME   = "admin"
	PASSWORD   = "password"
)

func main() {
	if _, err := os.Stat(UPLOAD_DIR); os.IsNotExist(err) {
		os.Mkdir(UPLOAD_DIR, 0755)
	}

	// Start mDNS service for Bonjour
	server, err := zeroconf.Register("FileServer", "_http._tcp", "local.", PORT, nil, nil)
	if err != nil {
		panic(err)
	}
	defer server.Shutdown()

	http.HandleFunc("/", handleListFiles)
	http.HandleFunc("/upload", handleFileUpload)
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(UPLOAD_DIR))))

	fmt.Printf("Serving at http://localhost:%d\n", PORT)
	http.ListenAndServe(fmt.Sprintf(":%d", PORT), nil)
}

func authenticate(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || user != USERNAME || pass != PASSWORD {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"File Server\"")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Unauthorized"))
		return false
	}
	return true
}

func handleListFiles(w http.ResponseWriter, r *http.Request) {
	if !authenticate(w, r) {
		return
	}

	files, err := os.ReadDir(UPLOAD_DIR)
	if err != nil {
		http.Error(w, "Failed to list files", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("<html><body><h2>File Server</h2>"))
	w.Write([]byte("<form action='/upload' method='post' enctype='multipart/form-data'>"))
	w.Write([]byte("Upload Files: <input type='file' name='file' multiple>"))
	w.Write([]byte("<input type='submit' value='Upload'>"))
	w.Write([]byte("</form><hr><h3>Files:</h3><ul>"))

	for _, file := range files {
		fileURL := fmt.Sprintf("/files/%s", file.Name())
		w.Write([]byte(fmt.Sprintf("<li><a href='%s' download>%s</a></li>", fileURL, file.Name())))
	}

	w.Write([]byte("</ul></body></html>"))
}

func handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if !authenticate(w, r) {
		return
	}

	r.ParseMultipartForm(10 << 20)
	files := r.MultipartForm.File["file"]

	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			http.Error(w, "Failed to read file", http.StatusInternalServerError)
			return
		}
		defer file.Close()

		filePath := filepath.Join(UPLOAD_DIR, fileHeader.Filename)
		outFile, err := os.Create(filePath)
		if err != nil {
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}
		defer outFile.Close()

		io.Copy(outFile, file)
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}
