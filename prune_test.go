package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type pruningFixture struct {
	mu           sync.Mutex
	r            *runner
	db           *sql.DB
	folder       string
	current      map[string]any
	apiError     bool
	deleteError  bool
	deletions    [][]string
	requestCount int
	output       bytes.Buffer
}

func newPruningFixture(t *testing.T, engine, state string) *pruningFixture {
	t.Helper()
	t.Setenv("LINEAR_API_KEY", "test-secret")
	root := t.TempDir()
	f := &pruningFixture{folder: filepath.Join(root, ".runs", issueID), current: testIssue()}
	f.current["state"] = map[string]any{"id": doneID, "name": state, "type": "completed"}
	checkout := filepath.Join(f.folder, "repo")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	c := testConfig(root)
	// Current engine configuration must not replace the recorded CLI.
	c.Engine = "codex"
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	if _, err = f.db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", issueID, "APP-1", "ready", f.folder, prURL); err != nil {
		t.Fatal(err)
	}
	saved := runContext{ProjectID: projectID, Route: c.Projects[projectID], Scratch: f.folder,
		Checkout: checkout, Engine: engine, Executable: "/recorded/" + engine,
		Issue: testIssue(), Owner: userID, ProgressState: progressID, ReviewState: reviewID,
		Requirements: issueRequirements(testIssue())}
	if err = saveRunContext(f.db, issueID, saved); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"type": "thread.started", "thread_id": projectID}
	if engine == "opencode" {
		event = map[string]any{"type": "step_start", "sessionID": "ses_test123"}
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.folder, "events.jsonl"), string(raw)+"\n")
	mustWrite(t, filepath.Join(f.folder, "worklog.md"), "Retain evidence\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requestCount++
		var call apiCall
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(call.Query, "mutation") || !strings.Contains(call.Query, "state { id name type }") || textAt(call.Variables, "id") != issueID {
			t.Error("pruning requires a current status read without Linear mutations", call)
		}
		if f.apiError {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": f.current}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	f.r = newRunner(filepath.Join(root, "agent-linear"))
	f.r.client, f.r.endpoint = server.Client(), server.URL
	f.r.out, f.r.errOut = &f.output, &f.output
	f.r.command = func(dir string, args ...string) (string, error) {
		if reflect.DeepEqual(args, []string{"git", "remote", "get-url", "origin"}) {
			return "https://github.com/acme/widget.git", nil
		}
		if dir != checkout {
			t.Error("deletion must use the recorded checkout", dir)
		}
		f.deletions = append(f.deletions, args)
		if f.deleteError {
			return "", errors.New("CLI deletion failed")
		}
		return "", nil
	}
	return f
}

func TestPruneSessionsExactLiveStatuses(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		for _, state := range []string{"Done", "Canceled", "Duplicate", "In Review", "In Progress", "Todo", "Cancelled", "done", "Archived", ""} {
			t.Run(engine+"/"+state, func(t *testing.T) {
				f := newPruningFixture(t, engine, state)
				eligible := state == "Done" || state == "Canceled" || state == "Duplicate"
				if err := executeTest(f.r, "prune-sessions"); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(f.folder, "session-pruned.json")
				if len(f.deletions) != 0 || exists(marker) || strings.Contains(f.output.String(), "would delete") != eligible {
					t.Fatal("incorrect preview", f.output.String(), f.deletions)
				}
				if err := executeTest(f.r, "prune-sessions", "--apply"); err != nil {
					t.Fatal(err)
				}
				if !eligible {
					if len(f.deletions) != 0 || exists(marker) {
						t.Fatal("deleted a non-allowlisted session")
					}
					return
				}
				expected := []string{"/recorded/codex", "delete", "--force", projectID}
				if engine == "opencode" {
					expected = []string{"/recorded/opencode", "session", "delete", "ses_test123"}
				}
				f.mu.Lock()
				requests := f.requestCount
				f.mu.Unlock()
				if len(f.deletions) != 1 || !reflect.DeepEqual(f.deletions[0], expected) || !exists(marker) || requests != 2 {
					t.Fatal("incorrect deletion", f.deletions, requests)
				}
				if err := executeTest(f.r, "prune-sessions", "--apply"); err != nil || len(f.deletions) != 1 {
					t.Fatal("repeated a completed deletion", err, f.deletions)
				}
				if status, err := claimedStatus(f.db, issueID); err != nil || status != "ready" || !exists(filepath.Join(f.folder, "worklog.md")) || !exists(filepath.Join(f.folder, "repo")) || !exists(filepath.Join(f.folder, "events.jsonl")) {
					t.Fatal("pruning changed claims or discarded evidence", status, err)
				}
			})
		}
	}
}

func TestPruneSessionsRetainsUnsafeOrUnverifiedSessions(t *testing.T) {
	for _, variant := range []string{"api-error", "missing-issue", "changed-project", "changed-team", "invalid-context", "missing-session", "running", "deferred", "waiting", "cli-error", "marker-error", "worker-active"} {
		t.Run(variant, func(t *testing.T) {
			f := newPruningFixture(t, "codex", "Done")
			wantError := true
			switch variant {
			case "api-error":
				f.apiError = true
			case "missing-issue":
				f.current = nil
			case "changed-project":
				f.current["project"] = map[string]any{"id": teamID}
			case "changed-team":
				f.current["team"] = map[string]any{"id": projectID}
			case "invalid-context":
				if _, err := f.db.Exec("DELETE FROM run_context"); err != nil {
					t.Fatal(err)
				}
			case "missing-session":
				mustWrite(t, filepath.Join(f.folder, "events.jsonl"), `{"type":"thread.started","thread_id":"--last"}`)
				wantError = false
			case "running", "deferred":
				if _, err := f.db.Exec("UPDATE runs SET status=?", variant); err != nil {
					t.Fatal(err)
				}
				wantError = false
			case "waiting":
				if _, err := f.db.Exec("INSERT INTO provider_recoveries VALUES (?,0,'waiting','Cooldown')", issueID); err != nil {
					t.Fatal(err)
				}
				wantError = false
			case "cli-error":
				f.deleteError = true
			case "marker-error":
				mustWrite(t, filepath.Join(f.folder, "session-pruned.json"), "broken")
			case "worker-active":
				lock, err := os.OpenFile(filepath.Join(f.r.root, ".runs", "worker.lock"), os.O_CREATE|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			err := executeTest(f.r, "prune-sessions", "--apply")
			if (err != nil) != wantError {
				t.Fatal("unexpected pruning result", err)
			}
			if variant != "cli-error" && len(f.deletions) != 0 {
				t.Fatal("unsafe deletion", f.deletions)
			}
			if variant != "marker-error" && exists(filepath.Join(f.folder, "session-pruned.json")) {
				t.Fatal("marked an unsuccessful deletion as complete")
			}
		})
	}
}

func TestPruneSessionsRechecksStatusAfterPreview(t *testing.T) {
	f := newPruningFixture(t, "codex", "Done")
	if err := executeTest(f.r, "prune-sessions"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.current["state"] = map[string]any{"name": "In Review", "type": "completed"}
	f.mu.Unlock()
	if err := executeTest(f.r, "prune-sessions", "--apply"); err != nil || len(f.deletions) != 0 {
		t.Fatal("deleted using the preview's stale status", err, f.deletions)
	}
	f.mu.Lock()
	f.current["state"] = map[string]any{"name": "Canceled", "type": "canceled"}
	f.mu.Unlock()
	if _, err := f.db.Exec("UPDATE runs SET status='failed'"); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(f.r, "prune-sessions", "--apply"); err != nil || len(f.deletions) != 1 {
		t.Fatal("current terminal status must govern a stopped run", err, f.deletions)
	}
}
