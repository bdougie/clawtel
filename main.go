// clawtel - local token telemetry for claw.tech
//
// SECURITY MODEL (read this first):
//
// clawtel reads four columns from your local Tapes SQLite database (nodes table):
//
//   created_at, model, prompt_tokens, completion_tokens
//
// It reads nothing else. No prompts. No responses. No tool calls.
// No session IDs. No file paths. No hostnames.
//
// The payload sent to claw.tech contains:
//
//   claw_id, window_start, window_end, model,
//   input_tokens (from prompt_tokens), output_tokens (from completion_tokens), message_count
//
// That is the complete list. You can verify this by reading send().
//
// clawtel runs only when CLAW_INGEST_KEY is set. Without the key,
// the binary exits immediately. No key, no network calls, ever.
//
// Source: https://github.com/papercomputeco/clawtel
// License: MIT

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ingestEndpoint = "https://ingest.claw.tech/v1/heartbeat"
	pollInterval   = 60 * time.Minute
	version        = "0.1.0"
)

// heartbeat is the complete payload sent to claw.tech.
// This struct is the source of truth for what leaves your machine.
type heartbeat struct {
	ClawID       string    `json:"claw_id"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	Model        string    `json:"model"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	MessageCount int64     `json:"message_count"`
}

// row is what clawtel reads from tapes.sqlite.
// Four columns from the nodes table. Nothing else is queried.
type row struct {
	createdAt        time.Time
	model            string
	promptTokens     int64
	completionTokens int64
}

func main() {
	log.SetPrefix("clawtel: ")
	log.SetFlags(0)

	ingestKey := os.Getenv("CLAW_INGEST_KEY")
	if ingestKey == "" {
		// Silent exit. No key = no telemetry. This is intentional.
		os.Exit(0)
	}

	clawID := os.Getenv("CLAW_ID")
	if clawID == "" {
		log.Fatal("CLAW_ID is required when CLAW_INGEST_KEY is set")
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		log.Fatal(err)
	}

	cursorPath := resolveCursorPath(dbPath)

	log.Printf("clawtel %s", version)
	log.Printf("db:     %s", dbPath)
	log.Printf("cursor: %s", cursorPath)
	log.Printf("claw:   %s", clawID)
	log.Printf("reads:  created_at, model, prompt_tokens, completion_tokens (from nodes table)")
	log.Printf("sends:  tokens + model counts only. no prompts. no responses.")

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Verify we can only see what we expect.
	if err := assertSchema(db); err != nil {
		log.Fatalf("schema check: %v", err)
	}

	cursor, err := loadCursor(cursorPath)
	if err != nil {
		log.Fatalf("load cursor: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 10 * time.Second}

	log.Printf("polling every %s", pollInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
			newCursor, err := poll(db, client, ingestKey, clawID, cursor)
			if err != nil {
				log.Printf("poll error: %v", err)
				continue
			}
			if newCursor.After(cursor) {
				cursor = newCursor
				if err := saveCursor(cursorPath, cursor); err != nil {
					log.Printf("save cursor: %v", err)
				}
			}
		}
	}
}

// poll reads new rows since cursor, aggregates, and sends a heartbeat.
// A heartbeat is always sent — even with zero new rows — so that
// claw.tech can distinguish "online but idle" from "offline".
// Returns the new cursor timestamp on success.
func poll(db *sql.DB, client *http.Client, ingestKey, clawID string, cursor time.Time) (time.Time, error) {
	return pollWithURL(db, client, ingestEndpoint, ingestKey, clawID, cursor)
}

// pollWithURL is the testable version of poll that accepts a custom endpoint URL.
func pollWithURL(db *sql.DB, client *http.Client, url, ingestKey, clawID string, cursor time.Time) (time.Time, error) {
	windowStart := cursor
	windowEnd := time.Now().UTC()

	rows, err := readRows(db, cursor)
	if err != nil {
		return cursor, fmt.Errorf("read: %v", err)
	}

	hb := aggregate(clawID, windowStart, windowEnd, rows)

	if err := sendToURL(client, url, ingestKey, hb); err != nil {
		return cursor, fmt.Errorf("send: %v", err)
	}

	if len(rows) > 0 {
		log.Printf("sent: %d turns, %d in, %d out, model=%s",
			hb.MessageCount, hb.InputTokens, hb.OutputTokens, hb.Model)
	}

	return windowEnd, nil
}

// readRows queries ONLY these four columns from the nodes table.
// This is the complete read surface.
func readRows(db *sql.DB, since time.Time) ([]row, error) {
	const query = `
		SELECT
			created_at,
			model,
			prompt_tokens,
			completion_tokens
		FROM nodes
		WHERE created_at > ?
		ORDER BY created_at ASC
	`

	sqlRows, err := db.Query(query, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()

	var out []row
	for sqlRows.Next() {
		var r row
		var createdAtStr string
		if err := sqlRows.Scan(&createdAtStr, &r.model, &r.promptTokens, &r.completionTokens); err != nil {
			return nil, err
		}
		r.createdAt, err = time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %v", err)
		}
		out = append(out, r)
	}
	return out, sqlRows.Err()
}

// aggregate builds the heartbeat payload from raw rows.
// MessageCount is the number of API turns (nodes) in the window, not chat messages.
// Model is the most-used model in the window.
func aggregate(clawID string, start, end time.Time, rows []row) heartbeat {
	var totalIn, totalOut int64
	modelCounts := map[string]int64{}

	for _, r := range rows {
		totalIn += r.promptTokens
		totalOut += r.completionTokens
		modelCounts[r.model]++
	}

	dominantModel := ""
	var maxCount int64
	for m, c := range modelCounts {
		if c > maxCount {
			dominantModel = m
			maxCount = c
		}
	}

	return heartbeat{
		ClawID:       clawID,
		WindowStart:  start,
		WindowEnd:    end,
		Model:        dominantModel,
		InputTokens:  totalIn,
		OutputTokens: totalOut,
		MessageCount: int64(len(rows)),
	}
}

// send posts the heartbeat to claw.tech.
// The JSON payload matches the heartbeat struct exactly.
// Inspect the struct above to verify what is sent.
func send(client *http.Client, ingestKey string, hb heartbeat) error {
	return sendToURL(client, ingestEndpoint, ingestKey, hb)
}

// sendToURL posts a heartbeat to the given URL. Extracted for testability.
func sendToURL(client *http.Client, url, ingestKey string, hb heartbeat) error {
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ingestKey)
	req.Header.Set("User-Agent", "clawtel/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("ingest returned %d", resp.StatusCode)
	}
	return nil
}

// assertSchema verifies:
//  1. The nodes table exists
//  2. The four columns clawtel reads are present
//  3. Sensitive columns (prompts, responses, project names) are flagged
//
// This is a safety check, not just documentation.
func assertSchema(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(nodes)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Columns clawtel actually reads — all must exist.
	required := map[string]bool{
		"created_at":        false,
		"model":             false,
		"prompt_tokens":     false,
		"completion_tokens": false,
	}

	// Columns that contain user content or identify projects.
	// clawtel never reads these, but users deserve to know they exist in the DB.
	sensitive := map[string]bool{
		"content":    true, // actual message text
		"bucket":     true, // raw API call JSON
		"project":    true, // git repo / project name
		"agent_name": true, // harness identifier
	}

	var columns []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return err
		}
		columns = append(columns, name)
	}

	if len(columns) == 0 {
		return fmt.Errorf("nodes table not found in database")
	}

	// Mark required columns as found.
	for _, col := range columns {
		if _, ok := required[col]; ok {
			required[col] = true
		}
	}

	// Fail if any required column is missing.
	for col, found := range required {
		if !found {
			return fmt.Errorf("nodes table is missing required column %q", col)
		}
	}

	// Warn about sensitive columns. clawtel never queries them.
	for _, col := range columns {
		if sensitive[col] {
			log.Printf("NOTE: nodes table has column %q — clawtel does NOT read it", col)
		}
	}

	return rows.Err()
}

// resolveDBPath finds tapes.sqlite using a priority chain:
//  1. TAPES_DB env var (explicit override)
//  2. .mb/tapes/tapes.sqlite (openclaw-in-a-box layout)
//  3. ~/.tapes/tapes.sqlite (standalone tapes install)
func resolveDBPath() (string, error) {
	if p := os.Getenv("TAPES_DB"); p != "" {
		return p, nil
	}

	mbPath := ".mb/tapes/tapes.sqlite"
	if _, err := os.Stat(mbPath); err == nil {
		return mbPath, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find home dir: %v", err)
	}
	tapesPath := filepath.Join(home, ".tapes", "tapes.sqlite")
	if _, err := os.Stat(tapesPath); err == nil {
		return tapesPath, nil
	}

	return "", fmt.Errorf(
		"tapes.sqlite not found at .mb/tapes/ or ~/.tapes/. Set TAPES_DB to override",
	)
}

// resolveCursorPath stores cursor state next to the DB it tracks.
func resolveCursorPath(dbPath string) string {
	dir := filepath.Dir(dbPath)
	cursorDir := filepath.Join(dir, "clawtel")
	_ = os.MkdirAll(cursorDir, 0700)
	return filepath.Join(cursorDir, "cursor")
}

func loadCursor(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// First run. Start from now, not from the beginning of history.
		return time.Now().UTC(), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(data))
}

func saveCursor(path string, t time.Time) error {
	return os.WriteFile(path, []byte(t.UTC().Format(time.RFC3339Nano)), 0600)
}
