package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultIndex = "https://s3.amazonaws.com/rds.nsrl.nist.gov?list-type=2&prefix=RDS/current/"

type config struct {
	Addr, DataDir, SourceURL, IndexURL string
	Refresh, HTTPTimeout               time.Duration
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
	meta       *metadata
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
	if err != nil {
		slog.Warn("invalid duration; using default", "variable", key, "value", v)
		return fallback
	}
	return d
}

func main() {
	cfg := config{Addr: env("NSRL_ADDR", ":8080"), DataDir: env("NSRL_DATA_DIR", "/data"), SourceURL: os.Getenv("NSRL_SOURCE_URL"), IndexURL: env("NSRL_INDEX_URL", defaultIndex), Refresh: durationEnv("NSRL_REFRESH_INTERVAL", 24*time.Hour), HTTPTimeout: durationEnv("NSRL_HTTP_TIMEOUT", 6*time.Hour)}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		slog.Error("create data directory", "error", err)
		os.Exit(1)
	}
	a := &app{cfg: cfg, client: &http.Client{Timeout: cfg.HTTPTimeout}}
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
	m.HandleFunc("GET /v1/status", a.status)
	m.HandleFunc("GET /v1/nsrl", a.download)
	m.HandleFunc("HEAD /v1/nsrl", a.download)
	m.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"service":"nsrl-server","download":"/v1/nsrl","status":"/v1/status"}`)
	})
	return m
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"database": a.meta, "refreshing": a.refreshing, "last_check": a.lastCheck, "last_error": a.lastError})
}

func (a *app) download(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	var m metadata
	if a.meta != nil {
		m = *a.meta
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

func (a *app) updateLoop(ctx context.Context) {
	a.refresh(ctx)
	t := time.NewTicker(a.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh(ctx)
		}
	}
}

func (a *app) refresh(ctx context.Context) {
	a.mu.Lock()
	a.refreshing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.refreshing = false; a.lastCheck = time.Now().UTC(); a.mu.Unlock() }()
	source, etag, modified, err := a.latest(ctx)
	if err != nil {
		a.setError(err)
		return
	}
	a.mu.RLock()
	current := a.meta
	unchanged := current != nil && current.Source == source && (etag == "" || current.ETag == etag)
	a.mu.RUnlock()
	if unchanged {
		a.setError(nil)
		return
	}
	if err := a.fetch(ctx, source, etag, modified); err != nil {
		a.setError(err)
		return
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

type listing struct {
	Contents []struct {
		Key, ETag    string
		LastModified time.Time
		Size         int64
	} `xml:"Contents"`
}

func (a *app) latest(ctx context.Context) (string, string, time.Time, error) {
	if a.cfg.SourceURL != "" {
		return a.cfg.SourceURL, "", time.Time{}, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.IndexURL, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("read NSRL index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, fmt.Errorf("read NSRL index: HTTP %s", resp.Status)
	}
	var list listing
	if err := xml.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", "", time.Time{}, fmt.Errorf("decode NSRL index: %w", err)
	}
	var candidates []int
	for i, o := range list.Contents {
		n := strings.ToLower(o.Key)
		if strings.HasSuffix(n, "-modern.zip") && !strings.Contains(n, "minimal") {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return "", "", time.Time{}, errors.New("NSRL index contains no modern database archive")
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := list.Contents[candidates[i]], list.Contents[candidates[j]]
		if a.LastModified.Equal(b.LastModified) {
			return a.Key > b.Key
		}
		return a.LastModified.After(b.LastModified)
	})
	o := list.Contents[candidates[0]]
	base, err := url.Parse(a.cfg.IndexURL)
	if err != nil {
		return "", "", time.Time{}, err
	}
	base.RawQuery = ""
	base.Path = strings.TrimRight(base.Path, "/") + "/" + o.Key
	return base.String(), strings.Trim(o.ETag, `"`), o.LastModified, nil
}

func (a *app) fetch(ctx context.Context, source, etag string, modified time.Time) error {
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
	storedName := digest[:16] + "-" + name
	final := filepath.Join(a.cfg.DataDir, storedName)
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	m := &metadata{Source: source, Filename: storedName, ArchiveName: name, ETag: etag, SHA256: digest, Size: size, LastModified: modified, DownloadedAt: time.Now().UTC()}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "metadata.json.tmp"), b, 0o640); err != nil {
		_ = os.Remove(final)
		return err
	}
	if err := os.Rename(filepath.Join(a.cfg.DataDir, "metadata.json.tmp"), filepath.Join(a.cfg.DataDir, "metadata.json")); err != nil {
		_ = os.Remove(final)
		return err
	}
	a.mu.Lock()
	old := a.meta
	a.meta = m
	a.mu.Unlock()
	if old != nil && old.Filename != name {
		_ = os.Remove(filepath.Join(a.cfg.DataDir, old.Filename))
	}
	slog.Info("NSRL database updated", "filename", name, "bytes", strconv.FormatInt(size, 10), "sha256", m.SHA256)
	return nil
}

func (a *app) loadMetadata() {
	b, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "metadata.json"))
	if err != nil {
		return
	}
	var m metadata
	if json.Unmarshal(b, &m) == nil {
		if _, err := os.Stat(filepath.Join(a.cfg.DataDir, m.Filename)); err == nil {
			a.meta = &m
		}
	}
}
