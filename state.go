package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

func openState(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS runs (issue_id TEXT PRIMARY KEY, identifier TEXT, status TEXT, folder TEXT, pr_url TEXT)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS provider_cooldowns (provider TEXT PRIMARY KEY, retry_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, errors.New("state database is newer than this runner")
	}
	if version == 0 {
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			return nil, err
		}
		if _, err = tx.Exec(`CREATE TABLE run_context (issue_id TEXT PRIMARY KEY, context TEXT NOT NULL)`); err == nil {
			_, err = tx.Exec("PRAGMA user_version=1")
		}
		if err != nil {
			tx.Rollback()
			db.Close()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}
func claimedStatus(db *sql.DB, id string) (string, error) {
	var status string
	err := db.QueryRow("SELECT status FROM runs WHERE issue_id=?", id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, err
}
func printStatus(db *sql.DB, out io.Writer) error {
	rows, err := db.Query("SELECT identifier,status,folder,pr_url FROM runs")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status, folder, url string
		if err = rows.Scan(&id, &status, &folder, &url); err != nil {
			return err
		}
		if err = json.NewEncoder(out).Encode([]string{id, status, folder, url}); err != nil {
			return err
		}
		var activity map[string]any
		if err = readJSON(filepath.Join(folder, "activity.json"), &activity); err == nil {
			activity["identifier"] = id
			for _, name := range []string{"events.jsonl", "stderr.log"} {
				if info, e := os.Stat(filepath.Join(folder, name)); e == nil {
					previous, _ := time.Parse(time.RFC3339Nano, textAt(activity, "last_activity"))
					if info.ModTime().After(previous) {
						activity["last_activity"] = info.ModTime().UTC().Format(time.RFC3339Nano)
					}
				}
			}
			if err = json.NewEncoder(out).Encode(activity); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return rows.Err()
}

func (r *runner) readStatus(runs string) error {
	active := false
	lock, err := os.Open(filepath.Join(runs, "worker.lock"))
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		active = errors.Is(err, syscall.EWOULDBLOCK)
		lock.Close()
		if err != nil && !active {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = json.NewEncoder(r.out).Encode(map[string]any{"worker_active": active}); err != nil {
		return err
	}
	path := filepath.Join(runs, "state.sqlite3")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err = r.printPollHealth(runs, active); err != nil {
			return err
		}
		fmt.Fprintln(r.out, "No recorded runs")
		return nil
	} else if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	if err = r.printPollHealth(runs, active); err != nil {
		return err
	}
	var blocked, failed, interrupted int
	if err = db.QueryRow("SELECT COALESCE(SUM(status='blocked'),0),COALESCE(SUM(status='failed'),0),COALESCE(SUM(status='running'),0) FROM runs").Scan(&blocked, &failed, &interrupted); err != nil {
		return err
	}
	if active {
		interrupted = 0
	}
	if err = json.NewEncoder(r.out).Encode(map[string]any{"attention_required": blocked+failed+interrupted > 0, "blocked_runs": blocked, "failed_runs": failed, "interrupted_runs": interrupted}); err != nil {
		return err
	}
	return printStatus(db, r.out)
}

func recordActivity(folder, phase string) error {
	path := filepath.Join(folder, "activity.json")
	var activity map[string]any
	if err := readJSON(path, &activity); os.IsNotExist(err) {
		host, err := os.Hostname()
		if err != nil {
			return err
		}
		activity = map[string]any{"started_at": time.Now().UTC().Format(time.RFC3339Nano), "pid": os.Getpid(), "hostname": host}
	} else if err != nil {
		return err
	}
	activity["phase"], activity["last_activity"] = phase, time.Now().UTC().Format(time.RFC3339Nano)
	return writeJSON(path, activity)
}
