package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"

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
	}
	return rows.Err()
}
