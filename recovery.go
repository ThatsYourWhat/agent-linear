package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

type runContext struct {
	ProjectID       string         `json:"project_id"`
	Route           *route         `json:"route"`
	Branch          string         `json:"branch"`
	BaseSHA         string         `json:"base_sha"`
	Scratch         string         `json:"scratch"`
	Checkout        string         `json:"checkout"`
	Profile         gitProfile     `json:"git_profile"`
	Engine          string         `json:"engine"`
	Executable      string         `json:"executable"`
	Model           *string        `json:"model"`
	ReasoningEffort *string        `json:"model_reasoning_effort,omitempty"`
	Issue           map[string]any `json:"issue"`
	Owner           string         `json:"owner"`
	ProgressState   string         `json:"progress_state"`
	ReviewState     string         `json:"review_state"`
	Requirements    []requirement  `json:"requirements"`
}

func saveRunContext(db *sql.DB, id string, saved runContext) error {
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if _, err = db.Exec("INSERT INTO run_context VALUES (?,?)", id, string(raw)); err != nil {
		return err
	}
	return writeJSON(filepath.Join(saved.Scratch, "context.json"), saved)
}

func (r *runner) finishRun(db *sql.DB, saved runContext, report map[string]any, gh string) error {
	id := textAt(saved.Issue, "id")
	if err := recordActivity(saved.Scratch, "verifying"); err != nil {
		return err
	}
	if err := r.verifyPR(report, saved.Route, saved.Checkout, saved.Branch, saved.BaseSHA, gh, saved.Profile, saved.Requirements); err != nil {
		return err
	}
	if textAt(report, "status") == "ready" {
		current, err := r.issue(id)
		if err != nil {
			return err
		}
		if textAt(current, "id") != id || textAt(current, "project", "id") != saved.ProjectID || textAt(current, "team", "id") != saved.Route.TeamID || textAt(current, "assignee", "id") != saved.Owner || textAt(current, "title") != textAt(saved.Issue, "title") || textAt(current, "description") != textAt(saved.Issue, "description") || !reflect.DeepEqual(current["relations"], saved.Issue["relations"]) {
			return errors.New("Linear ownership, route or requirements changed; human changes preserved")
		}
		requestPath := filepath.Join(saved.Scratch, "finish-state-request.json")
		state := textAt(current, "state", "id")
		switch {
		case state == saved.ReviewState || textAt(current, "state", "type") == "completed":
			var request map[string]any
			if err = readJSON(requestPath, &request); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err == nil && (textAt(request, "id") != id || textAt(request, "input", "stateId") != saved.ReviewState) {
				return errors.New("recorded finish request conflicts with the verified issue state")
			}
			// Verified review/completion needs no state mutation or backward transition.
			if err = writeJSON(filepath.Join(saved.Scratch, "finish-state.json"), map[string]any{"success": true, "issue": current, "observed_state": state}); err != nil {
				return err
			}
		case state == saved.ProgressState:
			if exists(requestPath) || exists(filepath.Join(saved.Scratch, "finish-state.json")) {
				return errors.New("previous finish mutation may have applied; inspect Linear before authorizing another update")
			}
			if err = r.updateIssueState(id, saved.ReviewState, saved.Scratch, "finish", "", false); err != nil {
				return err
			}
		default:
			return errors.New("Linear status changed; human changes preserved")
		}
	}
	if _, err := db.Exec("UPDATE runs SET status=?,pr_url=? WHERE issue_id=?", textAt(report, "status"), textAt(report, "pr_url"), id); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM provider_recoveries WHERE issue_id=?", id); err != nil {
		return err
	}
	if err := recordActivity(saved.Scratch, textAt(report, "status")); err != nil {
		return err
	}
	if textAt(report, "status") == "ready" {
		if err := archiveResolvedErrors(saved.Scratch); err != nil {
			fmt.Fprintln(r.errOut, "Run outcome ready; previous diagnostics could not be archived")
		}
	}
	return nil
}

func archiveResolvedErrors(folder string) error {
	archive := ""
	for _, name := range []string{"failure.txt", "reconcile-error.txt", "provider-limit.json"} {
		path := filepath.Join(folder, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if archive == "" {
			var err error
			archive, err = os.MkdirTemp(folder, "resolved-errors-")
			if err != nil {
				return err
			}
		}
		if err := os.Rename(path, filepath.Join(archive, name)); err != nil {
			return err
		}
	}
	return nil
}

// resolveIssueID maps an issue reference to its Linear UUID. A reference may
// already be a UUID, or it may be a human identifier such as "INT-3" resolved
// through the Linear API.
func (r *runner) resolveIssueID(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("--issue is required")
	}
	if uuidRE.MatchString(ref) {
		return ref, nil
	}
	issue, err := r.issue(ref)
	if err != nil {
		return "", err
	}
	return textAt(issue, "id"), nil
}

func loadRecovery(c *config, id, runs string, db *sql.DB) (runContext, error) {
	var saved runContext
	if !uuidRE.MatchString(id) {
		return saved, errors.New("recovery requires --issue with a recorded issue UUID")
	}
	var raw, folder, status string
	if err := db.QueryRow("SELECT folder,status FROM runs WHERE issue_id=?", id).Scan(&folder, &status); err != nil {
		return saved, fmt.Errorf("cannot find recorded run: %w", err)
	}
	if !isClaimed(status) {
		return saved, errors.New("deferred work has no feature session to recover")
	}
	if err := db.QueryRow("SELECT context FROM run_context WHERE issue_id=?", id).Scan(&raw); err != nil {
		return saved, errors.New("run has no trusted recovery context; inspect the retained artifacts manually")
	}
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return saved, err
	}
	expected := filepath.Join(runs, id)
	current := c.Projects[saved.ProjectID]
	if saved.Route == nil || current == nil {
		return saved, errors.New("saved run does not match the configured route and original context")
	}
	// The skip_ci_checks toggle may change after launch without invalidating the
	// run; compare route identity ignoring it, then honor the current value.
	savedRoute := *saved.Route
	currentRoute := *current
	savedRoute.SkipCIChecks = currentRoute.SkipCIChecks
	if !reflect.DeepEqual(savedRoute, currentRoute) || saved.Scratch != expected || folder != expected || saved.Checkout != filepath.Join(expected, "repo") || textAt(saved.Issue, "id") != id || saved.Owner == "" || saved.ProgressState == "" || saved.ReviewState == "" || !reflect.DeepEqual(saved.Requirements, issueRequirements(saved.Issue)) {
		return saved, errors.New("saved run does not match the configured route and original context")
	}
	saved.Route.SkipCIChecks = currentRoute.SkipCIChecks
	for _, path := range []string{saved.Scratch, saved.Checkout} {
		actual, err := filepath.EvalSymlinks(path)
		if err != nil || actual != path {
			return saved, errors.New("recovery checkout/scratch is missing or redirects outside the recorded path")
		}
	}
	if err := checkoutIdle(saved.Checkout, saved.Executable); err != nil {
		return saved, err
	}
	return saved, nil
}

func (r *runner) reconcile(c *config, id, runs string, db *sql.DB) error {
	resolved, err := r.resolveIssueID(id)
	if err != nil {
		return err
	}
	id = resolved
	if err := stopProviderRecovery(db, id, "Manual reconciliation requested; inspect retained result and logs before further recovery"); err != nil {
		return err
	}
	saved, err := loadRecovery(c, id, runs, db)
	if err != nil {
		return err
	}
	viewer, _, err := r.viewer()
	if err != nil {
		return err
	}
	current, err := r.issue(id)
	if err != nil {
		return err
	}
	if textAt(current, "assignee", "id") != viewer {
		return errors.New("only the currently assigned Linear user may reconcile this issue; reassign it explicitly before continuing")
	}
	// Reassignment permits the new owner to finish; retain the original stored launch context.
	saved.Owner = viewer
	if saved.Engine == "opencode" {
		if err := opencodeReport(saved.Scratch); err != nil {
			return err
		}
	}
	report, err := readReport(saved.Scratch)
	if err != nil {
		return err
	}
	if err = r.finishRun(db, saved, report, c.GH); err != nil {
		if e := writeFile(filepath.Join(saved.Scratch, "reconcile-error.txt"), []byte(redact(err.Error())+"\n")); e != nil {
			return errors.Join(err, e)
		}
		return err
	}
	stage := "reconcile"
	if !exists(filepath.Join(saved.Scratch, stage+"-comment.json")) {
		if exists(filepath.Join(saved.Scratch, stage+"-comment.txt")) {
			return errors.New("reconciliation saved; previous comment delivery is uncertain and was not repeated")
		}
		body := fmt.Sprintf("Recorded run reconciled with status %s. View pull request at %s. No implementation was relaunched.", textAt(report, "status"), textAt(report, "pr_url"))
		if textAt(report, "status") == "ready" && noCodeCompletion(report) {
			body = "Recorded run reconciled with status ready. No code changes; PR not required. No implementation was relaunched."
		}
		if err = r.postComment(id, body, saved.Scratch, stage); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "%s: reconciled %s %s\n", textAt(saved.Issue, "identifier"), textAt(report, "status"), textAt(report, "pr_url"))
	return nil
}

func checkoutIdle(repo, executable string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			// Unrelated non-dumpable helpers can hide their cwd; never ignore the recorded CLI.
			commandLine, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if os.IsNotExist(readErr) || errors.Is(readErr, syscall.ESRCH) {
				continue
			}
			if readErr != nil {
				return readErr
			}
			program := strings.SplitN(string(commandLine), "\x00", 2)[0]
			if filepath.Base(program) == filepath.Base(executable) {
				return fmt.Errorf("cannot establish whether agent process %d is active: %w", pid, err)
			}
			continue
		}
		if cwd == repo || strings.HasPrefix(cwd, repo+string(filepath.Separator)) {
			return fmt.Errorf("process %d still uses the checkout; reconciliation refused", pid)
		}
	}
	return nil
}
