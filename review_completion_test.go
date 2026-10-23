package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifiedCompletionAcceptsObservedReview(t *testing.T) {
	for _, outcome := range []string{"ready", "no-code"} {
		t.Run(outcome, func(t *testing.T) {
			root, r, _, api := gapRunner(t, outcome)
			previous := r.command
			r.command = func(dir string, args ...string) (string, error) {
				if len(args) > 2 && args[0] == "git" && args[1] == "status" && args[2] == "--porcelain" {
					api.mu.Lock()
					api.liveState = reviewID
					api.mu.Unlock()
				}
				return previous(dir, args...)
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal("verified task falsely failed", err)
			}
			folder := filepath.Join(root, ".runs", issueID)
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
				t.Fatal(status, err)
			}
			var activity map[string]any
			if err = readJSON(filepath.Join(folder, "activity.json"), &activity); err != nil || textAt(activity, "phase") != "ready" {
				t.Fatal(activity, err)
			}
			if exists(filepath.Join(folder, "failure.txt")) || exists(filepath.Join(folder, "finish-state-request.json")) {
				t.Fatal("successful observed state generated failure or redundant mutation")
			}
			comment, err := os.ReadFile(filepath.Join(folder, "finish-comment.txt"))
			if err != nil || !strings.Contains(string(comment), "status ready") || strings.Contains(string(comment), "status failed") {
				t.Fatal(string(comment), err)
			}
			var health pollHealth
			if err = readJSON(filepath.Join(root, ".runs", "poll-health.json"), &health); err != nil || health.Outcome != "success" {
				t.Fatal(health, err)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if textAt(call.Variables, "input", "stateId") == reviewID {
					t.Fatal("runner replayed an already complete state transition")
				}
			}
		})
	}
}

func TestReconcileArchivesResolvedFailures(t *testing.T) {
	root, r, _, _ := gapRunner(t, "no-code")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, ".runs", issueID)
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE runs SET status='failed' WHERE issue_id=?", issueID); err != nil {
		t.Fatal(err)
	}
	if err = recordActivity(folder, "failed"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(folder, "failure.txt"), "previous finalization error")
	mustWrite(t, filepath.Join(folder, "reconcile-error.txt"), "previous reconciliation error")
	if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
		t.Fatal(status, err)
	}
	if exists(filepath.Join(folder, "failure.txt")) || exists(filepath.Join(folder, "reconcile-error.txt")) {
		t.Fatal("stale active failures survived successful recovery")
	}
	archived, err := filepath.Glob(filepath.Join(folder, "resolved-errors-*", "failure.txt"))
	if err != nil || len(archived) != 1 {
		t.Fatal("failure evidence not archived", archived, err)
	}
	raw, err := os.ReadFile(archived[0])
	if err != nil || string(raw) != "previous finalization error" {
		t.Fatal("old failure evidence changed", err)
	}
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"phase":"failed"`) || !strings.Contains(out.String(), `"failed_runs":0`) {
		t.Fatal(out.String())
	}
}

func TestObservedReviewDoesNotBypassCompletionGates(t *testing.T) {
	for _, scenario := range []string{"missing-pr", "pending-ci", "scope", "owner", "cancelled", "request-conflict", "request-malformed"} {
		t.Run(scenario, func(t *testing.T) {
			root, r, c, api := gapRunner(t, "ready")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(root, ".runs", issueID)
			var saved runContext
			if err := readJSON(filepath.Join(folder, "context.json"), &saved); err != nil {
				t.Fatal(err)
			}
			report, err := readReport(folder)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing-pr":
				report["pr_url"] = ""
			case "pending-ci":
				mustWrite(t, filepath.Join(root, "outcome"), "ci-pending")
			case "scope":
				api.issue["description"] = "human scope change"
			case "owner":
				api.issue["assignee"] = map[string]any{"id": projectID}
			case "cancelled":
				api.liveState = readyID
			case "request-conflict":
				if err = writeJSON(filepath.Join(folder, "finish-state-request.json"), map[string]any{"id": issueID, "input": map[string]any{"stateId": readyID}}); err != nil {
					t.Fatal(err)
				}
			case "request-malformed":
				mustWrite(t, filepath.Join(folder, "finish-state-request.json"), "{broken")
			}
			before := len(api.calls)
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = r.finishRun(db, saved, report, c.GH); err == nil {
				t.Fatal("observed review bypassed completion gate", scenario)
			}
			for _, call := range api.calls[before:] {
				if strings.Contains(call.Query, "mutation") {
					t.Fatal("invalid completion wrote to Linear")
				}
			}
		})
	}
}

func TestObservedCompletionDoesNotOverwriteReopenedIssue(t *testing.T) {
	root, r, _, api := gapRunner(t, "no-code")
	original := r.command
	r.command = func(dir string, args ...string) (string, error) {
		if len(args) > 2 && args[0] == "git" && args[1] == "status" {
			api.liveState = reviewID
		}
		return original(dir, args...)
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	r.command = original
	api.liveState = progressID
	if err := executeTest(r, "reconcile", "--issue", issueID); err == nil {
		t.Fatal("human reopening was overwritten")
	}
	if api.liveState != progressID {
		t.Fatal("reopened state changed")
	}
	if exists(filepath.Join(root, ".runs", issueID, "finish-state-request.json")) {
		t.Fatal("reopened completion replayed state mutation")
	}
}

func TestVerifiedCompletionPreservesObservedDone(t *testing.T) {
	for _, outcome := range []string{"ready", "no-code"} {
		t.Run(outcome, func(t *testing.T) {
			root, r, _, api := gapRunner(t, outcome)
			original := r.command
			r.command = func(dir string, args ...string) (string, error) {
				if len(args) > 2 && args[0] == "git" && args[1] == "status" {
					api.mu.Lock()
					api.liveState = doneID
					api.mu.Unlock()
				}
				return original(dir, args...)
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal("human-completed valid task falsely failed", err)
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
			defer api.mu.Unlock()
			if api.liveState != doneID {
				t.Fatal("human completion was overwritten")
			}
			for _, call := range api.calls {
				if textAt(call.Variables, "input", "stateId") == reviewID {
					t.Fatal("runner moved completed issue back to review")
				}
			}
		})
	}
}
