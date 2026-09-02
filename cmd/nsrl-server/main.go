package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultModernSourceURL = "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_modern.zip"
	defaultLegacySourceURL = "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_legacy.zip"
)

//go:embed openapi.json
var openAPISpec []byte

//go:embed docs.html
var docsPage []byte

type config struct {
	Addr, DataDir, SourceURL, LegacySourceURL string
	Refresh, Retry, HTTPTimeout               time.Duration
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
	cfg        config
	client     *http.Client
	mu         sync.RWMutex
	databases  map[string]*metadata
	refreshing bool
	lastError  string
	lastCheck  time.Time
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration; using default", "variable", key, "value", v)
		return fallback
	}
	return d
}

func main() {
	cfg := config{Addr: env("NSRL_ADDR", ":8080"), DataDir: env("NSRL_DATA_DIR", "/data"), SourceURL: env("NSRL_SOURCE_URL", defaultModernSourceURL), LegacySourceURL: env("NSRL_LEGACY_SOURCE_URL", defaultLegacySourceURL), Refresh: durationEnv("NSRL_REFRESH_INTERVAL", 24*time.Hour), Retry: durationEnv("NSRL_RETRY_INTERVAL", 5*time.Minute), HTTPTimeout: durationEnv("NSRL_HTTP_TIMEOUT", 6*time.Hour)}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		slog.Error("create data directory", "error", err)
		os.Exit(1)
	}
	a := &app{cfg: cfg, client: &http.Client{Timeout: cfg.HTTPTimeout}, databases: make(map[string]*metadata)}
	a.loadMetadata()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go a.updateLoop(ctx)
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
	a.mu.RLock()
	files := make([]string, 0, 2)
	for _, dataset := range []string{"modern", "legacy"} {
		if m := a.databases[dataset]; m != nil {
			files = append(files, m.Filename)
		}
	}
	a.mu.RUnlock()
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
	a.mu.RLock()
	defer a.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"databases": a.databases, "refreshing": a.refreshing, "last_check": a.lastCheck, "last_error": a.lastError})
}

func (a *app) download(dataset string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		var m metadata
		if a.databases[dataset] != nil {
			m = *a.databases[dataset]
		}
		a.mu.RUnlock()
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

func (a *app) updateLoop(ctx context.Context) {
	for {
		a.refresh(ctx)
		a.mu.RLock()
		failed := a.lastError != ""
		a.mu.RUnlock()
		wait := a.cfg.Refresh
		if failed {
			wait = a.cfg.Retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (a *app) refresh(ctx context.Context) {
	a.mu.Lock()
	a.refreshing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.refreshing = false; a.lastCheck = time.Now().UTC(); a.mu.Unlock() }()
	for _, dataset := range []string{"modern", "legacy"} {
		source, etag, modified, err := a.latest(ctx, dataset)
		if err != nil {
			a.setError(fmt.Errorf("%s: %w", dataset, err))
			return
		}
		a.mu.RLock()
		current := a.databases[dataset]
		unchanged := current != nil && current.Source == source && (etag == "" || current.ETag == etag)
		a.mu.RUnlock()
		if unchanged {
			continue
		}
		if err := a.fetch(ctx, dataset, source, etag, modified); err != nil {
			a.setError(fmt.Errorf("%s: %w", dataset, err))
			return
		}
	}
	a.setError(nil)
}

func (a *app) setError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastError = ""
	} else {
		a.lastError = err.Error()
		slog.Error("NSRL refresh failed", "error", err)
	}
}

func (a *app) latest(_ context.Context, dataset string) (string, string, time.Time, error) {
	sourceURL := a.cfg.SourceURL
	if dataset == "legacy" {
		sourceURL = a.cfg.LegacySourceURL
	}
	if sourceURL == "" {
		switch dataset {
		case "modern":
			sourceURL = defaultModernSourceURL
		case "legacy":
			sourceURL = defaultLegacySourceURL
		default:
			return "", "", time.Time{}, fmt.Errorf("unknown NSRL dataset %q", dataset)
		}
	}
	return sourceURL, "", time.Time{}, nil
}

func (a *app) fetch(ctx context.Context, dataset, source, etag string, modified time.Time) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("download NSRL archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download NSRL archive: HTTP %s", resp.Status)
	}
	name := filepath.Base(resp.Request.URL.Path)
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name = "nsrl-modern.zip"
	}
	tmp, err := os.CreateTemp(a.cfg.DataDir, ".nsrl-*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("write NSRL archive: %w", copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if modified.IsZero() {
		modified = time.Now().UTC()
	}
	digest := hex.EncodeToString(h.Sum(nil))
	storedName := dataset + "-" + digest[:16] + "-" + name
	final := filepath.Join(a.cfg.DataDir, storedName)
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	m := &metadata{Source: source, Filename: storedName, ArchiveName: name, ETag: etag, SHA256: digest, Size: size, LastModified: modified, DownloadedAt: time.Now().UTC()}
	b, _ := json.MarshalIndent(m, "", "  ")
	metadataPath := filepath.Join(a.cfg.DataDir, dataset+"-metadata.json")
	if err := os.WriteFile(metadataPath+".tmp", b, 0o640); err != nil {
		_ = os.Remove(final)
		return err
	}
	if err := os.Rename(metadataPath+".tmp", metadataPath); err != nil {
		_ = os.Remove(final)
		return err
	}
	a.mu.Lock()
	old := a.databases[dataset]
	a.databases[dataset] = m
	a.mu.Unlock()
	if old != nil && old.Filename != storedName {
		_ = os.Remove(filepath.Join(a.cfg.DataDir, old.Filename))
	}
	slog.Info("NSRL database updated", "dataset", dataset, "filename", name, "bytes", strconv.FormatInt(size, 10), "sha256", m.SHA256)
	return nil
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
