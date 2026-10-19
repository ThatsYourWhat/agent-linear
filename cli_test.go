package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func executeTest(r *runner, args ...string) error {
	cmd := r.cli()
	cmd.SetArgs(args)
	return cmd.Execute()
}
func TestSetupAndCLI(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	path := filepath.Join(root, "config.json")
	original := testConfig(source)
	original.Engine = "opencode"
	if err := writeJSON(path, original); err != nil {
		t.Fatal(err)
	}
	// Unknown trusted metadata and existing routes must survive setup.
	var raw map[string]any
	if err := readJSON(path, &raw); err != nil {
		t.Fatal(err)
	}
	raw["operator_note"] = "preserve"
	if err := writeJSON(path, raw); err != nil {
		t.Fatal(err)
	}
	second := "00000000-0000-4000-8000-000000000000"
	r.in = strings.NewReader(strings.Join([]string{second, teamID, readyID, source, "-", "n", ""}, "\n"))
	if err := executeTest(r, "setup"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, ".runs")) {
		t.Fatal("setup created runtime state")
	}
	if err := readJSON(path, &raw); err != nil {
		t.Fatal(err)
	}
	if textAt(raw, "operator_note") != "preserve" {
		t.Fatal("lost metadata")
	}
	saved, err := r.loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Projects) != 2 || saved.Engine != "opencode" || saved.Projects[second].Label != "" || saved.Projects[second].BaseBranch != "staging" || saved.Projects[projectID].Deployment.Instructions != original.Projects[projectID].Deployment.Instructions {
		t.Fatal("setup did not preserve config")
	}
	if strings.Join(projectIDs(saved), ",") != projectID+","+second {
		t.Fatal("setup changed project processing order")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("config permissions", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r.in = strings.NewReader(strings.Join([]string{projectID, "invalid", "", "", "", "n", ""}, "\n"))
	if err = executeTest(r, "setup"); err == nil {
		t.Fatal("invalid setup accepted")
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid setup overwrote config")
	}
	for _, args := range [][]string{{"install-cron", "--dry-run"}, {"poll", "--setup"}, {"status", "extra"}, {"run"}} {
		if err = executeTest(r, args...); err == nil {
			t.Fatal("invalid flags accepted", args)
		}
	}
	for _, name := range []string{"setup", "smoke-test", "poll", "run", "status", "cron-line", "install-cron", "uninstall-cron"} {
		cmd, _, err := r.cli().Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatal("missing Cobra command", name)
		}
	}
}
func TestConfigurationGates(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	r := newRunner(filepath.Join(root, "agent-linear"))
	for _, variant := range []string{"valid", "engine", "model", "project", "team", "ready", "origin", "base", "unsigned", "mode", "environment", "instructions", "relative-path"} {
		t.Run(variant, func(t *testing.T) {
			c := testConfig(source)
			rt := c.Projects[projectID]
			switch variant {
			case "engine":
				c.Engine = "other"
			case "model":
				value := " "
				c.Model = &value
			case "project":
				delete(c.Projects, projectID)
				c.Projects["wrong"] = rt
			case "team":
				rt.TeamID = "wrong"
			case "ready":
				rt.ReadyStateID = "wrong"
			case "origin":
				rt.GitHubRepo = "another/repo"
			case "base":
				rt.BaseBranch = "main"
			case "unsigned":
				rt.RequireSignedCommits = false
			case "mode":
				rt.Deployment.Mode = "direct"
			case "environment":
				rt.Deployment.Environment = "production"
			case "instructions":
				rt.Deployment.Instructions = " "
			case "relative-path":
				rt.RepoPath = "."
			}
			if err := r.validateConfig(c); (err == nil) != (variant == "valid") {
				t.Fatalf("%s: %v", variant, err)
			}
		})
	}
	for _, remote := range []string{"git@github.com:Acme/Widget.git", "https://github.com/Acme/Widget.git"} {
		name, err := githubName(remote)
		if err != nil || name != "acme/widget" {
			t.Fatal(name, err)
		}
	}
	for _, remote := range []string{"https://secret@github.com/acme/widget.git", "https://example.com/acme/widget", "/local/repo"} {
		if _, err := githubName(remote); err == nil {
			t.Fatal("unsafe remote accepted")
		}
	}
	for _, variant := range []string{"project", "team", "state", "id", "identifier", "description"} {
		issue := testIssue()
		switch variant {
		case "project", "team", "state":
			issue[variant] = map[string]any{"id": "wrong"}
		default:
			issue[variant] = ""
		}
		if err := validateIssue(issue, projectID, testConfig(source).Projects[projectID]); err == nil {
			t.Fatal("invalid issue accepted", variant)
		}
	}
}
func TestSmokeTestIsReadOnly(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			fixtureBinaries(t, root)
			api := &linearFixture{issue: testIssue()}
			r := newFixtureRunner(t, root, source, api)
			c := testConfig(source)
			c.Engine = engine
			model := "test/model"
			c.Model = &model
			path := filepath.Join(root, "config.json")
			if err := writeJSON(path, c); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = executeTest(r, "smoke-test"); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || exists(filepath.Join(root, ".runs")) || exists(filepath.Join(root, "crontab")) {
				t.Fatal("smoke test had side effects")
			}
			api.mu.Lock()
			for _, call := range api.calls {
				if strings.Contains(call.Query, "mutation") {
					t.Fatal("smoke test mutation")
				}
			}
			api.mu.Unlock()
			t.Setenv("LINEAR_API_KEY", "")
			if err = r.smokeTest(c); err == nil {
				t.Fatal("missing key accepted")
			}
			t.Setenv("LINEAR_API_KEY", "test-secret")
			if engine == "opencode" {
				model = "missing/model"
				if err = r.smokeTest(c); err == nil {
					t.Fatal("missing model accepted")
				}
				model = "test/model"
			}
			savedCommand := r.command
			r.command = func(dir string, args ...string) (string, error) {
				if len(args) > 1 && args[0] == "gh" && args[1] == "repo" {
					return `{"viewerPermission":"READ"}`, nil
				}
				return savedCommand(dir, args...)
			}
			if err = r.smokeTest(c); err == nil {
				t.Fatal("read-only GitHub user accepted")
			}
		})
	}
}
func TestCronInstallPreviewUninstall(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{}
	r := newFixtureRunner(t, root, source, api)
	r.in = strings.NewReader(strings.Join([]string{projectID, teamID, readyID, source, "", "n", ""}, "\n"))
	mustWrite(t, filepath.Join(root, "crontab"), "0 0 * * * backup\n*/9 * * * * old # blank-agent-linear\n")
	var out bytes.Buffer
	r.out = &out
	if err := executeTest(r, "install-cron", "--setup"); err != nil {
		t.Fatal(err)
	}
	c, err := r.loadConfig(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(root, "crontab"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "0 0 * * * backup\n") || strings.Count(string(text), cronMarker) != 1 || !strings.Contains(string(text), shellQuote(r.executable)+" poll --config") || strings.Contains(string(text), "test-secret") {
		t.Fatal(string(text))
	}
	if exists(filepath.Join(root, ".runs", issueID)) {
		t.Fatal("cron installation launched issue")
	}
	if err = executeTest(r, "install-cron"); err != nil {
		t.Fatal(err)
	}
	text2, err := os.ReadFile(filepath.Join(root, "crontab"))
	if err != nil || !bytes.Equal(text, text2) {
		t.Fatal("cron not idempotent")
	}
	if err = r.installCron(c, filepath.Join(root, "config.json"), filepath.Join(root, ".runs"), true); err != nil {
		t.Fatal(err)
	}
	// Uninstall works without config/key and while another worker owns the lock.
	if err = os.Remove(filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEAR_API_KEY", "")
	lock, err := os.OpenFile(filepath.Join(root, ".runs", "worker.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = executeTest(r, "uninstall-cron"); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadFile(filepath.Join(root, "crontab"))
	if err != nil || string(remaining) != "0 0 * * * backup\n" {
		t.Fatal(string(remaining), err)
	}
	if !exists(filepath.Join(root, ".runs", "crontab.before-uninstall")) {
		t.Fatal("missing backup")
	}
	if err = executeTest(r, "uninstall-cron"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "crontab-error"), "fail")
	if err = executeTest(r, "uninstall-cron"); err == nil {
		t.Fatal("unreadable crontab accepted")
	}
}
func TestDryRunClaimsAndWithdrawnIssues(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{issue: testIssue()}
	r := newFixtureRunner(t, root, source, api)
	var out bytes.Buffer
	r.out = &out
	if err := writeJSON(filepath.Join(root, "config.json"), testConfig(source)); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "poll", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "APP-1: eligible") {
		t.Fatal(out.String())
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seen, err := claimedStatus(db, issueID)
	if err != nil || seen != "" {
		t.Fatal("dry run claimed issue")
	}
	api.mu.Lock()
	api.outcome = "withdrawn"
	api.queueReads = 0
	api.mu.Unlock()
	if err = executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	seen, err = claimedStatus(db, issueID)
	if err != nil || seen != "" {
		t.Fatal("withdrawn issue claimed")
	}
	if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", issueID, "APP-1", "running", "/original/scratch", ""); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.outcome = ""
	api.queueReads = 0
	api.mu.Unlock()
	if err = executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	reads := api.queueReads
	api.mu.Unlock()
	if reads != 1 {
		t.Fatal("already claimed issue was refreshed", reads)
	}
	out.Reset()
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `["APP-1","running","/original/scratch",""]`) {
		t.Fatal(out.String())
	}
	t.Setenv("LINEAR_API_KEY", "")
	mustWrite(t, filepath.Join(root, ".env"), "LINEAR_API_KEY=file-key\n")
	if err = executeTest(r, "poll", "--dry-run"); err == nil {
		t.Fatal("dotenv credential loaded")
	}
}
func TestGraphQLPaginationAndFailures(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-secret")
	for _, variant := range []string{"pages", "cursor", "errors", "null", "http", "malformed"} {
		t.Run(variant, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				var call apiCall
				if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
					t.Error(err)
					return
				}
				if textAt(call.Variables, "filter", "project", "id", "eq") != projectID || textAt(call.Variables, "filter", "labels", "name", "eq") != "agent-ready" {
					t.Error("missing server-side route filters")
				}
				switch variant {
				case "errors":
					fmt.Fprint(w, `{"data":{},"errors":[{"message":"test-secret"}]}`)
					return
				case "null":
					fmt.Fprint(w, `{"data":null}`)
					return
				case "http":
					w.WriteHeader(429)
					fmt.Fprint(w, "test-secret")
					return
				case "malformed":
					fmt.Fprint(w, "broken")
					return
				}
				if calls == 2 && textAt(call.Variables, "after") != "cursor1" {
					t.Error("missing cursor")
				}
				next := calls == 1 || variant == "cursor"
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": []any{testIssue()}, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "cursor1"}}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			r := newRunner("/tmp/agent-linear")
			r.endpoint = server.URL
			r.client = server.Client()
			items, err := r.issues(projectID, testConfig("/tmp").Projects[projectID], "")
			if variant == "pages" {
				if err != nil || len(items) != 2 || calls != 2 {
					t.Fatal(items, err, calls)
				}
			} else {
				if err == nil || strings.Contains(err.Error(), "test-secret") {
					t.Fatal("API failure not safely rejected", err)
				}
				if variant != "cursor" && calls != 1 {
					t.Fatal("API retried")
				}
			}
		})
	}
}

func TestExistingSQLiteClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite3")
	existing, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { existing.Close() })
	if _, err = existing.Exec("CREATE TABLE runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i, pair := range []struct{ id, status string }{{issueID, "ready"}, {projectID, "failed"}, {teamID, "running"}} {
		if _, err = existing.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", pair.id, fmt.Sprintf("APP-%d", i+1), pair.status, "/old/scratch", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err = existing.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, pair := range []struct{ id, status string }{{issueID, "ready"}, {projectID, "failed"}, {teamID, "running"}} {
		got, err := claimedStatus(db, pair.id)
		if err != nil || got != pair.status {
			t.Fatal("lost existing claim", got, err)
		}
	}
	if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", userID, "APP-4", "blocked", "/old/scratch", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
}
