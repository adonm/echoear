package main

import (
	"embed"
	"net/http"
)

//go:embed web/index.html web/style.css web/*.js
var webFiles embed.FS

var appAssets = map[string][2]string{
	"/app":           {"web/index.html", "text/html; charset=utf-8"},
	"/app/style.css": {"web/style.css", "text/css; charset=utf-8"},
}

func init() {
	for _, name := range []string{"client", "api", "audio", "chat", "devices", "diag", "dom", "library", "live", "mic", "photos", "state", "transcript"} {
		name += ".js"
		appAssets["/app/"+name] = [2]string{"web/" + name, "text/javascript; charset=utf-8"}
	}
}

func serveApp(w http.ResponseWriter, path string) bool {
	file, ok := appAssets[path]
	if !ok {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src data: blob:; worker-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "microphone=(self)")
	data, err := webFiles.ReadFile(file[0])
	if err != nil {
		http.Error(w, "App unavailable", 500)
	} else {
		sendBytes(w, 200, file[1], data)
	}
	return true
}
