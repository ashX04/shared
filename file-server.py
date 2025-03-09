import os
import http.server
import socketserver
from urllib.parse import unquote
import zeroconf
import threading

PORT = 8000
UPLOAD_DIR = os.path.join(os.getcwd(), 'uploads')
USERNAME = 'admin'
PASSWORD = 'password'

# Ensure upload directory exists
if not os.path.exists(UPLOAD_DIR):
    os.makedirs(UPLOAD_DIR)

class FileServerHandler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        # Check authentication
        if not self.authenticate():
            self.send_auth_request()
            return

        # List files for download
        if self.path == '/':
            self.list_files()
        else:
            super().do_GET()

    def list_files(self):
        files = os.listdir(UPLOAD_DIR)
        self.send_response(200)
        self.send_header('Content-type', 'text/html')
        self.end_headers()

        # Simple HTML page for listing and uploading files
        self.wfile.write(b'<html><body><h2>File Server</h2>')
        self.wfile.write(b'<form action="/upload" method="post" enctype="multipart/form-data">')
        self.wfile.write(b'Upload Files: <input type="file" name="file" multiple>')
        self.wfile.write(b'<input type="submit" value="Upload">')
        self.wfile.write(b'</form><hr>')

        # List files
        self.wfile.write(b'<h3>Files:</h3><ul>')
        for file in files:
            file_path = f'/uploads/{file}'
            self.wfile.write(f'<li><a href="{file_path}" download>{file}</a></li>'.encode())
        self.wfile.write(b'</ul></body></html>')

    def do_POST(self):
        # Check authentication
        if not self.authenticate():
            self.send_auth_request()
            return

        # Handle multiple file uploads
        if self.path == '/upload':
            content_length = int(self.headers['Content-Length'])
            boundary = self.headers['Content-Type'].split('boundary=')[-1]
            body = self.rfile.read(content_length)

            # Split files by boundary
            parts = body.split(b'--' + boundary.encode())
            for part in parts:
                if b'filename="' in part:
                    header, file_data = part.split(b'\r\n\r\n', 1)
                    file_data = file_data.rsplit(b'\r\n', 1)[0]
                    filename = header.split(b'filename="')[1].split(b'"')[0].decode()

                    # Save each uploaded file
                    with open(os.path.join(UPLOAD_DIR, filename), 'wb') as f:
                        f.write(file_data)

            # Redirect to the file list
            self.send_response(303)
            self.send_header('Location', '/')
            self.end_headers()

    def authenticate(self):
        auth_header = self.headers.get('Authorization')
        if not auth_header:
            return False

        auth_type, credentials = auth_header.split(' ', 1)
        if auth_type.lower() != 'basic':
            return False

        import base64
        decoded_credentials = base64.b64decode(credentials).decode()
        username, password = decoded_credentials.split(':', 1)

        return username == USERNAME and password == PASSWORD

    def send_auth_request(self):
        self.send_response(401)
        self.send_header('WWW-Authenticate', 'Basic realm="File Server"')
        self.send_header('Content-type', 'text/html')
        self.end_headers()
        self.wfile.write(b'Authentication required')

# Broadcast using Bonjour
class BonjourService:
    def __init__(self):
        self.zeroconf = zeroconf.Zeroconf()
        self.service_info = zeroconf.ServiceInfo(
            "_http._tcp.local.",
            "FileServer._http._tcp.local.",
            port=PORT,
            properties={}
        )
        self.zeroconf.register_service(self.service_info)

    def close(self):
        self.zeroconf.unregister_service(self.service_info)
        self.zeroconf.close()

# Start server
bonjour_service = BonjourService()
with socketserver.TCPServer(("", PORT), FileServerHandler) as httpd:
    print(f"Serving at port {PORT}")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        bonjour_service.close()
