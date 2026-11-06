package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadAttachments(t *testing.T) {
	var hostedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/hosted/receipt.json":
			hostedAuth = req.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		case "/external/evidence.tar.gz":
			if req.Header.Get("Authorization") != "" {
				t.Error("API key sent to an external host")
			}
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(make([]byte, 1024))
		case "/external/pull/7":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html>pull request</html>"))
		case "/hosted/huge.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(make([]byte, 4096))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	// Hosted and external URLs are told apart by prefix; point the hosted prefix at the test server.
	previous := linearUploads
	linearUploads = srv.URL + "/hosted/"
	defer func() { linearUploads = previous }()
	t.Setenv("LINEAR_API_KEY", "lin_api_test")

	folder := t.TempDir()
	r := &runner{client: srv.Client()}
	issue := map[string]any{"attachments": map[string]any{"nodes": []any{
		map[string]any{"title": "receipt.json", "subtitle": "signed receipt", "url": srv.URL + "/hosted/receipt.json"},
		map[string]any{"title": "evidence.tar.gz", "subtitle": "copied from object storage", "url": srv.URL + "/external/evidence.tar.gz"},
		map[string]any{"title": "fix: thing (APP-1)", "subtitle": "", "url": srv.URL + "/external/pull/7"},
		map[string]any{"title": "huge.bin", "subtitle": "", "url": srv.URL + "/hosted/huge.bin"},
		map[string]any{"title": "missing.pdf", "subtitle": "", "url": srv.URL + "/hosted/missing.pdf"},
	}}}
	note := r.downloadAttachments(&route{AttachmentLimitBytes: 2048}, issue, folder)

	dir := filepath.Join(folder, "attachments")
	if hostedAuth != "lin_api_test" {
		t.Fatalf("hosted upload fetched without the API key: %q", hostedAuth)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "receipt.json")); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("receipt.json = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "evidence.tar.gz")); err != nil || info.Size() != 1024 {
		t.Fatalf("external file not downloaded: %v", err)
	}
	for _, name := range []string{"fix_thing", "huge.bin", "missing.pdf"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s should not have been written", name)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "MANIFEST.txt"))
	if err != nil {
		t.Fatal(err)
	}
	receiptSum := sha256.Sum256([]byte(`{"ok":true}`))
	evidenceSum := sha256.Sum256(make([]byte, 1024))
	for _, want := range []string{
		"- attachments/receipt.json (11 bytes, sha256 " + hex.EncodeToString(receiptSum[:]) + ") - receipt.json - signed receipt - " + srv.URL + "/hosted/receipt.json",
		"- attachments/evidence.tar.gz (1024 bytes, sha256 " + hex.EncodeToString(evidenceSum[:]) + ") - evidence.tar.gz - copied from object storage - ",
		"- LINK ONLY (HTML page, not a file) - fix: thing (APP-1) - " + srv.URL + "/external/pull/7",
		"- NOT DOWNLOADED (exceeds the remaining budget of 1013 bytes) - huge.bin - ",
		"- NOT DOWNLOADED (HTTP 404) - missing.pdf - ",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	if !strings.HasPrefix(note, "\nLocal copies of the issue's attachments (outside the repository):\n") || !strings.Contains(note, string(manifest)) {
		t.Fatalf("prompt note should locate the files:\n%s", note)
	}
}

func TestDownloadAttachmentsCreatesNothingWithoutAttachments(t *testing.T) {
	r := &runner{client: http.DefaultClient}
	for _, issue := range []map[string]any{
		{},
		{"attachments": map[string]any{"nodes": []any{}}},
	} {
		folder := t.TempDir()
		if note := r.downloadAttachments(&route{}, issue, folder); note != "" {
			t.Fatalf("unexpected prompt note %q", note)
		}
		if _, err := os.Stat(filepath.Join(folder, "attachments")); !os.IsNotExist(err) {
			t.Fatalf("attachments folder created with nothing to download: %v", err)
		}
	}
}

func TestDownloadAttachmentsKeepsDuplicateNamesApart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(req.URL.Path))
	}))
	defer srv.Close()
	folder := t.TempDir()
	r := &runner{client: srv.Client()}
	issue := map[string]any{"attachments": map[string]any{"nodes": []any{
		map[string]any{"title": "notes.txt", "url": srv.URL + "/a/notes.txt"},
		map[string]any{"title": "notes.txt", "url": srv.URL + "/b/notes.txt"},
	}}}
	r.downloadAttachments(nil, issue, folder)
	for name, want := range map[string]string{"notes.txt": "/a/notes.txt", "notes-2.txt": "/b/notes.txt"} {
		if data, err := os.ReadFile(filepath.Join(folder, "attachments", name)); err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", name, data, err)
		}
	}
}

func TestDownloadAttachmentsOutlastsTheAPIClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			w.Write(make([]byte, 512))
			flusher.Flush()
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer srv.Close()
	folder := t.TempDir()
	// The API client's total timeout is far shorter than this download; the download must not inherit it.
	r := &runner{client: &http.Client{Timeout: 40 * time.Millisecond, Transport: srv.Client().Transport}}
	issue := map[string]any{"attachments": map[string]any{"nodes": []any{
		map[string]any{"title": "slow.bin", "url": srv.URL + "/slow.bin"},
	}}}
	r.downloadAttachments(nil, issue, folder)
	if info, err := os.Stat(filepath.Join(folder, "attachments", "slow.bin")); err != nil || info.Size() != 1536 {
		t.Fatalf("slow download should complete under its own deadline: %v", err)
	}
}

func TestAttachmentFilename(t *testing.T) {
	url := "https://uploads.linear.app/abc/upload.pdf?signature=***"
	for title, want := range map[string]string{
		"receipt.json": "receipt.json",
		"batch-001-checkpoint.tar.gz (sha256 7f4c43b7)": "batch-001-checkpoint.tar.gz",
		"fix: thing (APP-1)":                            "fix_thing",
		"../../etc/passwd":                              ".._.._etc_passwd",
		"":                                              "upload.pdf",
	} {
		if got := attachmentFilename(title, url); got != want {
			t.Errorf("attachmentFilename(%q) = %q, want %q", title, got, want)
		}
	}
}
