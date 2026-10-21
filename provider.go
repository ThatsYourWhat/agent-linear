package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const providerCooldown = 15 * time.Minute
const availabilityPrompt = "Provider availability check. Reply with exactly READY. Do not call tools, inspect files, or perform issue work."

type providerLimit struct {
	RetryAt         time.Time
	recordingFailed bool
}

func (e *providerLimit) Error() string {
	return "provider rate/usage limit; pickup paused until " + e.RetryAt.UTC().Format(time.RFC3339)
}
func providerKey(c *config) string {
	engine, binary := c.agent()
	model := ""
	if c.Model != nil {
		model = *c.Model
	}
	b, _ := json.Marshal([]string{engine, binary, model})
	return string(b)
}
func activeCooldown(db *sql.DB, c *config) (*providerLimit, error) {
	var retryAt int64
	err := db.QueryRow("SELECT retry_at FROM provider_cooldowns WHERE provider=?", providerKey(c)).Scan(&retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if retryAt <= time.Now().Unix() {
		return nil, nil
	}
	return &providerLimit{RetryAt: time.Unix(retryAt, 0)}, nil
}
func saveCooldown(db *sql.DB, c *config, limit *providerLimit) error {
	_, err := db.Exec(`INSERT INTO provider_cooldowns(provider,retry_at) VALUES (?,?)
 ON CONFLICT(provider) DO UPDATE SET retry_at=MAX(retry_at,excluded.retry_at)`, providerKey(c), limit.RetryAt.Unix())
	return err
}
func isClaimed(status string) bool { return status != "" && status != "deferred" }

// Only provider error/retry records qualify; assistant text and tool output do not.
func limitEvent(line []byte) bool {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return false
	}
	kind := textAt(event, "type")
	switch kind {
	case "error", "turn.failed", "session.error", "retry":
		return limitDetails(event)
	case "session.status":
		return textAt(event, "status", "type") == "retry" && limitDetails(event["status"])
	default:
		return false
	}
}
func limitDetails(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "statusCode" || key == "status_code" || key == "http_status" || key == "status" {
				if n, ok := item.(float64); ok && n == 429 {
					return true
				}
				if text, ok := item.(string); ok && text == "429" {
					return true
				}
			}
			if limitDetails(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if limitDetails(item) {
				return true
			}
		}
	case string:
		return limitMessage(v)
	}
	return false
}

var http429 = regexp.MustCompile(`(?i)(?:http(?: status)?|status(?:code|_code)?|error|response|request rejected)[\s:=\"]+429\b`)

func limitMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"rate_limit_exceeded", "rate_limit_error", "ratelimiterror", "rate limit exceeded", "rate limit reached", "too many requests", "toomanyrequestsexception", "usage_limit_reached", "usage_limit_exceeded", "usage limit reached", "usage limit has been reached", "hit your usage limit", "exceeded your usage limit", "insufficient_quota", "quota_exceeded", "quota exceeded", "exceeded your current quota", "credit balance is too low", "insufficient credits", "credits exhausted", "insufficient_balance", "freeusagelimiterror"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return http429.MatchString(message)
}

type limitStream struct {
	file    io.Writer
	pending []byte
	stderr  bool
	signal  func()
	mu      sync.Mutex
}

func (w *limitStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	w.pending = append(w.pending, p...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			break
		}
		w.check(w.pending[:end])
		w.pending = w.pending[end+1:]
	}
	// Keep diagnostic buffering bounded; a complete JSON event may be large.
	w.check(w.pending)
	if w.stderr && len(w.pending) > 1024*1024 {
		w.pending = nil
	}
	return n, nil
}
func (w *limitStream) check(line []byte) {
	if limitEvent(line) || w.stderr && limitMessage(string(line)) {
		w.signal()
	}
}
func (w *limitStream) finish() { w.mu.Lock(); defer w.mu.Unlock(); w.check(w.pending); w.pending = nil }

func runModel(ctx context.Context, cmd *exec.Cmd, out, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	detected := make(chan struct{}, 1)
	signal := func() {
		select {
		case detected <- struct{}{}:
		default:
		}
		cancel()
	}
	stdoutStream := &limitStream{file: out, signal: signal}
	stderrStream := &limitStream{file: stderr, stderr: true, signal: signal}
	// Kill the CLI and its children when it reports a limit, including internal retries.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Stdout = stdoutStream
	cmd.Stderr = stderrStream
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	<-stopped
	stdoutStream.finish()
	stderrStream.finish()
	select {
	case <-detected:
		return &providerLimit{RetryAt: time.Now().Add(providerCooldown)}
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (r *runner) checkProvider(c *config, repo, runs string, db *sql.DB, lock *os.File) (string, error) {
	limit, err := activeCooldown(db, c)
	if err != nil {
		return "", err
	}
	if limit != nil {
		return "", limit
	}
	parent := filepath.Join(runs, "preflight")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	folder, err := os.MkdirTemp(parent, "check-")
	if err != nil {
		return "", err
	}
	engine, binary := c.agent()
	response := filepath.Join(folder, "response.txt")
	var args []string
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "LINEAR_API_KEY=") {
			env = append(env, v)
		}
	}
	if engine == "codex" {
		args = []string{"-a", "never", "exec", "--sandbox", "read-only", "--ephemeral", "--cd", repo, "--json", "--output-last-message", response,
			"-c", "features.shell_tool=false", "-c", `web_search="disabled"`, "-c", "mcp_servers={}"}
	} else {
		settings := map[string]any{"share": "disabled", "autoupdate": false, "agent": map[string]any{"linear-issue": map[string]any{"description": "Check model availability without tools", "mode": "primary", "permission": "deny"}}}
		b, e := json.Marshal(settings)
		if e != nil {
			return folder, e
		}
		env = setEnv(env, "OPENCODE_CONFIG_CONTENT", string(b))
		args = []string{"run", "--format", "json", "--dir", repo, "--agent", "linear-issue"}
	}
	if c.Model != nil {
		args = append(args, "--model", *c.Model)
	}
	if engine == "codex" {
		args = append(args, "-")
	}
	out, err := os.OpenFile(filepath.Join(folder, "events.jsonl"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return folder, err
	}
	defer out.Close()
	stderr, err := os.OpenFile(filepath.Join(folder, "stderr.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return folder, err
	}
	defer stderr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), r.preflightTimeout)
	defer cancel()
	cmd := exec.Command(binary, args...)
	cmd.Dir = repo
	cmd.Env = env
	cmd.Stdin = strings.NewReader(availabilityPrompt)
	if lock != nil {
		cmd.ExtraFiles = []*os.File{lock}
	}
	err = runModel(ctx, cmd, out, stderr)
	if err == nil && engine == "opencode" {
		err = opencodeReport(folder)
		response = filepath.Join(folder, "result.json")
	}
	if err != nil {
		return folder, fmt.Errorf("%s availability check failed: %w; inspect %s", engine, err, folder)
	}
	b, err := os.ReadFile(response)
	if err != nil {
		return folder, err
	}
	if strings.TrimSpace(string(b)) != "READY" {
		return folder, fmt.Errorf("%s availability check did not confirm READY; inspect %s", engine, folder)
	}
	return folder, nil
}
