package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchInstallsArchiveAndMetadata(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	database, err := zw.Create("RDS.db")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = database.Write([]byte("SQLite format 3\x00"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		_, _ = w.Write(body.Bytes())
	}))
	defer archive.Close()
	dataDir := t.TempDir()
	source := archive.URL + "/RDS-modern.zip"
	if err := fetch(context.Background(), &http.Client{Timeout: time.Second}, dataDir, "modern", source); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(dataDir, "modern-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value metadata
	if err := json.Unmarshal(contents, &value); err != nil {
		t.Fatal(err)
	}
	if value.ArchiveName != "RDS-modern.zip" || value.DatabaseFilename == "" || !current(dataDir, "modern", source) {
		t.Fatalf("unexpected metadata: %+v", value)
	}
	archiveContents, err := os.ReadFile(filepath.Join(dataDir, value.Filename))
	if err != nil || !bytes.Equal(archiveContents, body.Bytes()) {
		t.Fatalf("archive mismatch: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(dataDir, value.DatabaseFilename)); err != nil || string(contents) != "SQLite format 3\x00" {
		t.Fatalf("database = %q, %v", contents, err)
	}
}

func TestDefaultSourcesUseCurrentRelease(t *testing.T) {
	if defaultModernSourceURL != "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_modern_minimal.zip" || defaultLegacySourceURL != "https://s3.amazonaws.com/rds.nsrl.nist.gov/RDS/rds_2026.03.1/RDS_2026.03.1_legacy_minimal.zip" {
		t.Fatal("default NSRL sources changed unexpectedly")
	}
}
