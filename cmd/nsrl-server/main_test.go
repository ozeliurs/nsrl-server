package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLatestSelectsNewestModernArchive(t *testing.T) {
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "" {
			t.Errorf("index query was lost")
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<ListBucketResult><Contents><Key>RDS/current/RDS-old-modern.zip</Key><LastModified>2025-01-01T00:00:00Z</LastModified><ETag>"old"</ETag></Contents><Contents><Key>RDS/current/RDS-new-modern.zip</Key><LastModified>2026-01-01T00:00:00Z</LastModified><ETag>"new"</ETag></Contents><Contents><Key>RDS/current/RDS-minimal.zip</Key><LastModified>2027-01-01T00:00:00Z</LastModified></Contents></ListBucketResult>`))
	}))
	defer index.Close()
	a := app{cfg: config{IndexURL: index.URL + "?list-type=2"}, client: index.Client()}
	u, etag, _, err := a.latest(context.Background())
	if err != nil || !strings.HasSuffix(u, "/RDS/current/RDS-new-modern.zip") || etag != "new" {
		t.Fatalf("latest() = %q, %q, %v", u, etag, err)
	}
}

func TestFetchAndServe(t *testing.T) {
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("zip data")) }))
	defer archive.Close()
	a := app{cfg: config{DataDir: t.TempDir()}, client: archive.Client()}
	if err := a.fetch(context.Background(), archive.URL+"/RDS-modern.zip", "tag", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Installing identical content again must not delete the content-addressed
	// archive when cleaning up the previous version.
	if err := a.fetch(context.Background(), archive.URL+"/RDS-modern.zip", "new-tag", time.Now()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/nsrl", nil)
	w := httptest.NewRecorder()
	a.download(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "zip data" {
		t.Fatalf("download status/body = %d/%q", w.Code, w.Body.String())
	}
	if _, err := os.Stat(a.cfg.DataDir + "/metadata.json"); err != nil {
		t.Fatal(err)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="RDS-modern.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	a := app{cfg: config{DataDir: t.TempDir()}}
	w := httptest.NewRecorder()
	a.ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness without database = %d, want 503", w.Code)
	}

	if err := os.WriteFile(a.cfg.DataDir+"/database.zip", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.meta = &metadata{Filename: "database.zip"}
	w = httptest.NewRecorder()
	a.ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readiness with database = %d, want 200", w.Code)
	}
}
