package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// loadEnv reads a .env file and sets variables into the process environment.
// Lines starting with # and blank lines are ignored.
// Already-set environment variables are not overwritten.
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Strip optional surrounding quotes
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		// Don't overwrite variables already set in the real environment
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// rpcProxy forwards a JSON-RPC POST body to targetURL and streams the response
// back to the client. The API key stays on the server — the browser only ever
// calls /rpc/eth or /rpc/matic.
func rpcProxy(targetURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16)) // 64 KB max
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// gzipResponseWriter wraps http.ResponseWriter to compress response bodies.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(statusCode int) {
	// Remove Content-Length set by http.ServeFile; it reflects uncompressed size.
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(statusCode)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gz.Write(b)
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip gzip for HEAD requests and Range requests (byte ranges are for uncompressed content).
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Method == http.MethodHead || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}

func headersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Security headers (also improve Google's trust signals).
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Cache-Control: versioned static assets can be cached longer;
		// HTML should always revalidate so crawlers see fresh content.
		switch {
		case strings.HasSuffix(r.URL.Path, ".css") || strings.HasSuffix(r.URL.Path, ".js"):
			h.Set("Cache-Control", "public, max-age=3600")
		case r.URL.Path == "/robots.txt" || r.URL.Path == "/sitemap.xml":
			h.Set("Cache-Control", "public, max-age=86400")
		default:
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func main() {

	loadEnv(".env")

	mux := http.NewServeMux()

	// Pretty URLs for legal pages
	mux.HandleFunc("/privacy-policy", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./public/privacy-policy.html")
	})
	mux.HandleFunc("/terms-of-service", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./public/terms-of-service.html")
	})

	// RPC proxy routes — only registered when an API URL is configured.
	// The browser calls /rpc/eth or /rpc/matic; the key never leaves the server.
	if url := os.Getenv("RPC_ETH_URL"); url != "" {
		mux.HandleFunc("/rpc/eth", rpcProxy(url))
		log.Println("Ethereum RPC proxy active")
	}
	if url := os.Getenv("RPC_MATIC_URL"); url != "" {
		mux.HandleFunc("/rpc/matic", rpcProxy(url))
		log.Println("Polygon RPC proxy active")
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html at root
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "./public/index.html")
			return
		}
		// Whitelist: only serve files inside ./public
		absPublic, _ := filepath.Abs("./public")
		absTarget, err := filepath.Abs("./public" + r.URL.Path)
		if err != nil || !strings.HasPrefix(absTarget, absPublic) {
			serve404(w, r)
			return
		}
		if fi, err := os.Stat(absTarget); err == nil && !fi.IsDir() {
			http.ServeFile(w, r, absTarget)
			return
		}
		serve404(w, r)
	})

	// Serve all other static assets (css, js, etc.)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./public"))))
	mux.HandleFunc("/styles.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./public/styles.css")
	})

	// Crawler essentials
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./public/robots.txt")
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		http.ServeFile(w, r, "./public/sitemap.xml")
	})

	handler := gzipMiddleware(headersMiddleware(mux))

	log.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}

func serve404(w http.ResponseWriter, r *http.Request) {
	body, err := os.ReadFile("./public/404.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(body)
}
