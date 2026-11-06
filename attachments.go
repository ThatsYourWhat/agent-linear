package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultAttachmentLimit bounds the bytes written per run across all attachments when the
// route does not set attachment_limit_bytes.
const defaultAttachmentLimit int64 = 256 << 20

// attachmentTimeout caps a single download; the API client's short total timeout would cut
// large files off mid-stream.
const attachmentTimeout = 10 * time.Minute

// linearUploads prefixes Linear-hosted uploads, the only URLs that receive the API key.
var linearUploads = "https://uploads.linear.app/"

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// downloadAttachments copies the issue's attachments into <folder>/attachments, a sibling of
// the checkout rather than part of it, so the agent can read what a human attached to the
// issue. Linear-hosted uploads are fetched with the API key; external URLs are fetched without
// credentials when they serve a file and are listed as links when they serve an HTML page.
// Every attachment is listed with its size, sha256 and URL in attachments/MANIFEST.txt, and
// the same list is returned for the prompt so the agent can locate the files. The folder only
// exists when the issue has attachments, downloads stream to disk under a per-run byte budget,
// and no failure here fails the run.
func (r *runner) downloadAttachments(rt *route, issue map[string]any, folder string) string {
	nodes, _ := issue["attachments"].(map[string]any)
	list, _ := nodes["nodes"].([]any)
	if len(list) == 0 {
		return ""
	}
	dir := filepath.Join(folder, "attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	budget := defaultAttachmentLimit
	if rt != nil && rt.AttachmentLimitBytes > 0 {
		budget = rt.AttachmentLimitBytes
	}
	key := os.Getenv("LINEAR_API_KEY")
	used := map[string]bool{"MANIFEST.txt": true}
	var lines []string
	for _, n := range list {
		a, _ := n.(map[string]any)
		url, title, subtitle := textAt(a, "url"), textAt(a, "title"), textAt(a, "subtitle")
		label := title
		if subtitle != "" {
			label += " - " + subtitle
		}
		name := uniqueName(used, attachmentFilename(title, url))
		hosted := strings.HasPrefix(url, linearUploads)
		size, sum, err := r.fetchAttachment(url, key, hosted, filepath.Join(dir, name), budget)
		switch {
		case err == nil:
			budget -= size
			lines = append(lines, fmt.Sprintf("- attachments/%s (%d bytes, sha256 %s) - %s - %s", name, size, sum, label, url))
		case hosted:
			lines = append(lines, fmt.Sprintf("- NOT DOWNLOADED (%v) - %s - %s", err, label, url))
		default:
			lines = append(lines, fmt.Sprintf("- LINK ONLY (%v) - %s - %s", err, label, url))
		}
	}
	manifest := strings.Join(lines, "\n")
	_ = writeFile(filepath.Join(dir, "MANIFEST.txt"), []byte(manifest+"\n"))
	return "\nLocal copies of the issue's attachments (outside the repository):\n" + manifest + "\n"
}

func attachmentFilename(title, url string) string {
	name := title
	if i := strings.Index(name, " ("); i > 0 {
		name = name[:i]
	}
	if name == "" {
		name = filepath.Base(strings.SplitN(url, "?", 2)[0])
	}
	name = unsafeName.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	return name
}

func uniqueName(used map[string]bool, name string) string {
	candidate := name
	for i := 2; used[candidate]; i++ {
		ext := filepath.Ext(name)
		candidate = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
	}
	used[candidate] = true
	return candidate
}

// fetchAttachment streams url into dest within budget and returns the bytes written and their
// sha256. Hosted uploads need the API key; other hosts never see it and are left as links when
// they answer with an HTML page instead of a file. Downloads use their own deadline rather than
// the API client's total timeout.
func (r *runner) fetchAttachment(url, key string, hosted bool, dest string, budget int64) (int64, string, error) {
	if hosted && key == "" {
		return 0, "", errors.New("LINEAR_API_KEY unset")
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return 0, "", errors.New("not an http(s) URL")
	}
	parent := r.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, attachmentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if hosted {
		req.Header.Set("Authorization", key)
	}
	client := *r.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, "", fmt.Errorf("timed out after %s", attachmentTimeout)
		}
		return 0, "", errors.New("download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if !hosted && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return 0, "", errors.New("HTML page, not a file")
	}
	if resp.ContentLength > budget {
		return 0, "", fmt.Errorf("exceeds the remaining budget of %d bytes", budget)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, budget+1))
	if err != nil {
		os.Remove(dest)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, "", fmt.Errorf("timed out after %s", attachmentTimeout)
		}
		return 0, "", errors.New("download failed")
	}
	if n > budget {
		os.Remove(dest)
		return 0, "", fmt.Errorf("exceeds the remaining budget of %d bytes", budget)
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}
