package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const projectID = "11111111-1111-4111-8111-111111111111"
const issueID = "22222222-2222-4222-8222-222222222222"
const teamID = "33333333-3333-4333-8333-333333333333"
const readyID = "44444444-4444-4444-8444-444444444444"
const progressID = "66666666-6666-4666-8666-666666666666"
const reviewID = "77777777-7777-4777-8777-777777777777"
const userID = "88888888-8888-4888-8888-888888888888"
const prURL = "https://github.com/acme/widget/pull/12"

func goodReport() map[string]any {
	var result map[string]any
	if err := json.Unmarshal([]byte(`{"status":"ready","summary":"Handle empty input","pr_url":"https://github.com/acme/widget/pull/12","commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","criteria":[{"requirement":"Reject empty input","status":"passed","evidence":"test passed"}],"checks":[{"command":"go test ./...","passed":true,"evidence":"exit 0"}],"security":"APPROVED","qa":"passed","blockers":[],"deployment":{"status":"not-requested","environment":"none","sha":"","url":"","evidence":"PR only"}}`), &result); err != nil {
		panic(err)
	}
	return result
}
func testIssue() map[string]any {
	return map[string]any{"id": issueID, "identifier": "APP-1", "title": "Handle input", "description": "Reject empty input", "project": map[string]any{"id": projectID}, "team": map[string]any{"id": teamID}, "state": map[string]any{"id": readyID}}
}
func testConfig(repo string) *config {
	var c config
	if err := json.Unmarshal([]byte(asset("config.example.json")), &c); err != nil {
		panic(err)
	}
	var rt *route
	for _, v := range c.Projects {
		rt = v
		break
	}
	rt.TeamID = teamID
	rt.ReadyStateID = readyID
	rt.GitHubRepo = "acme/widget"
	rt.RepoPath = repo
	rt.Label = "agent-ready"
	c.Projects = map[string]*route{projectID: rt}
	return &c
}
func mustCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := command(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := writeFile(path, []byte(text)); err != nil {
		t.Fatal(err)
	}
}
func testRepo(t *testing.T, root string) string {
	t.Helper()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, source, "git", "init", "-b", "staging")
	for k, v := range map[string]string{"user.name": "Test", "user.email": "test@example.com", "commit.gpgsign": "false"} {
		mustCommand(t, source, "git", "config", k, v)
	}
	mustWrite(t, filepath.Join(source, "app.txt"), "original\n")
	mustCommand(t, source, "git", "add", "app.txt")
	mustCommand(t, source, "git", "commit", "-m", "Initial")
	key := filepath.Join(root, "signing")
	mustCommand(t, "", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(root, "allowed-signers")
	mustWrite(t, allowed, "test@example.com "+string(pub))
	for k, v := range map[string]string{"gpg.format": "ssh", "user.signingkey": key + ".pub", "gpg.ssh.allowedSignersFile": allowed} {
		mustCommand(t, source, "git", "config", k, v)
	}
	mustCommand(t, source, "git", "remote", "add", "origin", "https://github.com/acme/widget.git")
	mustWrite(t, filepath.Join(source, "app.txt"), "human uncommitted work\n")
	return source
}

// The test executable is also the external agent/gh/crontab fixture.
func TestMain(m *testing.M) {
	if root := os.Getenv("LINEAR_TEST_FIXTURE"); root != "" {
		switch filepath.Base(os.Args[0]) {
		case "codex", "opencode":
			os.Exit(agentFixture(root, filepath.Base(os.Args[0])))
		case "gh":
			os.Exit(githubFixture(root))
		case "crontab":
			os.Exit(crontabFixture(root))
		case "lock-parent":
			os.Exit(lockParentFixture(root))
		case "lock-child":
			os.Exit(lockChildFixture(root))
		}
	}
	os.Exit(m.Run())
}
func agentFixture(root, engine string) int {
	fail := func(v any) int { fmt.Fprintln(os.Stderr, v); return 1 }
	if len(os.Args) > 1 && (os.Args[1] == "login" || os.Args[1] == "auth" || os.Args[1] == "models") {
		fmt.Println("test/model")
		return 0
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(err)
	}
	if !strings.Contains(string(prompt), "Reject empty input") || os.Getenv("LINEAR_API_KEY") != "" || os.Getenv("GIT_AUTHOR_NAME") != "Test" || os.Getenv("GIT_AUTHOR_EMAIL") != "test@example.com" {
		return fail("invalid prompt/environment")
	}
	if !slices.Contains(os.Args, "--model") || slices.Contains(os.Args, "--session") || slices.Contains(os.Args, "resume") {
		return fail("invalid launch arguments")
	}
	repo, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	folder := filepath.Dir(repo)
	var start map[string]any
	if err = readJSON(filepath.Join(folder, "start-state.json"), &start); err != nil {
		return fail(err)
	}
	if textAt(start, "issue", "assignee", "id") != userID || textAt(start, "issue", "state", "id") != progressID {
		return fail("launch before confirmed assignment/status")
	}
	session := projectID
	if engine == "opencode" {
		session = "ses_test123"
	}
	emit := func(kind string, part map[string]any) {
		e := map[string]any{"type": kind, "sessionID": session}
		if part != nil {
			e["part"] = part
		}
		if kind == "thread.started" {
			e["thread_id"] = session
		}
		if err := json.NewEncoder(os.Stdout).Encode(e); err != nil {
			panic(err)
		}
	}
	if engine == "opencode" {
		var settings map[string]any
		if err = json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &settings); err != nil {
			return fail(err)
		}
		if textAt(settings, "agent", "linear-issue", "permission", "task") != "deny" || textAt(settings, "agent", "linear-issue", "permission", "question") != "deny" {
			return fail("unsafe permissions")
		}
		emit("step_start", nil)
	} else {
		if !slices.Contains(os.Args, "workspace-write") {
			return fail("missing sandbox")
		}
		emit("thread.started", nil)
	}
	outcomeBytes, err := os.ReadFile(filepath.Join(root, "outcome"))
	if err != nil {
		return fail(err)
	}
	outcome := string(outcomeBytes)
	if outcome == "process-error" {
		return fail("fixture process error")
	}
	original, err := os.ReadFile("app.txt")
	if err != nil || string(original) != "original\n" {
		return fail("incorrect isolated base")
	}
	if err = writeFile("app.txt", []byte("feature\n")); err != nil {
		return fail(err)
	}
	if _, err = command(repo, "git", "add", "app.txt"); err != nil {
		return fail(err)
	}
	args := []string{"git", "commit", "-S", "-m", "Handle input"}
	if outcome == "unsigned" {
		args = []string{"git", "-c", "commit.gpgsign=false", "commit", "--no-gpg-sign", "-m", "Unsigned"}
	}
	if _, err = command(repo, args...); err != nil {
		return fail(err)
	}
	report := goodReport()
	sha, err := command(repo, "git", "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	report["commit_sha"] = sha
	switch outcome {
	case "blocked", "failed":
		report["status"] = outcome
	case "invalid-report":
		report["checks"].([]any)[0].(map[string]any)["passed"] = "true"
	case "dirty":
		if err = writeFile("app.txt", []byte("uncommitted")); err != nil {
			return fail(err)
		}
	case "empty-criteria":
		report["criteria"] = []any{}
	case "unapproved":
		report["security"] = "CONDITIONAL"
	}
	data, err := json.Marshal(report)
	if err != nil {
		return fail(err)
	}
	if outcome == "malformed" {
		data = []byte("not JSON")
	}
	if engine == "codex" {
		index := slices.Index(os.Args, "--output-last-message")
		if index < 0 {
			return fail("no report path")
		}
		if err = writeFile(os.Args[index+1], data); err != nil {
			return fail(err)
		}
	} else {
		emit("text", map[string]any{"id": "part_test", "text": string(data)})
		reason := "stop"
		if outcome == "incomplete" {
			reason = "length"
		}
		emit("step_finish", map[string]any{"reason": reason})
		if outcome == "error-event" {
			emit("error", nil)
		}
	}
	return 0
}
func githubFixture(root string) int {
	args := strings.Join(os.Args[1:], " ")
	if strings.HasPrefix(args, "auth status") {
		return 0
	}
	if strings.HasPrefix(args, "repo view") {
		fmt.Println(`{"viewerPermission":"WRITE"}`)
		return 0
	}
	if !strings.HasPrefix(args, "pr view") {
		return 1
	}
	repo, err := os.Getwd()
	if err != nil {
		return 1
	}
	sha, err := command(repo, "git", "rev-parse", "HEAD")
	if err != nil {
		return 1
	}
	branch, err := command(repo, "git", "branch", "--show-current")
	if err != nil {
		return 1
	}
	outcome, err := os.ReadFile(filepath.Join(root, "outcome"))
	if err != nil {
		return 1
	}
	base := "staging"
	if string(outcome) == "wrong-pr" {
		base = "main"
	}
	err = json.NewEncoder(os.Stdout).Encode(map[string]any{"url": prURL, "state": "OPEN", "isDraft": false, "baseRefName": base, "headRefName": branch, "headRefOid": sha, "isCrossRepository": false})
	if err != nil {
		return 1
	}
	return 0
}
func crontabFixture(root string) int {
	if exists(filepath.Join(root, "crontab-error")) {
		fmt.Fprintln(os.Stderr, "permission denied")
		return 2
	}
	path := filepath.Join(root, "crontab")
	if len(os.Args) != 2 {
		return 2
	}
	if os.Args[1] == "-l" {
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "no crontab for fixture")
			return 1
		}
		if err != nil {
			return 2
		}
		fmt.Print(string(b))
		return 0
	}
	if os.Args[1] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 2
		}
		if err = writeFile(path, b); err != nil {
			return 2
		}
		return 0
	}
	return 2
}
func fixtureBinaries(t *testing.T, root string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "opencode", "gh", "crontab", "lock-parent", "lock-child"} {
		if err = os.Symlink(exe, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LINEAR_TEST_FIXTURE", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LINEAR_API_KEY", "test-secret")
}

type apiCall struct {
	Query     string
	Variables map[string]any
}
type linearFixture struct {
	mu         sync.Mutex
	calls      []apiCall
	outcome    string
	issue      map[string]any
	queueReads int
}

func (f *linearFixture) handler(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Header.Get("Authorization") != "test-secret" {
		w.WriteHeader(401)
		return
	}
	var call apiCall
	if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
		w.WriteHeader(400)
		return
	}
	f.calls = append(f.calls, call)
	var data any
	switch {
	case strings.Contains(call.Query, "viewer"):
		id := userID
		if f.outcome == "viewer-error" {
			id = ""
		}
		data = map[string]any{"viewer": map[string]any{"id": id}}
	case strings.Contains(call.Query, "workflowStates("):
		nodes := []any{map[string]any{"id": progressID, "name": "In Progress"}, map[string]any{"id": reviewID, "name": "In Review"}}
		if f.outcome == "missing-state" {
			nodes = nodes[:1]
		}
		data = map[string]any{"workflowStates": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}
	case strings.Contains(call.Query, "issueUpdate("):
		state := textAt(call.Variables, "input", "stateId")
		if f.outcome == "start-state-error" || f.outcome == "review-state-error" && state == reviewID {
			w.WriteHeader(503)
			fmt.Fprint(w, "test-secret")
			return
		}
		assignee := userID
		if f.outcome == "assignment-error" {
			assignee = projectID
		}
		data = map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": textAt(call.Variables, "id"), "state": map[string]any{"id": state}, "assignee": map[string]any{"id": assignee}}}}
	case strings.Contains(call.Query, "commentCreate("):
		body := textAt(call.Variables, "input", "body")
		if f.outcome == "start-error" && strings.Contains(body, "Run claimed") || f.outcome == "finish-error" && strings.Contains(body, "Run finished") {
			w.WriteHeader(503)
			return
		}
		data = map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": issueID}}}
	case strings.Contains(call.Query, "issues("):
		f.queueReads++
		nodes := []any{f.issue}
		if f.issue == nil || f.outcome == "withdrawn" && f.queueReads > 1 {
			nodes = []any{}
		}
		data = map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
	default:
		w.WriteHeader(400)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		panic(err)
	}
}
func newFixtureRunner(t *testing.T, root, source string, api *linearFixture) *runner {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(server.Close)
	r := newRunner(filepath.Join(root, "agent-linear"))
	r.out = io.Discard
	r.errOut = io.Discard
	r.endpoint = server.URL
	r.client = server.Client()
	r.command = func(dir string, args ...string) (string, error) {
		if slices.Equal(args, []string{"git", "remote", "get-url", "origin"}) {
			return "https://github.com/acme/widget.git", nil
		}
		if len(args) > 3 && slices.Equal(args[:4], []string{"git", "remote", "set-url", "origin"}) {
			_, err := command(dir, "git", "remote", "set-url", "origin", source)
			return "", err
		}
		if slices.Equal(args, []string{"git", "ls-remote", "--exit-code", "origin", "refs/heads/staging"}) {
			return command(dir, "git", "ls-remote", "--exit-code", source, "refs/heads/staging")
		}
		return command(dir, args...)
	}
	return r
}
func TestIssueWorkflow(t *testing.T) {
	outcomes := []string{"ready", "blocked", "failed", "invalid-report", "malformed", "process-error", "wrong-pr", "unsigned", "dirty", "empty-criteria", "unapproved", "viewer-error", "assignment-error", "missing-state", "start-error", "start-state-error", "review-state-error", "finish-error"}
	for _, engine := range []string{"codex", "opencode"} {
		variants := append([]string{}, outcomes...)
		if engine == "opencode" {
			variants = append(variants, "incomplete", "error-event")
		}
		for _, outcome := range variants {
			t.Run(engine+"/"+outcome, func(t *testing.T) {
				root := t.TempDir()
				source := testRepo(t, root)
				fixtureBinaries(t, root)
				mustWrite(t, filepath.Join(root, "outcome"), outcome)
				api := &linearFixture{outcome: outcome}
				r := newFixtureRunner(t, root, source, api)
				c := testConfig(source)
				c.Engine = engine
				model := "test/model"
				c.Model = &model
				runs := filepath.Join(root, ".runs")
				if err := os.Mkdir(runs, 0700); err != nil {
					t.Fatal(err)
				}
				db, err := openState(filepath.Join(runs, "state.sqlite3"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if outcome == "ready" {
					if err = writeJSON(filepath.Join(root, "config.json"), c); err != nil {
						t.Fatal(err)
					}
					if err = writeJSON(filepath.Join(root, "issue.json"), testIssue()); err != nil {
						t.Fatal(err)
					}
					err = executeTest(r, "run", "--project", projectID, "--issue-file", filepath.Join(root, "issue.json"))
				} else {
					err = r.runIssue(c, projectID, testIssue(), runs, db, nil)
				}
				expected := "failed"
				if outcome == "ready" || outcome == "finish-error" {
					expected = "ready"
				}
				if outcome == "blocked" {
					expected = "blocked"
				}
				if (err == nil) != (outcome == "ready" || outcome == "blocked" || outcome == "failed" || outcome == "finish-error") {
					t.Fatalf("unexpected workflow result: %v", err)
				}
				status, e := claimedStatus(db, issueID)
				if e != nil || status != expected {
					t.Fatalf("status %q %v; expected %q", status, e, expected)
				}
				api.mu.Lock()
				calls := append([]apiCall{}, api.calls...)
				api.mu.Unlock()
				if err = r.runIssue(c, projectID, testIssue(), runs, db, nil); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				count := len(api.calls)
				api.mu.Unlock()
				if count != len(calls) {
					t.Fatal("claimed work repeated")
				}
				var transitions []string
				for _, call := range calls {
					if strings.Contains(call.Query, "issueUpdate(") {
						state := textAt(call.Variables, "input", "stateId")
						transitions = append(transitions, state)
						assignee := textAt(call.Variables, "input", "assigneeId")
						if state == progressID && assignee != userID || state == reviewID && assignee != "" {
							t.Fatal("incorrect assignment", call)
						}
					}
				}
				expectedStates := []string{}
				if outcome != "viewer-error" && outcome != "missing-state" && outcome != "start-error" {
					expectedStates = append(expectedStates, progressID)
				}
				if outcome == "ready" || outcome == "review-state-error" || outcome == "finish-error" {
					expectedStates = append(expectedStates, reviewID)
				}
				if !slices.Equal(transitions, expectedStates) {
					t.Fatalf("transitions %v; want %v", transitions, expectedStates)
				}
				folder := filepath.Join(runs, issueID)
				comment, e := os.ReadFile(filepath.Join(folder, "finish-comment.txt"))
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(string(comment), "status "+expected+".") {
					t.Fatal(string(comment))
				}
				original, e := os.ReadFile(filepath.Join(source, "app.txt"))
				if e != nil || string(original) != "human uncommitted work\n" {
					t.Fatal("source checkout was changed")
				}
				launched := exists(filepath.Join(folder, "context.json"))
				if slices.Contains([]string{"viewer-error", "missing-state", "assignment-error", "start-error", "start-state-error"}, outcome) && launched {
					t.Fatal("launched before confirmed start")
				}
				if expected == "failed" && outcome != "failed" && !exists(filepath.Join(folder, "failure.txt")) {
					t.Fatal("missing failure evidence")
				}
				if launched && outcome != "process-error" {
					if sessionID(folder, engine) == "" {
						t.Fatal("missing recovery session")
					}
				}
				if outcome == "finish-error" {
					if !exists(filepath.Join(folder, "finish-comment-error.txt")) || exists(filepath.Join(folder, "failure.txt")) {
						t.Fatal("notification changed saved outcome")
					}
				}
			})
		}
	}
}

func lockParentFixture(root string) int {
	lock, err := os.OpenFile(filepath.Join(root, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 1
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 1
	}
	c := testConfig(root)
	c.Codex = filepath.Join(root, "bin", "lock-child")
	r := newRunner(filepath.Join(root, "agent-linear"))
	_, err = r.launchAgent(c, root, root, "lock test", os.Environ(), nil, lock)
	if err != nil {
		return 1
	}
	return 0
}
func lockChildFixture(root string) int {
	if err := writeFile(filepath.Join(root, "child.pid"), []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return 1
	}
	deadline := time.Now().Add(15 * time.Second)
	for !exists(filepath.Join(root, "release")) {
		if time.Now().After(deadline) {
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}
func TestOrphanedAgentRetainsWorkerLock(t *testing.T) {
	root := t.TempDir()
	fixtureBinaries(t, root)
	parent := exec.Command(filepath.Join(root, "bin", "lock-parent"))
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if parent.ProcessState == nil {
			if err := parent.Process.Kill(); err != nil {
				t.Log(err)
			}
			if err := parent.Wait(); err == nil {
				t.Log("parent exited")
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !exists(filepath.Join(root, "child.pid")) {
		if time.Now().After(deadline) {
			t.Fatal("agent child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Log(err)
		}
	}()
	if err = parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = parent.Wait(); err == nil {
		t.Fatal("expected killed parent")
	}
	lock, err := os.OpenFile(filepath.Join(root, "worker.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("orphaned agent lost lock: %v", err)
	}
	mustWrite(t, filepath.Join(root, "release"), "done")
	deadline = time.Now().Add(10 * time.Second)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			t.Fatal("agent did not release lock", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
