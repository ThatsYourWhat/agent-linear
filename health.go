package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const pollStaleAfter = 15 * time.Minute

type pollHealth struct {
	LastStarted  time.Time `json:"last_started,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	LastFinished time.Time `json:"last_finished,omitempty"`
	Outcome      string    `json:"outcome"`
	LastError    string    `json:"last_error,omitempty"`
}

func beginPoll(runs string) (pollHealth, error) {
	var health pollHealth
	path := filepath.Join(runs, "poll-health.json")
	if err := readJSON(path, &health); err != nil && !os.IsNotExist(err) {
		return health, err
	}
	health.LastStarted = time.Now().UTC()
	health.Outcome = "running"
	return health, writeJSON(path, health)
}

func finishPoll(runs string, health pollHealth, outcome, phase string, runErr error) error {
	health.LastFinished = time.Now().UTC()
	health.Outcome = outcome
	if runErr != nil {
		health.Outcome = "failed"
		// External command errors can contain credentials; retain only our execution phase.
		health.LastError = "poll failed during " + phase + "; inspect cron.log"
	} else {
		health.LastError = ""
		if outcome == "success" {
			health.LastSuccess = health.LastFinished
		}
	}
	return writeJSON(filepath.Join(runs, "poll-health.json"), health)
}

func (r *runner) printPollHealth(runs string, active bool) error {
	var attempt struct {
		LastAttempt time.Time `json:"last_attempt"`
	}
	if err := readJSON(filepath.Join(runs, "poll-attempt.json"), &attempt); err != nil && !os.IsNotExist(err) {
		return err
	}
	var health pollHealth
	if err := readJSON(filepath.Join(runs, "poll-health.json"), &health); err != nil && !os.IsNotExist(err) {
		return err
	}
	stale := attempt.LastAttempt.IsZero() || time.Since(attempt.LastAttempt) > pollStaleAfter
	warnings := []string{}
	if stale {
		warnings = append(warnings, "polling_stale")
	}
	if health.Outcome == "failed" {
		warnings = append(warnings, "poll_failed")
	}
	if health.Outcome == "running" && !active {
		warnings = append(warnings, "poll_interrupted")
	}
	if health.Outcome == "deferred" {
		warnings = append(warnings, "provider_deferred")
	}
	if health.Outcome == "" {
		health.Outcome = "never_polled"
	}
	value := map[string]any{"poll_health": health, "last_poll_attempt": attempt.LastAttempt, "polling_stale": stale, "last_success_stale": health.LastSuccess.IsZero() || time.Since(health.LastSuccess) > pollStaleAfter, "warnings": warnings}
	return json.NewEncoder(r.out).Encode(value)
}
