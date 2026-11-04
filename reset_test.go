package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteBranchExists(t *testing.T) {
	r := newRunner("agent-linear")
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		exists  bool
		wantErr bool
	}{
		{"exists", "abc123\trefs/heads/linear-app-1-22222222", nil, true, false},
		{"missing", "", nil, false, false},
		{"unreachable", "", errors.New("network down"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.command = func(dir string, args ...string) (string, error) { return tc.out, tc.err }
			exists, err := r.remoteBranchExists("repo", "linear-app-1-22222222")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && exists != tc.exists {
				t.Fatalf("exists = %v, want %v", exists, tc.exists)
			}
		})
	}
}

func TestResetClearsClaimAndMovesToReady(t *testing.T) {
	root, r, _, api := gapRunner(t, "no-code-blocked")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(root, ".runs")
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "blocked" {
		t.Fatalf("expected a blocked claim before reset, got %q %v", status, err)
	}
	if err := executeTest(r, "reset", "--issue", "APP-1"); err != nil {
		t.Fatal(err)
	}
	if status, err := claimedStatus(db, issueID); err != nil || status != "" {
		t.Fatalf("claim was not cleared: %q %v", status, err)
	}
	if exists(filepath.Join(runs, issueID)) {
		t.Fatal("run directory was not removed")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "issueUpdate(") && textAt(call.Variables, "input", "stateId") == readyID {
			return
		}
	}
	t.Fatal("reset did not move the issue to the ready state")
}

func TestResetRefusesWhenPRRecorded(t *testing.T) {
	root, r, _, _ := gapRunner(t, "blocked")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "reset", "--issue", issueID); err == nil {
		t.Fatal("reset should refuse when a pull request was recorded")
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "blocked" {
		t.Fatalf("claim should remain after a refused reset: %q %v", status, err)
	}
}
