package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManagedResume(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root, r, c, api := gapRunner(t, "blocked")
			c.Engine = engine
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(root, ".runs", issueID)
			before, err := os.ReadFile(filepath.Join(folder, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			context, err := os.ReadFile(filepath.Join(folder, "context.json"))
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(folder, "repo", "partial.txt"), "preserve partial work")
			mustWrite(t, filepath.Join(root, "outcome"), "blocked")
			// Recovery must use the recorded engine/model even if defaults have changed.
			c.Engine = "codex"
			if engine == "codex" {
				c.Engine = "opencode"
			}
			c.Model = nil
			if err = writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if err = executeTest(r, "resume", "--issue", issueID); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(root, "resume-calls.json")) {
				t.Fatal("session was not resumed")
			}
			archives, err := filepath.Glob(filepath.Join(folder, "resumes", "attempt-*", "events.jsonl"))
			if err != nil || len(archives) != 1 {
				t.Fatal(archives, err)
			}
			archived, err := os.ReadFile(archives[0])
			if err != nil || !bytes.Equal(before, archived) {
				t.Fatal("lost original events", err)
			}
			after, err := os.ReadFile(filepath.Join(folder, "context.json"))
			if err != nil || !bytes.Equal(context, after) {
				t.Fatal("launch context changed", err)
			}
			if !exists(filepath.Join(folder, "repo", "partial.txt")) {
				t.Fatal("discarded partial work")
			}
			mustCommand(t, filepath.Join(folder, "repo"), "git", "add", "partial.txt")
			mustCommand(t, filepath.Join(folder, "repo"), "git", "commit", "-S", "-m", "Preserve partial work")
			mustWrite(t, filepath.Join(root, "outcome"), "ready")
			if err = executeTest(r, "resume", "--issue", issueID); err != nil {
				t.Fatal(err)
			}
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
				t.Fatal(status, err)
			}
			api.mu.Lock()
			state := api.liveState
			api.mu.Unlock()
			if state != reviewID {
				t.Fatal("resume did not finish verification")
			}
		})
	}
}

func TestManagedResumeRejectsUnsafeContinuation(t *testing.T) {
	for _, scenario := range []string{"other-user", "reassigned", "scope", "state", "branch", "profile", "missing-session", "missing-context", "worker", "checkout", "finish-request"} {
		t.Run(scenario, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "blocked")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(root, ".runs", issueID)
			switch scenario {
			case "other-user":
				api.viewerID = projectID
			case "reassigned":
				api.viewerID = projectID
				api.issue["assignee"] = map[string]any{"id": projectID}
			case "scope":
				api.issue["description"] = "new requirements"
			case "state":
				api.liveState = readyID
			case "branch":
				mustCommand(t, filepath.Join(folder, "repo"), "git", "checkout", "-b", "other")
			case "profile":
				mustCommand(t, filepath.Join(folder, "repo"), "git", "config", "user.email", "other@example.com")
			case "missing-session":
				mustWrite(t, filepath.Join(folder, "events.jsonl"), "partial")
			case "missing-context":
				db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("DELETE FROM run_context")
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "worker":
				f, err := os.OpenFile(filepath.Join(root, ".runs", "worker.lock"), os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			case "checkout":
				cmd := exec.Command("sleep", "10")
				cmd.Dir = filepath.Join(folder, "repo")
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			case "finish-request":
				mustWrite(t, filepath.Join(folder, "finish-state-request.json"), "{}")
			}
			before, err := os.ReadFile(filepath.Join(folder, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err = executeTest(r, "resume", "--issue", issueID); err == nil {
				t.Fatal("unsafe resume accepted")
			}
			after, err := os.ReadFile(filepath.Join(folder, "events.jsonl"))
			if err != nil || !bytes.Equal(before, after) || exists(filepath.Join(root, "resume-calls.json")) {
				t.Fatal("unauthorized continuation touched output", err)
			}
		})
	}
}

func TestFailedResumePreservesSessionAndRemovesOldReport(t *testing.T) {
	root, r, _, _ := gapRunner(t, "blocked")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "outcome"), "process-error")
	if err := executeTest(r, "resume", "--issue", issueID); err == nil {
		t.Fatal("failed process accepted")
	}
	folder := filepath.Join(root, ".runs", issueID)
	if exists(filepath.Join(folder, "result.json")) {
		t.Fatal("old report survived failed resume")
	}
	if sessionID(folder, "codex") != projectID {
		t.Fatal("lost session after failure")
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "failed" {
		t.Fatal(status, err)
	}
	mustWrite(t, filepath.Join(root, "outcome"), "ready")
	if err := executeTest(r, "resume", "--issue", issueID); err != nil {
		t.Fatal("could not resume after interruption", err)
	}
}

func TestPollHealthAndAttention(t *testing.T) {
	root, r, _, _ := gapRunner(t, "blocked")
	runs := filepath.Join(root, ".runs")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	var health pollHealth
	if err := readJSON(filepath.Join(runs, "poll-health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Outcome != "success" || health.LastSuccess.IsZero() {
		t.Fatal(health)
	}
	previous := health.LastSuccess
	t.Setenv("LINEAR_API_KEY", "")
	if err := executeTest(r, "poll"); err == nil {
		t.Fatal("missing credentials accepted")
	}
	if err := readJSON(filepath.Join(runs, "poll-health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Outcome != "failed" || health.LastSuccess != previous || !strings.Contains(health.LastError, "Linear queue read") {
		t.Fatal(health)
	}
	var out bytes.Buffer
	r.out = &out
	if err := executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"blocked_runs":1`) || !strings.Contains(out.String(), `"poll_failed"`) || !strings.Contains(out.String(), `"polling_stale":false`) {
		t.Fatal(out.String())
	}
	health.LastStarted = time.Now().Add(-time.Hour)
	health.Outcome = "running"
	if err := writeJSON(filepath.Join(runs, "poll-health.json"), health); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(runs, "poll-attempt.json"), map[string]any{"last_attempt": time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"polling_stale":true`) || !strings.Contains(out.String(), `"poll_interrupted"`) {
		t.Fatal(out.String())
	}
}

func TestPollHealthLockSkipAndDryRun(t *testing.T) {
	root, r, _, _ := gapRunner(t, "ready")
	runs := filepath.Join(root, ".runs")
	if err := executeTest(r, "poll", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(runs, "poll-health.json")) || exists(filepath.Join(runs, "poll-attempt.json")) {
		t.Fatal("dry run pretended to poll")
	}
	health := pollHealth{LastSuccess: time.Now().UTC(), Outcome: "success"}
	if err := writeJSON(filepath.Join(runs, "poll-health.json"), health); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	var after pollHealth
	if err = readJSON(filepath.Join(runs, "poll-health.json"), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(health, after) || !exists(filepath.Join(runs, "poll-attempt.json")) {
		t.Fatal("lock skip overwrote last completed poll")
	}
}

func TestPollHealthBeforeStateAndSecretSanitization(t *testing.T) {
	root, r, _, _ := gapRunner(t, "ready")
	mustWrite(t, filepath.Join(root, "config.json"), "invalid secret-value")
	if err := executeTest(r, "poll"); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	healthPath := filepath.Join(root, ".runs", "poll-health.json")
	raw, err := os.ReadFile(healthPath)
	if err != nil || bytes.Contains(raw, []byte("secret-value")) {
		t.Fatal(string(raw), err)
	}
	var health pollHealth
	if err = json.Unmarshal(raw, &health); err != nil || health.Outcome != "failed" {
		t.Fatal(health, err)
	}
	if err = os.Remove(filepath.Join(root, ".runs", "state.sqlite3")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "poll_failed") {
		t.Fatal("health hidden without database", out.String())
	}
	if _, err = os.Stat(filepath.Join(root, ".runs", "state.sqlite3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status created state", err)
	}
}

func TestResumeFailureBoundaries(t *testing.T) {
	for _, outcome := range []string{"resume-report-error", "silent-stall", "limit-during", "ci-pending"} {
		t.Run(outcome, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "blocked")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(root, "outcome"), outcome)
			if outcome == "silent-stall" {
				r.featureTimeout = 30 * time.Millisecond
			}
			err := executeTest(r, "resume", "--issue", issueID)
			if err == nil {
				t.Fatal("failed continuation accepted")
			}
			folder := filepath.Join(root, ".runs", issueID)
			if exists(filepath.Join(folder, "result.json")) != (outcome == "ci-pending") {
				t.Fatal("incorrect recovery report retention")
			}
			api.mu.Lock()
			state := api.liveState
			api.mu.Unlock()
			if state != progressID {
				t.Fatal("failed continuation moved Linear")
			}
			db, e := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			expected := "failed"
			if outcome == "limit-during" {
				expected = "blocked"
			}
			if status, e := claimedStatus(db, issueID); e != nil || status != expected {
				t.Fatal(status, e)
			}
			if outcome == "limit-during" {
				c, e := r.loadConfig(filepath.Join(root, "config.json"))
				if e != nil {
					t.Fatal(e)
				}
				if limit, e := activeCooldown(db, c); e != nil || limit == nil {
					t.Fatal("resume did not retain provider cooldown", e)
				}
			}
			if outcome == "ci-pending" {
				mustWrite(t, filepath.Join(root, "outcome"), "ready")
				if e = executeTest(r, "reconcile", "--issue", issueID); e != nil {
					t.Fatal("fresh resumed result could not be reconciled", e)
				}
			}
		})
	}
}

func TestPollHealthDeferredAndEmptyQueue(t *testing.T) {
	for _, outcome := range []string{"limit-preflight", "empty"} {
		t.Run(outcome, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "ready")
			if outcome == "empty" {
				api.issue = nil
			} else {
				mustWrite(t, filepath.Join(root, "outcome"), "limit-before")
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			var health pollHealth
			if err := readJSON(filepath.Join(root, ".runs", "poll-health.json"), &health); err != nil {
				t.Fatal(err)
			}
			if outcome == "empty" {
				if health.Outcome != "success" || health.LastSuccess.IsZero() {
					t.Fatal(health)
				}
			} else if health.Outcome != "deferred" || !health.LastSuccess.IsZero() {
				t.Fatal(health)
			}
		})
	}
}

func TestStatusBeforeFirstPollIsReadOnly(t *testing.T) {
	root := t.TempDir()
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err := executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "never_polled") || !strings.Contains(out.String(), "polling_stale") || exists(filepath.Join(root, ".runs")) {
		t.Fatal(out.String())
	}
}

func TestHealthDoesNotReportActivePollInterruptedBeforeDatabaseExists(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, ".runs")
	if err := os.Mkdir(runs, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err = beginPoll(runs); err != nil {
		t.Fatal(err)
	}
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "poll_interrupted") {
		t.Fatal("active poll falsely reported interrupted", out.String())
	}
}
