package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func gapRunner(t *testing.T, outcome string) (string, *runner, *config, *linearFixture) {
	t.Helper()
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	mustWrite(t, filepath.Join(root, "outcome"), outcome)
	api := &linearFixture{issue: testIssue(), outcome: outcome}
	r := newFixtureRunner(t, root, source, api)
	c := testConfig(source)
	model := "test/model"
	c.Model = &model
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	return root, r, c, api
}

func TestInvalidIssueDoesNotStarveQueue(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	api.issue["description"] = ""
	second := testIssue()
	second["id"], second["identifier"] = userID, "APP-2"
	api.moreIssues = []map[string]any{second}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if seen, err := claimedStatus(db, userID); err != nil || seen != "ready" {
		t.Fatal("valid issue was starved", seen, err)
	}
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "" {
		t.Fatal("invalid issue was claimed", seen, err)
	}
	api.mu.Lock()
	api.issue["description"] = "Reject empty input"
	api.mu.Unlock()
	if err = executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "ready" {
		t.Fatal("corrected issue cannot run", seen, err)
	}
}

func TestStatusReadsDuringActiveWork(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, ".runs")
	if err := os.Mkdir(runs, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", issueID, "APP-1", "running", "/scratch", ""); err != nil {
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
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "APP-1") || strings.Contains(out.String(), "skipping this tick") {
		t.Fatal("active status hidden", out.String())
	}
}

func TestCompletionPreservesHumanChanges(t *testing.T) {
	for _, outcome := range []string{"cancelled", "reassigned", "edited", "retitled", "moved"} {
		t.Run(outcome, func(t *testing.T) {
			_, r, _, api := gapRunner(t, outcome)
			if err := executeTest(r, "poll"); err == nil {
				t.Fatal("changed issue accepted as ready")
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if textAt(call.Variables, "input", "stateId") == reviewID {
					t.Fatal("human changes overwritten")
				}
			}
		})
	}
}

func TestCompletionRequiresOriginalCriteriaAndCI(t *testing.T) {
	for _, outcome := range []string{"omitted-criterion", "ci-failed", "ci-pending", "ci-error", "ci-none", "ci-head-change", "ci-missing", "ci-ruleset-missing", "ci-policy-error", "ci-no-policy"} {
		t.Run(outcome, func(t *testing.T) {
			_, r, _, api := gapRunner(t, outcome)
			if outcome == "omitted-criterion" {
				api.issue["description"] = "Reject empty input\n\nReject oversized input"
			}
			if err := executeTest(r, "poll"); err == nil {
				t.Fatal("incomplete work accepted as ready")
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if textAt(call.Variables, "input", "stateId") == reviewID {
					t.Fatal("incomplete work moved to review")
				}
			}
		})
	}
}

func TestReconcileRecordedWorkWithoutRelaunch(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(root, ".runs")
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE runs SET status='failed' WHERE issue_id=?", issueID); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(runs, issueID)
	for _, name := range []string{"finish-state-request.json", "finish-state.json", "finish-comment.txt", "finish-comment.json"} {
		if err = os.Remove(filepath.Join(folder, name)); err != nil {
			t.Fatal(err)
		}
	}
	api.mu.Lock()
	api.liveState = progressID
	api.mu.Unlock()
	// Count availability calls rather than relying on a saved outcome alone.
	before, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "ready" {
		t.Fatal(seen, err)
	}
	api.mu.Lock()
	count := len(api.calls)
	api.mu.Unlock()
	if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	for _, call := range api.calls[count:] {
		if strings.Contains(call.Query, "mutation") {
			t.Error("completion mutations repeated")
		}
	}
	api.mu.Unlock()
	after, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reconciliation relaunched work", err)
	}
}

func TestFeatureDeadlinePreservesWork(t *testing.T) {
	root, r, _, _ := gapRunner(t, "silent-stall")
	r.featureTimeout = 500 * time.Millisecond
	started := time.Now()
	err := executeTest(r, "poll")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("feature deadline not enforced", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("agent was not stopped promptly")
	}
	folder := filepath.Join(root, ".runs", issueID)
	for _, name := range []string{"result.schema.json", "events.jsonl", "stderr.log", "failure.txt", "activity.json", "context.json"} {
		if !exists(filepath.Join(folder, name)) {
			t.Error("missing recovery artifact", name)
		}
	}
	if got := mustCommand(t, filepath.Join(folder, "repo"), "git", "log", "-1", "--format=%s"); got != "Handle input" {
		t.Fatal("partial commit lost", got)
	}
	if err = executeTest(r, "status"); err != nil {
		t.Fatal("worker lock retained after timeout", err)
	}
	if err = checkoutIdle(filepath.Join(folder, "repo"), "codex"); err != nil {
		t.Fatal(err)
	}
}

func TestCommandDeadlineStopsChildren(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := commandContext(ctx, root, "sh", "-c", "sleep 30 & echo $! > "+shellQuote(pidPath)+"; wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("command deadline not enforced", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	waitProcessStopped(t, pid)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	marker := filepath.Join(root, "unexpected")
	if _, err = commandContext(cancelled, root, "touch", marker); !errors.Is(err, context.Canceled) || exists(marker) {
		t.Fatal("cancelled command was started", err)
	}
}

func TestRequirementCoverageRejectsSubstitutions(t *testing.T) {
	issue := testIssue()
	issue["description"] = "Reject empty input\n\nReject oversized input"
	expected := issueRequirements(issue)
	if len(expected) != 3 || expected[2].Requirement != "Reject oversized input" {
		t.Fatal(expected)
	}
	for _, variant := range []string{"valid", "reordered", "missing", "duplicate", "unknown", "substituted"} {
		t.Run(variant, func(t *testing.T) {
			report := goodReport()
			criteria := []any{}
			for _, item := range expected {
				criteria = append(criteria, map[string]any{"id": item.ID, "requirement": item.Requirement, "status": "passed", "evidence": "test passed"})
			}
			switch variant {
			case "reordered":
				criteria[0], criteria[1] = criteria[1], criteria[0]
			case "missing":
				criteria = criteria[:2]
			case "duplicate":
				criteria[2] = criteria[1]
			case "unknown":
				criteria[2].(map[string]any)["id"] = "R99"
			case "substituted":
				criteria[2].(map[string]any)["requirement"] = "Different requirement"
			}
			report["criteria"] = criteria
			err := verifyRequirements(report, expected)
			if (err == nil) != (variant == "valid" || variant == "reordered") {
				t.Fatal(variant, err)
			}
		})
	}
}

func TestReconcileRejectsUnsafeRecovery(t *testing.T) {
	for _, scenario := range []string{"legacy", "active-worker", "active-checkout", "changed-scope", "dirty", "uncertain-progress", "uncertain-comment", "scratch-tamper"} {
		t.Run(scenario, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "ready")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			runs := filepath.Join(root, ".runs")
			folder := filepath.Join(runs, issueID)
			db, err := openState(filepath.Join(runs, "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			switch scenario {
			case "legacy":
				_, err = db.Exec("DELETE FROM run_context WHERE issue_id=?", issueID)
			case "active-worker":
				var lock *os.File
				lock, err = os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_WRONLY, 0600)
				if err == nil {
					defer lock.Close()
					err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				}
			case "active-checkout":
				child := exec.Command("sleep", "10")
				child.Dir = filepath.Join(folder, "repo")
				err = child.Start()
				if err == nil {
					defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
				}
			case "changed-scope":
				api.mu.Lock()
				api.outcome = "edited"
				api.mu.Unlock()
			case "dirty":
				mustWrite(t, filepath.Join(folder, "repo", "app.txt"), "Uncommitted recovery work")
			case "uncertain-progress":
				api.mu.Lock()
				api.liveState = progressID
				api.mu.Unlock()
				err = os.Remove(filepath.Join(folder, "finish-state.json"))
			case "uncertain-comment":
				mustWrite(t, filepath.Join(folder, "reconcile-comment.txt"), "possibly delivered")
			case "scratch-tamper":
				_, err = db.Exec("UPDATE runs SET folder=? WHERE issue_id=?", root, issueID)
			}
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			before := len(api.calls)
			api.mu.Unlock()
			if err = executeTest(r, "reconcile", "--issue", issueID); err == nil {
				t.Fatal("unsafe recovery accepted", scenario)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls[before:] {
				if strings.Contains(call.Query, "mutation") {
					t.Fatal("unsafe recovery repeated external writes")
				}
			}
		})
	}
}

func TestReconcileConfirmsAppliedMutationWithoutReplay(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(root, ".runs")
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE runs SET status='failed' WHERE issue_id=?", issueID); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(runs, issueID, "finish-state.json")); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	before := len(api.calls)
	api.mu.Unlock()
	if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "ready" {
		t.Fatal(seen, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls[before:] {
		if strings.Contains(call.Query, "issueUpdate(") {
			t.Fatal("already applied update repeated")
		}
	}
}

func TestPendingCIReconcilesAfterPassing(t *testing.T) {
	root, r, _, api := gapRunner(t, "ci-pending")
	if err := executeTest(r, "poll"); err == nil {
		t.Fatal("pending CI accepted")
	}
	mustWrite(t, filepath.Join(root, "outcome"), "ready")
	api.mu.Lock()
	api.outcome = "ready"
	api.mu.Unlock()
	before, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending CI caused a relaunch", err)
	}
}

func TestRequiredRulesetChecksPass(t *testing.T) {
	_, r, _, _ := gapRunner(t, "ci-ruleset-pass")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileDoesNotRequireInstalledModelCLI(t *testing.T) {
	root, r, c, _ := gapRunner(t, "ready")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	c.Codex = filepath.Join(root, "missing-model-cli")
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
}

func TestActivityRemainsReadableDuringUpdates(t *testing.T) {
	folder := t.TempDir()
	if err := recordActivity(folder, "starting"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 300; i++ {
			if err := recordActivity(folder, "agent"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var readErr error
	for i := 0; i < 1000; i++ {
		var activity map[string]any
		if err := readJSON(filepath.Join(folder, "activity.json"), &activity); err != nil {
			readErr = err
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal("status can observe a partial activity checkpoint", readErr)
	}
}

func TestCommandCleansUpBackgroundChildren(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	_, err := command(root, "sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $! > "+shellQuote(pidPath))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	waitProcessStopped(t, pid)
}

func waitProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(stat), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child survived process-group cleanup")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
