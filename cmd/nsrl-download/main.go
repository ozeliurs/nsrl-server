package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
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
	"strings"
	"syscall"
	"time"
)

const (
	defaultModernSourceURL = "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_modern_minimal.zip"
	defaultLegacySourceURL = "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_legacy_minimal.zip"
)

type metadata struct {
	Source           string    `json:"source"`
	Filename         string    `json:"filename"`
	DatabaseFilename string    `json:"database_filename,omitempty"`
	ArchiveName      string    `json:"archive_name,omitempty"`
	ETag             string    `json:"etag,omitempty"`
	SHA256           string    `json:"sha256"`
	Size             int64     `json:"size"`
	LastModified     time.Time `json:"last_modified"`
	DownloadedAt     time.Time `json:"downloaded_at"`
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	dataDir := env("NSRL_DATA_DIR", "/data")
	timeout, err := time.ParseDuration(env("NSRL_HTTP_TIMEOUT", "6h"))
	if err != nil || timeout <= 0 {
		slog.Error("invalid NSRL_HTTP_TIMEOUT", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		slog.Error("create data directory", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: timeout}
	for _, item := range []struct{ dataset, source string }{
		{"modern", env("NSRL_SOURCE_URL", defaultModernSourceURL)},
		{"legacy", env("NSRL_LEGACY_SOURCE_URL", defaultLegacySourceURL)},
	} {
		if current(dataDir, item.dataset, item.source) {
			slog.Info("NSRL database already present", "dataset", item.dataset)
			continue
		}
		if err := fetch(ctx, client, dataDir, item.dataset, item.source); err != nil {
			slog.Error("download NSRL database", "dataset", item.dataset, "error", err)
			os.Exit(1)
		}
	}
}

func current(dataDir, dataset, source string) bool {
	contents, err := os.ReadFile(filepath.Join(dataDir, dataset+"-metadata.json"))
	if err != nil {
		return false
	}
	var value metadata
	if json.Unmarshal(contents, &value) != nil || value.Source != source {
		return false
	}
	if _, err = os.Stat(filepath.Join(dataDir, value.Filename)); err != nil || value.DatabaseFilename == "" {
		return false
	}
	_, err = os.Stat(filepath.Join(dataDir, value.DatabaseFilename))
	return err == nil
}

func fetch(ctx context.Context, client *http.Client, dataDir, dataset, source string) error {
	var previous metadata
	if contents, err := os.ReadFile(filepath.Join(dataDir, dataset+"-metadata.json")); err == nil {
		_ = json.Unmarshal(contents, &previous)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request archive: HTTP %s", resp.Status)
	}
	name := filepath.Base(resp.Request.URL.Path)
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name = "nsrl-" + dataset + ".zip"
	}
	tmp, err := os.CreateTemp(dataDir, ".nsrl-*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("write archive: %w", copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	storedName := dataset + "-" + digest[:16] + "-" + name
	if err := os.Rename(tmpName, filepath.Join(dataDir, storedName)); err != nil {
		return err
	}
	databaseFilename, err := extractDatabase(filepath.Join(dataDir, storedName), dataDir, dataset, digest)
	if err != nil {
		_ = os.Remove(filepath.Join(dataDir, storedName))
		return err
	}
	now := time.Now().UTC()
	modified := now
	if header := resp.Header.Get("Last-Modified"); header != "" {
		if parsed, err := http.ParseTime(header); err == nil {
			modified = parsed
		}
	}
	value := metadata{Source: source, Filename: storedName, DatabaseFilename: databaseFilename, ArchiveName: name, ETag: resp.Header.Get("ETag"), SHA256: digest, Size: size, LastModified: modified, DownloadedAt: now}
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(dataDir, dataset+"-metadata.json")
	if err := os.WriteFile(metadataPath+".tmp", contents, 0o640); err != nil {
		_ = os.Remove(filepath.Join(dataDir, storedName))
		return err
	}
	if err := os.Rename(metadataPath+".tmp", metadataPath); err != nil {
		_ = os.Remove(filepath.Join(dataDir, storedName))
		return err
	}
	if previous.Filename != "" && previous.Filename != storedName {
		_ = os.Remove(filepath.Join(dataDir, previous.Filename))
	}
	if previous.DatabaseFilename != "" && previous.DatabaseFilename != databaseFilename {
		_ = os.Remove(filepath.Join(dataDir, previous.DatabaseFilename))
	}
	slog.Info("NSRL database installed", "dataset", dataset, "filename", name, "bytes", size, "sha256", digest)
	return nil
}

func extractDatabase(archivePath, dataDir, dataset, digest string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer r.Close()
	for _, entry := range r.File {
		ext := strings.ToLower(filepath.Ext(entry.Name))
		if entry.FileInfo().IsDir() || (ext != ".db" && ext != ".sqlite" && ext != ".sqlite3") {
			continue
		}
		source, err := entry.Open()
		if err != nil {
			return "", fmt.Errorf("open database in archive: %w", err)
		}
		name := dataset + "-" + digest[:16] + ext
		tmp, err := os.CreateTemp(dataDir, ".nsrl-db-*.part")
		if err != nil {
			source.Close()
			return "", err
		}
		_, copyErr := io.Copy(tmp, source)
		closeErr := tmp.Close()
		source.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(tmp.Name())
			if copyErr != nil {
				return "", fmt.Errorf("extract database: %w", copyErr)
			}
			return "", closeErr
		}
		if err := os.Rename(tmp.Name(), filepath.Join(dataDir, name)); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		return name, nil
	}
	return "", errors.New("archive does not contain a SQLite database")
}
