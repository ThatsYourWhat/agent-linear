package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// needsOperatorID is the workflow state a route may name for runs that need a person.
const needsOperatorID = "99999999-9999-4999-8999-999999999999"

func stateTransitions(api *linearFixture) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var states []string
	for _, call := range api.calls {
		if strings.Contains(call.Query, "issueUpdate(") {
			states = append(states, textAt(call.Variables, "input", "stateId"))
		}
	}
	return states
}

func TestNeedsOperatorStateAfterBlockedRun(t *testing.T) {
	root, r, c, api := gapRunner(t, "blocked")
	c.Projects[projectID].NeedsOperatorStateID = needsOperatorID
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	states := stateTransitions(api)
	if len(states) < 2 || states[0] != progressID || states[len(states)-1] != needsOperatorID {
		t.Fatalf("blocked run should move the issue to the needs-operator state last, got %v", states)
	}
	if api.liveState != needsOperatorID {
		t.Fatalf("live state = %q", api.liveState)
	}
	if _, err := os.Stat(filepath.Join(root, ".runs", issueID, "needs-operator-state.json")); err != nil {
		t.Fatalf("state change evidence missing: %v", err)
	}
}

func TestNeedsOperatorStateAfterNoCodeReady(t *testing.T) {
	root, r, c, api := gapRunner(t, "no-code")
	c.Projects[projectID].NeedsOperatorStateID = needsOperatorID
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	api.issue["description"] = "Do not modify product files. Reject empty input"
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	states := stateTransitions(api)
	if len(states) == 0 || states[len(states)-1] != needsOperatorID {
		t.Fatalf("ready run without a PR should end in the needs-operator state, got %v", states)
	}
}

func TestNeedsOperatorStateIsOptional(t *testing.T) {
	_, r, _, api := gapRunner(t, "blocked")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	for _, state := range stateTransitions(api) {
		if state == needsOperatorID {
			t.Fatal("state moved without needs_operator_state_id configured")
		}
	}
	if api.liveState != progressID {
		t.Fatalf("live state = %q, want unchanged in-progress", api.liveState)
	}
}

func TestProviderLimitStaysInProgress(t *testing.T) {
	root, r, c, api := gapRunner(t, "limit-during")
	c.Projects[projectID].NeedsOperatorStateID = needsOperatorID
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if api.liveState != progressID {
		t.Fatalf("provider-limited run is resumed automatically and should stay in progress, live state %q", api.liveState)
	}
}

func TestNeedsOperatorStateValidation(t *testing.T) {
	_, r, c, _ := gapRunner(t, "ready")
	rt := c.Projects[projectID]
	rt.NeedsOperatorStateID = readyID
	if err := r.validateConfig(c); err == nil || !strings.Contains(err.Error(), "differ from ready_state_id") {
		t.Fatalf("same state as ready should be rejected, got %v", err)
	}
	rt.NeedsOperatorStateID = "needs-operator"
	if err := r.validateConfig(c); err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("non-UUID state should be rejected, got %v", err)
	}
	rt.NeedsOperatorStateID = needsOperatorID
	if err := r.validateConfig(c); err != nil {
		t.Fatal(err)
	}
}
