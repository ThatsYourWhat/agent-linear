package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resetIssue returns a claimed issue to the ready state and removes its local
// claim, recovery context, and run directory so it can be picked up afresh. It
// refuses when remote work may still exist, so it cannot duplicate a PR.
func (r *runner) resetIssue(c *config, ref, runs string, db *sql.DB, dryRun bool) error {
	if ref == "" {
		return errors.New("reset requires --issue with an issue identifier or UUID")
	}
	issue, err := r.issue(ref)
	if err != nil {
		return err
	}
	id := textAt(issue, "id")
	identifier := textAt(issue, "identifier")
	if !uuidRE.MatchString(id) {
		return errors.New("Linear returned an invalid issue id")
	}
	rt := c.Projects[textAt(issue, "project", "id")]
	if rt == nil {
		return fmt.Errorf("issue %s belongs to a project that is not a configured route", identifier)
	}
	if owner := textAt(issue, "assignee", "id"); owner != "" && !boolAt(issue["assignee"], "isMe") {
		return errors.New("only the currently assigned Linear user may reset this issue")
	}

	var prURL string
	err = db.QueryRow("SELECT pr_url FROM runs WHERE issue_id=?", id).Scan(&prURL)
	claimed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if claimed {
		if prURL != "" {
			return fmt.Errorf("refusing to reset: a pull request was recorded at %s", prURL)
		}
		branch := "linear-" + strings.ToLower(identifier) + "-" + id[:8]
		exists, e := r.remoteBranchExists(rt.RepoPath, branch)
		if e != nil {
			return e
		}
		if exists {
			return fmt.Errorf("refusing to reset: remote branch %s still exists", branch)
		}
	}

	if dryRun {
		if claimed {
			fmt.Fprintf(r.out, "%s: would reset to ready, clear the local claim, and remove %s\n", identifier, filepath.Join(runs, id))
		} else {
			fmt.Fprintf(r.out, "%s: would reset to ready (no local claim found)\n", identifier)
		}
		return nil
	}

	evidence, err := os.MkdirTemp(runs, "reset-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(evidence)
	if err = r.updateIssueState(id, rt.ReadyStateID, evidence, "reset", "", false); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"runs", "run_context", "provider_recoveries"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE issue_id=?", id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(runs, id)); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s: reset to ready; local claim and run directory cleared\n", identifier)
	return nil
}

// remoteBranchExists reports whether origin has the named branch. A failure to
// reach the remote is treated as an error, so reset never proceeds unverified.
func (r *runner) remoteBranchExists(repoPath, branch string) (bool, error) {
	out, err := r.command(repoPath, "git", "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return false, fmt.Errorf("cannot verify remote state before reset: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}
