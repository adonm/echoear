package main

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/index.html web/style.css web/*.js
var webFiles embed.FS

func serveApp(w http.ResponseWriter, path string) bool {
	var name, mime string
	switch path {
	case "/app":
		name, mime = "index.html", "text/html; charset=utf-8"
	case "/app/style.css":
		name, mime = "style.css", "text/css; charset=utf-8"
	default:
		var ok bool
		name, ok = strings.CutPrefix(path, "/app/")
		if !ok || !strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".test.js") ||
			strings.ContainsAny(name, "/\\%") || strings.Contains(name, "..") {
			return false
		}
		mime = "text/javascript; charset=utf-8"
	}
	data, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src data: blob:; worker-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "microphone=(self)")
	sendBytes(w, 200, mime, data)
	return true
}
