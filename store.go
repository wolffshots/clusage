package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const keychainService = "clusage"

type Config struct {
	Model            string `json:"model"`
	ThresholdMinutes int    `json:"threshold_minutes"`
	// FetchCron is one or more 5-field cron expressions separated by ";".
	// Empty disables the TUI auto-fetch.
	FetchCron string `json:"fetch_cron"`
	// HistoryHours is how far back the history graphs read. 0 uses the default.
	HistoryHours int `json:"history_hours"`
}

var defaultConfig = Config{
	Model:            "claude-opus-5",
	ThresholdMinutes: 5,
	FetchCron:        "*/15 * * * *",
	HistoryHours:     168,
}

// configDir returns ~/.config/clusage, honoring XDG_CONFIG_HOME.
func configDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "clusage")
	return dir, os.MkdirAll(dir, 0o700)
}

// loadConfig reads the config file, writing the defaults first if it is missing.
func loadConfig() (Config, string, error) {
	dir, err := configDir()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		out, _ := json.MarshalIndent(defaultConfig, "", "  ")
		if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
			return Config{}, path, err
		}
		return defaultConfig, path, nil
	}
	if err != nil {
		return Config{}, path, err
	}
	cfg := defaultConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Model == "" {
		cfg.Model = defaultConfig.Model
	}
	if cfg.ThresholdMinutes <= 0 {
		cfg.ThresholdMinutes = defaultConfig.ThresholdMinutes
	}
	if cfg.HistoryHours <= 0 {
		cfg.HistoryHours = defaultConfig.HistoryHours
	}
	return cfg, path, nil
}

// saveToken stores the OAuth token in the login keychain.
func saveToken(token string) error {
	cmd := exec.Command("security", "add-generic-password",
		"-a", os.Getenv("USER"), "-s", keychainService, "-w", token, "-U")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain write failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func loadToken() (string, error) {
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", keychainService, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("no token stored, run: clusage setup")
	}
	return strings.TrimSpace(string(out)), nil
}

type Reading struct {
	FetchedAt time.Time
	Model     string
	Headers   map[string]string
}

func openDB() (*sql.DB, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "clusage.db"))
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS readings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		fetched_at TEXT NOT NULL,
		model TEXT NOT NULL,
		headers TEXT NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func saveReading(db *sql.DB, r Reading) error {
	blob, err := json.Marshal(r.Headers)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO readings (fetched_at, model, headers) VALUES (?, ?, ?)`,
		r.FetchedAt.UTC().Format(time.RFC3339Nano), r.Model, string(blob))
	return err
}

// latestReading returns the newest cached reading, or ok=false when the table is empty.
func latestReading(db *sql.DB) (Reading, bool, error) {
	var ts, model, blob string
	err := db.QueryRow(`SELECT fetched_at, model, headers FROM readings ORDER BY id DESC LIMIT 1`).
		Scan(&ts, &model, &blob)
	if err == sql.ErrNoRows {
		return Reading{}, false, nil
	}
	if err != nil {
		return Reading{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Reading{}, false, err
	}
	r := Reading{FetchedAt: at, Model: model}
	if err := json.Unmarshal([]byte(blob), &r.Headers); err != nil {
		return Reading{}, false, err
	}
	return r, true, nil
}

// readingsSince returns every reading fetched at or after since, oldest first,
// for the history graphs.
func readingsSince(db *sql.DB, since time.Time) ([]Reading, error) {
	rows, err := db.Query(
		`SELECT fetched_at, model, headers FROM readings WHERE fetched_at >= ? ORDER BY id`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var ts, model, blob string
		if err := rows.Scan(&ts, &model, &blob); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue // a row written by an older format; skip it rather than fail the view
		}
		r := Reading{FetchedAt: at, Model: model}
		if err := json.Unmarshal([]byte(blob), &r.Headers); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
