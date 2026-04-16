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
// When CLAWTEL_CLAWHUB_LOCKS is set, clawtel ALSO reads
// .clawhub/lock.json files at the configured paths. From each file it reads
// only `version` (top-level, must be 1) and `skills.<slug>.version`.
// It NEVER reads `installedAt` or any other field. It NEVER reads SKILL.md
// content from disk.
//
// The payload sent to claw.tech contains:
//
//   claw_id, window_start, window_end, model,
//   input_tokens (from prompt_tokens), output_tokens (from completion_tokens),
//   message_count, and optionally clawhub_skills (slug + version per skill).
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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ingestEndpoint = "https://ingest.claw.tech/v1/heartbeat"
	pollInterval   = 5 * time.Minute
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

	// ClawhubSkills lists clawhub-installed skills discovered from
	// CLAWTEL_CLAWHUB_LOCKS lock files. Optional. Omitted when unchanged
	// since the last successful send so the server keeps last-known state.
	// Each entry contains ONLY slug and version. No paths, timestamps, or
	// content from the lock file are transmitted.
	ClawhubSkills []skill `json:"clawhub_skills,omitempty"`
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

	lockPaths := parseLockPaths(os.Getenv("CLAWTEL_CLAWHUB_LOCKS"))

	log.Printf("clawtel %s", version)
	log.Printf("db:     %s", dbPath)
	log.Printf("cursor: %s", cursorPath)
	log.Printf("claw:   %s", clawID)
	log.Printf("reads:  created_at, model, prompt_tokens, completion_tokens (from nodes table)")
	log.Printf("sends:  tokens + model counts only. no prompts. no responses.")
	if len(lockPaths) > 0 {
		log.Printf("clawhub locks: %d paths configured", len(lockPaths))
		log.Printf("clawhub:  reads lock.json fields: version, skills.<slug>.version (nothing else)")
		log.Printf("clawhub:  NOTE: lock.json contains \"installedAt\" timestamp, clawtel does NOT read it")
	}

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

	var lastSkillsHash string

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
			newCursor, newHash, err := poll(db, client, ingestKey, clawID, cursor, lockPaths, lastSkillsHash)
			if err != nil {
				log.Printf("poll error: %v", err)
				continue
			}
			lastSkillsHash = newHash
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
// Returns the new cursor timestamp and the new skills hash on success.
func poll(db *sql.DB, client *http.Client, ingestKey, clawID string, cursor time.Time, lockPaths []string, lastSkillsHash string) (time.Time, string, error) {
	return pollWithURL(db, client, ingestEndpoint, ingestKey, clawID, cursor, lockPaths, lastSkillsHash)
}

// pollWithURL is the testable version of poll that accepts a custom endpoint URL.
//
// One heartbeat is sent per model present in the window (see aggregate).
// Skills are attached to the first heartbeat only so the server sees the
// current skill set exactly once per poll, not once per model.
//
// On send failure the loop stops immediately, returning the original cursor
// and skills hash. The next poll will re-read rows since the cursor and
// retry the whole window — no partial progress is persisted.
func pollWithURL(db *sql.DB, client *http.Client, url, ingestKey, clawID string, cursor time.Time, lockPaths []string, lastSkillsHash string) (time.Time, string, error) {
	windowStart := cursor
	windowEnd := time.Now().UTC()

	rows, err := readRows(db, cursor)
	if err != nil {
		return cursor, lastSkillsHash, fmt.Errorf("read: %v", err)
	}

	hbs := aggregate(clawID, windowStart, windowEnd, rows)

	skills := loadSkills(lockPaths)
	newHash := hashSkills(skills)
	if newHash != lastSkillsHash && len(hbs) > 0 {
		hbs[0].ClawhubSkills = skills
	}

	for _, hb := range hbs {
		if err := sendToURL(client, url, ingestKey, hb); err != nil {
			return cursor, lastSkillsHash, fmt.Errorf("send: %v", err)
		}
		if hb.MessageCount > 0 {
			log.Printf("sent: %d turns, %d in, %d out, model=%s",
				hb.MessageCount, hb.InputTokens, hb.OutputTokens, hb.Model)
		}
		if len(hb.ClawhubSkills) > 0 {
			log.Printf("sent: %d clawhub skills (hash changed)", len(hb.ClawhubSkills))
		}
	}

	return windowEnd, newHash, nil
}

// readRows queries ONLY these four columns from the nodes table.
// This is the complete read surface.
//
// Both sides of the timestamp comparison are wrapped in SQLite's datetime()
// so tapes' on-disk format (space separator, numeric offset) and clawtel's
// RFC3339Nano cursor compare correctly regardless of textual representation.
// See issue #4 for the silent-zero-rows bug this prevents.
func readRows(db *sql.DB, since time.Time) ([]row, error) {
	const query = `
		SELECT
			created_at,
			model,
			prompt_tokens,
			completion_tokens
		FROM nodes
		WHERE datetime(created_at) > datetime(?)
		ORDER BY datetime(created_at) ASC
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
		// prompt_tokens and completion_tokens are nullable in tapes: partial
		// request rows and mid-stream delta rows carry NULL until the turn
		// completes. Scanning into sql.NullInt64 lets a single NULL row live
		// in the window without poisoning the whole scan. NULL coerces to 0
		// so the row still contributes its message_count to the heartbeat.
		// See issue #8.
		var pt, ct sql.NullInt64
		if err := sqlRows.Scan(&createdAtStr, &r.model, &pt, &ct); err != nil {
			return nil, err
		}
		r.promptTokens = pt.Int64
		r.completionTokens = ct.Int64
		r.createdAt, err = parseCreatedAt(createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %v", err)
		}
		out = append(out, r)
	}
	return out, sqlRows.Err()
}

// parseCreatedAt accepts the three timestamp shapes clawtel can encounter:
//
//  1. RFC3339Nano ("T" separator, Z or numeric offset) — clawtel's own
//     cursor file format, and anything that round-trips through Go.
//  2. "YYYY-MM-DD HH:MM:SS[.fff][±HH:MM]" — tapes' on-disk format.
//     The ".fff" and offset are both optional during parsing.
//  3. "YYYY-MM-DD HH:MM:SS" — SQLite's plain CURRENT_TIMESTAMP output,
//     always UTC per SQLite docs.
//
// Go's time.Parse accepts a missing fractional even when the layout
// contains one, so a single layout covers both fractional and no-fractional
// inputs for each separator/offset combination.
func parseCreatedAt(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
	}
	var firstErr error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return time.Time{}, firstErr
}

// aggregate builds one heartbeat per model in the window.
// MessageCount is the number of API turns (nodes) attributed to that model.
//
// Returns heartbeats sorted by Model so the loop order is deterministic
// (skills are attached to the first one). When rows is empty a single
// presence-ping heartbeat with empty Model and zero tokens is returned,
// so claw.tech can distinguish "online but idle" from "offline".
//
// Before issue #7 this function collapsed all rows in a window into a single
// heartbeat labeled with whichever model had the most rows. Multi-model claws
// (e.g. opus for chat + haiku for heartbeat + sonnet for cron) showed up as
// single-model on the leaderboard. Splitting by model preserves per-model
// token counts and makes the model-distribution chart correct.
func aggregate(clawID string, start, end time.Time, rows []row) []heartbeat {
	if len(rows) == 0 {
		return []heartbeat{{
			ClawID:      clawID,
			WindowStart: start,
			WindowEnd:   end,
		}}
	}

	buckets := map[string]*heartbeat{}
	for _, r := range rows {
		hb, ok := buckets[r.model]
		if !ok {
			hb = &heartbeat{
				ClawID:      clawID,
				WindowStart: start,
				WindowEnd:   end,
				Model:       r.model,
			}
			buckets[r.model] = hb
		}
		hb.InputTokens += r.promptTokens
		hb.OutputTokens += r.completionTokens
		hb.MessageCount++
	}

	out := make([]heartbeat, 0, len(buckets))
	for _, hb := range buckets {
		out = append(out, *hb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
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

// skill is the per-skill payload sent to claw.tech for clawhub-installed skills.
// Only slug and version. Never installedAt, paths, or any other metadata.
type skill struct {
	Slug    string `json:"slug"`
	Version string `json:"version"`
}

// lockFile is the on-disk shape of <workdir>/.clawhub/lock.json.
// Only the version field per skill is read.
type lockFile struct {
	Version int                      `json:"version"`
	Skills  map[string]lockFileEntry `json:"skills"`
}

// lockFileEntry intentionally omits installedAt — clawtel never reads it.
type lockFileEntry struct {
	Version string `json:"version"`
}

// parseLockFile reads one .clawhub/lock.json and returns slug -> version.
// Returns an error on missing file, malformed JSON, or unsupported lock version.
func parseLockFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf lockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parse %s: %v", path, err)
	}
	if lf.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported lock version %d (want 1)", path, lf.Version)
	}
	out := make(map[string]string, len(lf.Skills))
	for slug, entry := range lf.Skills {
		out[slug] = entry.Version
	}
	return out, nil
}

// compareSemver returns -1 if a<b, 0 if a==b, 1 if a>b.
// Compares dotted components numerically when possible, lexically otherwise.
// Missing trailing components are treated as 0 ("1.0" == "1.0.0").
func compareSemver(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		ap := "0"
		bp := "0"
		if i < len(aParts) {
			ap = aParts[i]
		}
		if i < len(bParts) {
			bp = bParts[i]
		}
		ai, aErr := strconv.Atoi(ap)
		bi, bErr := strconv.Atoi(bp)
		if aErr == nil && bErr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		if ap != bp {
			if ap < bp {
				return -1
			}
			return 1
		}
	}
	return 0
}

// dedupeSkills merges multiple slug->version maps into a sorted []skill.
// On version conflict for the same slug, keeps the highest semver and logs.
// Always returns a non-nil slice so callers can safely len() and json-marshal.
func dedupeSkills(maps []map[string]string) []skill {
	merged := map[string]string{}
	for _, m := range maps {
		for slug, version := range m {
			cur, exists := merged[slug]
			if !exists {
				merged[slug] = version
				continue
			}
			cmp := compareSemver(version, cur)
			if cmp > 0 {
				log.Printf("clawhub skill %q: keeping %s over %s (higher semver)", slug, version, cur)
				merged[slug] = version
			} else if cmp < 0 {
				log.Printf("clawhub skill %q: keeping %s over %s (higher semver)", slug, cur, version)
			}
		}
	}
	out := make([]skill, 0, len(merged))
	for slug, version := range merged {
		out = append(out, skill{Slug: slug, Version: version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// loadSkills reads every configured lock.json, dedupes, and returns a sorted slice.
// Missing or malformed files are logged and skipped — the heartbeat loop must
// never fail because a lock file is bad. Always returns a non-nil slice.
func loadSkills(paths []string) []skill {
	maps := make([]map[string]string, 0, len(paths))
	for _, p := range paths {
		m, err := parseLockFile(p)
		if err != nil {
			log.Printf("clawhub: skipping %s: %v", p, err)
			continue
		}
		maps = append(maps, m)
	}
	return dedupeSkills(maps)
}

// hashSkills returns a stable sha256 hex digest of the skills list.
// Sorts defensively so callers cannot break the stability contract.
// Used to detect whether the skills set changed since the last sent heartbeat.
func hashSkills(skills []skill) string {
	cp := make([]skill, len(skills))
	copy(cp, skills)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Slug < cp[j].Slug })
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// parseLockPaths splits CLAWTEL_CLAWHUB_LOCKS on commas, trims whitespace,
// and drops empty entries. Returns nil for an empty/unset value.
func parseLockPaths(env string) []string {
	if env == "" {
		return nil
	}
	parts := strings.Split(env, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
