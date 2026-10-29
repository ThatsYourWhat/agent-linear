package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (r *runner) pruneSessions(c *config, runs string, db *sql.DB, dryRun bool) error {
	rows, err := db.Query(`SELECT issue_id FROM runs WHERE status NOT IN ('running','deferred')
 AND issue_id NOT IN (SELECT issue_id FROM provider_recoveries WHERE state='waiting') ORDER BY issue_id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	var failures error
	for _, id := range ids {
		if err := r.pruneSession(c, id, runs, db, dryRun); err != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", id, err))
		}
	}
	return failures
}

func (r *runner) pruneSession(c *config, id, runs string, db *sql.DB, dryRun bool) error {
	saved, err := loadRecovery(c, id, runs, db)
	if err != nil {
		return err
	}
	if saved.Engine != "codex" && saved.Engine != "opencode" || saved.Executable == "" {
		return errors.New("invalid recorded agent")
	}
	identifier := textAt(saved.Issue, "identifier")
	session := sessionID(saved.Scratch, saved.Engine)
	if session == "" {
		fmt.Fprintf(r.out, "%s: skipped; no recorded session ID\n", identifier)
		return nil
	}
	marker := filepath.Join(saved.Scratch, "session-pruned.json")
	var pruned map[string]any
	if err = readJSON(marker, &pruned); err == nil {
		if textAt(pruned, "engine") != saved.Engine || textAt(pruned, "session_id") != session {
			return errors.New("session deletion record does not match the recorded session")
		}
		fmt.Fprintf(r.out, "%s: session already pruned\n", identifier)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	current, err := r.issue(id)
	if err != nil {
		return err
	}
	if textAt(current, "project", "id") != saved.ProjectID || textAt(current, "team", "id") != saved.Route.TeamID {
		return errors.New("Linear route changed; session retained")
	}
	state := textAt(current, "state", "name")
	switch state {
	case "Done", "Canceled", "Duplicate":
	default:
		fmt.Fprintf(r.out, "%s: skipped; current Linear status %q\n", identifier, state)
		return nil
	}
	args := []string{saved.Executable, "delete", "--force", session}
	if saved.Engine == "opencode" {
		args = []string{saved.Executable, "session", "delete", session}
	}
	if dryRun {
		fmt.Fprintf(r.out, "%s: would delete %s session %s (%s)\n", identifier, saved.Engine, session, state)
		return nil
	}
	if _, err = r.command(saved.Checkout, args...); err != nil {
		return err
	}
	if err = writeJSON(marker, map[string]any{"engine": saved.Engine, "session_id": session, "linear_state": state}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s: deleted %s session %s (%s)\n", identifier, saved.Engine, session, state)
	return nil
}
