package main

import "testing"

func TestResolveIssueID(t *testing.T) {
	// A UUID passes through untouched, with no Linear call.
	r := newRunner("agent-linear")
	if got, err := r.resolveIssueID(issueID); err != nil || got != issueID {
		t.Fatalf("UUID passthrough: %q %v", got, err)
	}

	// An identifier resolves to its UUID through the Linear API.
	_, r2, _, _ := gapRunner(t, "ready")
	if got, err := r2.resolveIssueID("APP-1"); err != nil || got != issueID {
		t.Fatalf("identifier resolution: %q %v", got, err)
	}
	if _, err := r2.resolveIssueID("NOPE-1"); err == nil {
		t.Fatal("unknown identifier should fail")
	}
}

func TestReconcileAcceptsIdentifier(t *testing.T) {
	_, r, _, _ := gapRunner(t, "ready")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "reconcile", "--issue", "APP-1"); err != nil {
		t.Fatal(err)
	}
}
