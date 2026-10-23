package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSigningProfileAndCommitGates(t *testing.T) {
	root := t.TempDir()
	repo := testRepo(t, root)
	r := newRunner(filepath.Join(root, "agent-linear"))
	profile, err := r.gitProfile(repo)
	if err != nil {
		t.Fatal(err)
	}
	base := mustCommand(t, repo, "git", "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(repo, "app.txt"), "feature\n")
	mustCommand(t, repo, "git", "add", "app.txt")
	mustCommand(t, repo, "git", "commit", "-S", "-m", "Feature")
	if err = r.verifyCommits(repo, base, profile); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"author", "email", "key", "unsigned"} {
		t.Run(variant, func(t *testing.T) {
			candidate := gitProfile{Settings: map[string]string{}, Fingerprints: append([]string{}, profile.Fingerprints...)}
			for k, v := range profile.Settings {
				candidate.Settings[k] = v
			}
			switch variant {
			case "author":
				candidate.Settings["user.name"] = "Other"
			case "email":
				candidate.Settings["user.email"] = "other@example.com"
			case "key":
				candidate.Fingerprints = []string{"SHA256:other"}
			case "unsigned":
				mustWrite(t, filepath.Join(repo, "app.txt"), "unsigned\n")
				mustCommand(t, repo, "git", "add", "app.txt")
				mustCommand(t, repo, "git", "-c", "commit.gpgsign=false", "commit", "--no-gpg-sign", "-m", "Unsigned")
			}
			if err := r.verifyCommits(repo, base, candidate); err == nil {
				t.Fatal("invalid feature commits accepted")
			}
		})
	}
	pub, err := os.ReadFile(profile.Settings["user.signingkey"])
	if err != nil {
		t.Fatal(err)
	}
	mustCommand(t, repo, "git", "config", "user.signingkey", "key::"+strings.TrimSpace(string(pub)))
	inline, err := r.gitProfile(repo)
	if err != nil || inline.Fingerprints[0] != profile.Fingerprints[0] {
		t.Fatal("inline SSH key", err)
	}
	mustCommand(t, repo, "git", "config", "user.name", "")
	if _, err = r.gitProfile(repo); err == nil {
		t.Fatal("missing profile accepted")
	}
	for _, format := range []string{"openpgp", "x509"} {
		t.Run(format, func(t *testing.T) {
			fingerprint := strings.Repeat("A", 40)
			values := map[string]string{"user.name": "Test", "user.email": "test@example.com", "user.signingkey": fingerprint + "!", "gpg.format": format}
			r.command = func(dir string, args ...string) (string, error) {
				if args[0] == "git" {
					return values[args[len(args)-1]], nil
				}
				record := "pub"
				if format == "x509" {
					record = "crt"
				}
				return record + ":\nfpr:::::::::" + fingerprint + ":\nfpr:::::::::" + strings.Repeat("B", 40) + ":", nil
			}
			got, err := r.gitProfile(repo)
			if err != nil || len(got.Fingerprints) != 1 || got.Fingerprints[0] != fingerprint {
				t.Fatal(got, err)
			}
		})
	}
}
func TestReportAndReleaseGates(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(asset("result.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	rt := testConfig("/tmp").Projects[projectID]
	for _, variant := range []string{"valid", "missing-field", "unknown-field", "wrong-type", "bad-enum", "missing-criterion", "failed-criterion", "blank-evidence", "failed-check", "empty-checks", "blocker", "security", "qa", "sha", "foreign-pr", "deployment"} {
		t.Run(variant, func(t *testing.T) {
			report := goodReport()
			switch variant {
			case "missing-field":
				delete(report, "qa")
			case "unknown-field":
				report["extra"] = true
			case "wrong-type":
				report["checks"].([]any)[0].(map[string]any)["passed"] = "true"
			case "bad-enum":
				report["status"] = "done"
			case "missing-criterion":
				report["criteria"] = []any{}
			case "failed-criterion":
				report["criteria"].([]any)[0].(map[string]any)["status"] = "unverified"
			case "blank-evidence":
				report["criteria"].([]any)[0].(map[string]any)["evidence"] = " "
			case "failed-check":
				report["checks"].([]any)[0].(map[string]any)["passed"] = false
			case "empty-checks":
				report["checks"] = []any{}
			case "blocker":
				report["blockers"] = []any{"waiting"}
			case "security":
				report["security"] = "CONDITIONAL"
			case "qa":
				report["qa"] = "unverified"
			case "sha":
				report["commit_sha"] = "abc"
			case "foreign-pr":
				report["pr_url"] = "https://github.com/another/repo/pull/12"
			case "deployment":
				report["deployment"].(map[string]any)["status"] = "verified"
			}
			err := validateReport(report, schema, "result")
			if err == nil {
				err = validateResult(report, rt)
			}
			if (err == nil) != (variant == "valid") {
				t.Fatalf("%s: %v", variant, err)
			}
		})
	}
}
func TestWorkflowStateAndAssignmentConfirmation(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-secret")
	for _, variant := range []string{"valid", "missing", "duplicate", "case", "pages"} {
		t.Run("states/"+variant, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var call apiCall
				if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
					t.Error(err)
					return
				}
				if textAt(call.Variables, "team") != teamID {
					t.Error("wrong team")
				}
				nodes := []any{map[string]any{"id": progressID, "name": "In Progress"}, map[string]any{"id": reviewID, "name": "In Review"}}
				switch variant {
				case "missing":
					nodes = nodes[:1]
				case "duplicate":
					nodes = append(nodes, nodes[0])
				case "case":
					nodes[0].(map[string]any)["name"] = "in progress"
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"workflowStates": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": variant == "pages"}}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			r := newRunner("/tmp/agent-linear")
			r.endpoint = server.URL
			r.client = server.Client()
			states, err := r.workflowStates(teamID)
			if (err == nil) != (variant == "valid") {
				t.Fatal(states, err)
			}
		})
	}
	for _, variant := range []string{"valid", "success", "issue", "state", "assignee", "unassigned"} {
		t.Run("update/"+variant, func(t *testing.T) {
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				count++
				var call apiCall
				if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
					t.Error(err)
					return
				}
				if textAt(call.Variables, "input", "assigneeId") != userID {
					t.Error("missing assignee")
				}
				issue := map[string]any{"id": issueID, "state": map[string]any{"id": progressID}, "assignee": map[string]any{"id": userID}}
				switch variant {
				case "issue":
					issue["id"] = projectID
				case "state":
					issue["state"] = map[string]any{"id": reviewID}
				case "assignee":
					issue["assignee"] = map[string]any{"id": projectID}
				case "unassigned":
					issue["assignee"] = nil
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueUpdate": map[string]any{"success": variant != "success", "issue": issue}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			r := newRunner("/tmp/agent-linear")
			r.endpoint = server.URL
			r.client = server.Client()
			folder := t.TempDir()
			err := r.updateIssueState(issueID, progressID, folder, "start", userID, true)
			if (err == nil) != (variant == "valid") || count != 1 || !exists(filepath.Join(folder, "start-state-request.json")) || exists(filepath.Join(folder, "start-state.json")) != (variant == "valid") {
				t.Fatal("unconfirmed/retried mutation", err, count)
			}
		})
	}
}
func TestEventParsingAndResume(t *testing.T) {
	folder := t.TempDir()
	events := filepath.Join(folder, "events.jsonl")
	mustWrite(t, events, "partial\n[]\n"+`{"type":"thread.started","thread_id":"bad"}`+"\n"+`{"type":"thread.started","thread_id":"`+projectID+`"}`+"\n{partial")
	if id := sessionID(folder, "codex"); id != projectID {
		t.Fatal(id)
	}
	report, err := json.Marshal(goodReport())
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	emit := func(kind string, part any) {
		if err := json.NewEncoder(&stream).Encode(map[string]any{"type": kind, "sessionID": "ses_fixture123", "part": part}); err != nil {
			t.Fatal(err)
		}
	}
	emit("step_start", nil)
	emit("text", map[string]any{"id": "old", "text": "intermediate"})
	emit("step_finish", map[string]any{"reason": "tool-calls"})
	emit("step_start", nil)
	// Repeated part IDs replace previous text; only final-step parts form the report.
	emit("text", map[string]any{"id": "a", "text": "outdated"})
	emit("text", map[string]any{"id": "a", "text": string(report[:20])})
	emit("text", map[string]any{"id": "b", "text": string(report[20:]) + strings.Repeat(" ", 100000)})
	emit("step_finish", map[string]any{"reason": "stop"})
	mustWrite(t, events, stream.String())
	if err = opencodeReport(folder); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = readJSON(filepath.Join(folder, "result.json"), &got); err != nil || textAt(got, "pr_url") != prURL {
		t.Fatal(got, err)
	}
	if id := sessionID(folder, "opencode"); id != "ses_fixture123" {
		t.Fatal(id)
	}
}
func TestFailureEvidenceRedactsLinearKey(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{}
	r := newFixtureRunner(t, root, source, api)
	original := r.command
	r.command = func(dir string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "git" && args[1] == "clone" {
			return "", fmt.Errorf("test-secret: clone unavailable")
		}
		return original(dir, args...)
	}
	runs := filepath.Join(root, ".runs")
	if err := os.Mkdir(runs, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = r.runIssue(testConfig(source), projectID, testIssue(), runs, db, nil, false); err == nil {
		t.Fatal("startup failure ignored")
	}
	evidence, err := os.ReadFile(filepath.Join(runs, issueID, "failure.txt"))
	if err != nil || strings.Contains(string(evidence), "test-secret") || !strings.Contains(string(evidence), "[REDACTED]") {
		t.Fatal(string(evidence), err)
	}
}
func TestWorkerLockKeepsCommandsSerial(t *testing.T) {
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
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Worker already active") {
		t.Fatal(out.String())
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil && !errors.Is(err, syscall.EBADF) {
		t.Fatal(err)
	}
}

func TestConfigDefaultsAndInvalidValues(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(asset("config.example.json")), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "engine")
	delete(raw, "codex")
	delete(raw, "opencode")
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if engine, binary := c.agent(); engine != "codex" || binary != "codex" {
		t.Fatal("legacy defaults changed")
	}
	for _, key := range []string{"engine", "codex", "gh", "model"} {
		for _, value := range []any{nil, "", 12} {
			raw[key] = value
			data, err = json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = decodeConfig(data); err == nil {
				t.Fatal("invalid configuration value accepted", key, value)
			}
			delete(raw, key)
		}
	}
}
