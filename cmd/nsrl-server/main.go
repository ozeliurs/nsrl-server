package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed openapi.json
var openAPISpec []byte

//go:embed docs.html
var docsPage []byte

type config struct {
	Addr, DataDir string
}

type metadata struct {
	Source       string    `json:"source"`
	Filename     string    `json:"filename"`
	ArchiveName  string    `json:"archive_name,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	DownloadedAt time.Time `json:"downloaded_at"`
}

type app struct {
	cfg       config
	databases map[string]*metadata
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := config{Addr: env("NSRL_ADDR", ":8080"), DataDir: env("NSRL_DATA_DIR", "/data")}
	a := &app{cfg: cfg, databases: make(map[string]*metadata)}
	a.loadMetadata()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Addr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	slog.Info("NSRL server listening", "address", cfg.Addr, "data_dir", cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	m.HandleFunc("GET /readyz", a.ready)
	m.HandleFunc("GET /v1/status", a.status)
	m.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(openAPISpec)
	})
	m.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(docsPage)
	})
	for _, dataset := range []string{"modern", "legacy"} {
		m.HandleFunc("GET /v1/nsrl/"+dataset, a.download(dataset))
		m.HandleFunc("HEAD /v1/nsrl/"+dataset, a.download(dataset))
	}
	// Preserve the original endpoint as an alias for the modern data set.
	m.HandleFunc("GET /v1/nsrl", a.download("modern"))
	m.HandleFunc("HEAD /v1/nsrl", a.download("modern"))
	m.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"service":"nsrl-server","downloads":{"modern":"/v1/nsrl/modern","legacy":"/v1/nsrl/legacy"},"status":"/v1/status"}`)
	})
	return m
}

func (a *app) ready(w http.ResponseWriter, _ *http.Request) {
	files := make([]string, 0, 2)
	for _, dataset := range []string{"modern", "legacy"} {
		if m := a.databases[dataset]; m != nil {
			files = append(files, m.Filename)
		}
	}
	if len(files) != 2 {
		http.Error(w, "NSRL databases are not available yet", http.StatusServiceUnavailable)
		return
	}
	for _, filename := range files {
		if _, err := os.Stat(filepath.Join(a.cfg.DataDir, filename)); err != nil {
			http.Error(w, "NSRL database is unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ready"}`)
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"databases": a.databases})
}

func (a *app) download(dataset string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m metadata
		if a.databases[dataset] != nil {
			m = *a.databases[dataset]
		}
		if m.Filename == "" {
			http.Error(w, "NSRL database is not available yet", http.StatusServiceUnavailable)
			return
		}
		f, err := os.Open(filepath.Join(a.cfg.DataDir, m.Filename))
		if err != nil {
			http.Error(w, "NSRL database is unavailable", http.StatusServiceUnavailable)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/zip")
		downloadName := m.ArchiveName
		if downloadName == "" { // Metadata written by versions before ArchiveName was added.
			downloadName = m.Filename
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName))
		w.Header().Set("ETag", `"sha256-`+m.SHA256+`"`)
		http.ServeContent(w, r, m.Filename, m.LastModified, f)
	}
}

func (a *app) loadMetadata() {
	for _, dataset := range []string{"modern", "legacy"} {
		path := filepath.Join(a.cfg.DataDir, dataset+"-metadata.json")
		if dataset == "modern" {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				path = filepath.Join(a.cfg.DataDir, "metadata.json")
			}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m metadata
		if json.Unmarshal(b, &m) == nil {
			if _, err := os.Stat(filepath.Join(a.cfg.DataDir, m.Filename)); err == nil {
				a.databases[dataset] = &m
			}
		}
	}
}
