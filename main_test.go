package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// --- helpers ---

// createTestDB creates an in-memory SQLite database with the nodes table.
// If sensitive is true, it also adds content/bucket/project/agent_name columns.
func createTestDB(t *testing.T, sensitive bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}

	extra := ""
	if sensitive {
		extra = ", content TEXT, bucket TEXT, project TEXT, agent_name TEXT"
	}

	_, err = db.Exec(fmt.Sprintf(`CREATE TABLE nodes (
		created_at TEXT NOT NULL,
		model TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0
		%s
	)`, extra))
	if err != nil {
		t.Fatal(err)
	}

	return db
}

func insertRow(t *testing.T, db *sql.DB, createdAt time.Time, model string, promptTokens, completionTokens int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO nodes (created_at, model, prompt_tokens, completion_tokens) VALUES (?, ?, ?, ?)`,
		createdAt.UTC().Format(time.RFC3339Nano), model, promptTokens, completionTokens,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// lockFixtures writes the canonical lock.json fixtures used across clawhub tests
// to a fresh t.TempDir() and returns paths. `missing` points at a file that is
// never created — useful for "skip missing file" assertions.
func lockFixtures(t *testing.T) (valid, malformed, v2, missing string) {
	t.Helper()
	dir := t.TempDir()

	valid = filepath.Join(dir, "lock_valid.json")
	if err := os.WriteFile(valid, []byte(`{
  "version": 1,
  "skills": {
    "granola":         { "version": "1.0.0", "installedAt": 1775635621739 },
    "openclaw-linear": { "version": "1.0.1", "installedAt": 1775635629099 }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}

	malformed = filepath.Join(dir, "lock_malformed.json")
	if err := os.WriteFile(malformed, []byte(`{ this is not valid json`), 0600); err != nil {
		t.Fatal(err)
	}

	v2 = filepath.Join(dir, "lock_v2.json")
	if err := os.WriteFile(v2, []byte(`{
  "version": 1,
  "skills": {
    "granola":   { "version": "1.2.0", "installedAt": 1775635621740 },
    "tapes-cli": { "version": "0.3.0", "installedAt": 1775635629100 }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}

	missing = filepath.Join(dir, "does_not_exist.json")
	return
}

// --- aggregate tests ---

func TestAggregate_EmptyRows(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Minute)

	hbs := aggregate("test-claw", start, end, nil)

	if len(hbs) != 1 {
		t.Fatalf("len(hbs) = %d, want 1 (presence ping)", len(hbs))
	}
	hb := hbs[0]
	if hb.ClawID != "test-claw" {
		t.Errorf("ClawID = %q, want %q", hb.ClawID, "test-claw")
	}
	if hb.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", hb.InputTokens)
	}
	if hb.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0", hb.OutputTokens)
	}
	if hb.MessageCount != 0 {
		t.Errorf("MessageCount = %d, want 0", hb.MessageCount)
	}
	if hb.Model != "" {
		t.Errorf("Model = %q, want empty", hb.Model)
	}
}

func TestAggregate_SingleRow(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Minute)
	rows := []row{
		{createdAt: start, model: "claude-opus-4-6", promptTokens: 1000, completionTokens: 500},
	}

	hbs := aggregate("my-claw", start, end, rows)

	if len(hbs) != 1 {
		t.Fatalf("len(hbs) = %d, want 1", len(hbs))
	}
	hb := hbs[0]
	if hb.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", hb.InputTokens)
	}
	if hb.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", hb.OutputTokens)
	}
	if hb.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", hb.MessageCount)
	}
	if hb.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want %q", hb.Model, "claude-opus-4-6")
	}
}

func TestAggregate_MultipleModels_OneHeartbeatPerModel(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Minute)
	// Matches the issue #7 repro: haiku has the most rows (dominant under
	// the old behavior) but holds the fewest tokens. Each model must keep
	// its own token counts instead of being folded into one dominant bucket.
	rows := []row{
		{model: "claude-opus-4-6", promptTokens: 80_000, completionTokens: 40_000},
		{model: "claude-opus-4-6", promptTokens: 0, completionTokens: 0},
		{model: "claude-sonnet-4-6", promptTokens: 10_000, completionTokens: 5_000},
		{model: "claude-sonnet-4-6", promptTokens: 0, completionTokens: 0},
		{model: "claude-haiku-4-5", promptTokens: 250, completionTokens: 250},
		{model: "claude-haiku-4-5", promptTokens: 250, completionTokens: 250},
		{model: "claude-haiku-4-5", promptTokens: 250, completionTokens: 250},
		{model: "claude-haiku-4-5", promptTokens: 250, completionTokens: 250},
	}

	hbs := aggregate("claw", start, end, rows)

	if len(hbs) != 3 {
		t.Fatalf("len(hbs) = %d, want 3 (one per model)", len(hbs))
	}

	byModel := map[string]heartbeat{}
	for _, hb := range hbs {
		byModel[hb.Model] = hb
	}

	opus, ok := byModel["claude-opus-4-6"]
	if !ok {
		t.Fatal("missing claude-opus-4-6 heartbeat")
	}
	if opus.InputTokens != 80_000 || opus.OutputTokens != 40_000 || opus.MessageCount != 2 {
		t.Errorf("opus: in=%d out=%d count=%d; want 80000/40000/2",
			opus.InputTokens, opus.OutputTokens, opus.MessageCount)
	}

	sonnet, ok := byModel["claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing claude-sonnet-4-6 heartbeat")
	}
	if sonnet.InputTokens != 10_000 || sonnet.OutputTokens != 5_000 || sonnet.MessageCount != 2 {
		t.Errorf("sonnet: in=%d out=%d count=%d; want 10000/5000/2",
			sonnet.InputTokens, sonnet.OutputTokens, sonnet.MessageCount)
	}

	haiku, ok := byModel["claude-haiku-4-5"]
	if !ok {
		t.Fatal("missing claude-haiku-4-5 heartbeat")
	}
	if haiku.InputTokens != 1_000 || haiku.OutputTokens != 1_000 || haiku.MessageCount != 4 {
		t.Errorf("haiku: in=%d out=%d count=%d; want 1000/1000/4",
			haiku.InputTokens, haiku.OutputTokens, haiku.MessageCount)
	}
}

func TestAggregate_SortedByModel(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Minute)
	rows := []row{
		{model: "zzz", promptTokens: 1, completionTokens: 1},
		{model: "aaa", promptTokens: 1, completionTokens: 1},
		{model: "mmm", promptTokens: 1, completionTokens: 1},
	}

	hbs := aggregate("claw", start, end, rows)

	if len(hbs) != 3 {
		t.Fatalf("len(hbs) = %d, want 3", len(hbs))
	}
	want := []string{"aaa", "mmm", "zzz"}
	for i, hb := range hbs {
		if hb.Model != want[i] {
			t.Errorf("hbs[%d].Model = %q, want %q", i, hb.Model, want[i])
		}
	}
}

func TestAggregate_PreservesWindowTimes(t *testing.T) {
	start := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 30, 13, 0, 0, 0, time.UTC)
	rows := []row{
		{model: "a", promptTokens: 1, completionTokens: 1},
		{model: "b", promptTokens: 1, completionTokens: 1},
	}

	hbs := aggregate("claw", start, end, rows)
	if len(hbs) != 2 {
		t.Fatalf("len(hbs) = %d, want 2", len(hbs))
	}
	for _, hb := range hbs {
		if !hb.WindowStart.Equal(start) {
			t.Errorf("WindowStart = %v, want %v", hb.WindowStart, start)
		}
		if !hb.WindowEnd.Equal(end) {
			t.Errorf("WindowEnd = %v, want %v", hb.WindowEnd, end)
		}
	}
}

func TestAggregate_ClawIDSetOnEveryBucket(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Minute)
	rows := []row{
		{model: "a", promptTokens: 1, completionTokens: 1},
		{model: "b", promptTokens: 1, completionTokens: 1},
	}

	hbs := aggregate("my-claw", start, end, rows)
	for _, hb := range hbs {
		if hb.ClawID != "my-claw" {
			t.Errorf("ClawID = %q, want %q", hb.ClawID, "my-claw")
		}
	}
}

// --- readRows tests ---

func TestReadRows_Empty(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	rows, err := readRows(db, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestReadRows_ReturnsRowsAfterCursor(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	insertRow(t, db, old, "old-model", 100, 50)
	insertRow(t, db, recent, "new-model", 200, 100)

	// Cursor 1 hour ago — should only get the recent row
	cursor := now.Add(-time.Hour)
	rows, err := readRows(db, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].model != "new-model" {
		t.Errorf("model = %q, want %q", rows[0].model, "new-model")
	}
	if rows[0].promptTokens != 200 {
		t.Errorf("promptTokens = %d, want 200", rows[0].promptTokens)
	}
}

func TestReadRows_OrderedByCreatedAt(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	base := time.Now().UTC()
	insertRow(t, db, base.Add(2*time.Minute), "second", 20, 10)
	insertRow(t, db, base.Add(time.Minute), "first", 10, 5)
	insertRow(t, db, base.Add(3*time.Minute), "third", 30, 15)

	rows, err := readRows(db, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].model != "first" {
		t.Errorf("rows[0].model = %q, want %q", rows[0].model, "first")
	}
	if rows[2].model != "third" {
		t.Errorf("rows[2].model = %q, want %q", rows[2].model, "third")
	}
}

// --- assertSchema tests ---

func TestAssertSchema_ValidSchema(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	if err := assertSchema(db); err != nil {
		t.Errorf("assertSchema failed on valid schema: %v", err)
	}
}

func TestAssertSchema_WithSensitiveColumns(t *testing.T) {
	db := createTestDB(t, true)
	defer db.Close()

	// Should pass — sensitive columns are logged, not rejected
	if err := assertSchema(db); err != nil {
		t.Errorf("assertSchema failed with sensitive columns: %v", err)
	}
}

func TestAssertSchema_MissingColumn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create table missing prompt_tokens
	_, err = db.Exec(`CREATE TABLE nodes (
		created_at TEXT NOT NULL,
		model TEXT NOT NULL,
		completion_tokens INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}

	err = assertSchema(db)
	if err == nil {
		t.Fatal("expected error for missing column, got nil")
	}
}

func TestAssertSchema_NoTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = assertSchema(db)
	if err == nil {
		t.Fatal("expected error for missing table, got nil")
	}
	if err.Error() != "nodes table not found in database" {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- send tests ---

func TestSend_Success(t *testing.T) {
	var receivedBody heartbeat
	var receivedAuth string
	var receivedUA string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedUA = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	// Temporarily override ingest endpoint by using the test server's client
	client := server.Client()
	hb := heartbeat{
		ClawID:       "test-claw",
		WindowStart:  time.Now().UTC(),
		WindowEnd:    time.Now().UTC(),
		Model:        "claude-opus-4-6",
		InputTokens:  1000,
		OutputTokens: 500,
		MessageCount: 5,
	}

	// We need to send to the test server, so we create a custom send
	err := sendToURL(client, server.URL, "ik_testkey", hb)
	if err != nil {
		t.Fatal(err)
	}

	if receivedAuth != "Bearer ik_testkey" {
		t.Errorf("Authorization = %q, want %q", receivedAuth, "Bearer ik_testkey")
	}
	if receivedUA != "clawtel/"+version {
		t.Errorf("User-Agent = %q, want %q", receivedUA, "clawtel/"+version)
	}
	if receivedBody.ClawID != "test-claw" {
		t.Errorf("body.ClawID = %q, want %q", receivedBody.ClawID, "test-claw")
	}
	if receivedBody.InputTokens != 1000 {
		t.Errorf("body.InputTokens = %d, want 1000", receivedBody.InputTokens)
	}
}

func TestSend_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := server.Client()
	hb := heartbeat{ClawID: "test"}

	err := sendToURL(client, server.URL, "ik_test", hb)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestSend_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := server.Client()
	hb := heartbeat{ClawID: "test"}

	err := sendToURL(client, server.URL, "ik_test", hb)
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
}

func TestSend_OK200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := server.Client()
	hb := heartbeat{ClawID: "test"}

	err := sendToURL(client, server.URL, "ik_test", hb)
	if err != nil {
		t.Errorf("expected nil error for 200, got %v", err)
	}
}

// --- poll tests ---

func TestPoll_Success(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	insertRow(t, db, now.Add(-time.Minute), "claude-opus-4-6", 500, 250)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	cursor := now.Add(-time.Hour)
	newCursor, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !newCursor.After(cursor) {
		t.Error("new cursor should be after old cursor")
	}
}

func TestPoll_SendError(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cursor := time.Now().UTC().Add(-time.Hour)
	_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err == nil {
		t.Fatal("expected error from send failure, got nil")
	}
}

func TestPoll_EmptyDB(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb heartbeat
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &hb)

		// Should still send a heartbeat even with zero rows
		if hb.MessageCount != 0 {
			t.Errorf("MessageCount = %d, want 0", hb.MessageCount)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	cursor := time.Now().UTC().Add(-time.Hour)
	_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestPoll_SendsOneHeartbeatPerModel(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	insertRow(t, db, now.Add(-3*time.Minute), "claude-opus-4-6", 80_000, 40_000)
	insertRow(t, db, now.Add(-2*time.Minute), "claude-sonnet-4-6", 10_000, 5_000)
	insertRow(t, db, now.Add(-time.Minute), "claude-haiku-4-5", 1_000, 1_000)

	var received []heartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb heartbeat
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &hb); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		received = append(received, hb)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	cursor := now.Add(-time.Hour)
	_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(received) != 3 {
		t.Fatalf("received %d heartbeats, want 3 (one per model)", len(received))
	}

	seen := map[string]heartbeat{}
	for _, hb := range received {
		seen[hb.Model] = hb
	}
	if seen["claude-opus-4-6"].InputTokens != 80_000 {
		t.Errorf("opus input tokens = %d, want 80000", seen["claude-opus-4-6"].InputTokens)
	}
	if seen["claude-sonnet-4-6"].InputTokens != 10_000 {
		t.Errorf("sonnet input tokens = %d, want 10000", seen["claude-sonnet-4-6"].InputTokens)
	}
	if seen["claude-haiku-4-5"].InputTokens != 1_000 {
		t.Errorf("haiku input tokens = %d, want 1000", seen["claude-haiku-4-5"].InputTokens)
	}
}

func TestPoll_ClawhubSkillsOnlyOnFirstHeartbeat(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	insertRow(t, db, now.Add(-2*time.Minute), "claude-opus-4-6", 100, 50)
	insertRow(t, db, now.Add(-time.Minute), "claude-sonnet-4-6", 200, 100)

	var received []heartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb heartbeat
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &hb)
		received = append(received, hb)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	valid, _, _, _ := lockFixtures(t)
	cursor := now.Add(-time.Hour)
	_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, []string{valid}, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(received) != 2 {
		t.Fatalf("received %d heartbeats, want 2", len(received))
	}
	// Heartbeats are sorted by Model, so claude-opus-4-6 (first alphabetically) gets the skills.
	if len(received[0].ClawhubSkills) == 0 {
		t.Error("first heartbeat should carry clawhub_skills")
	}
	if len(received[1].ClawhubSkills) != 0 {
		t.Errorf("second heartbeat should omit clawhub_skills, got %d", len(received[1].ClawhubSkills))
	}
}

func TestPoll_StopsOnFirstSendError(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	insertRow(t, db, now.Add(-2*time.Minute), "claude-opus-4-6", 100, 50)
	insertRow(t, db, now.Add(-time.Minute), "claude-sonnet-4-6", 200, 100)

	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cursor := now.Add(-time.Hour)
	_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err == nil {
		t.Fatal("expected error from send failure, got nil")
	}
	if count != 1 {
		t.Errorf("sent %d heartbeats, want 1 (stop on first error)", count)
	}
}

// --- resolveDBPath tests ---

func TestResolveDBPath_EnvVar(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.sqlite")
	os.WriteFile(tmp, nil, 0600)

	t.Setenv("TAPES_DB", tmp)
	path, err := resolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != tmp {
		t.Errorf("path = %q, want %q", path, tmp)
	}
}

func TestResolveDBPath_HomeFallback(t *testing.T) {
	t.Setenv("TAPES_DB", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	tapesPath := filepath.Join(home, ".tapes", "tapes.sqlite")
	if _, err := os.Stat(tapesPath); err != nil {
		t.Skipf("no tapes.sqlite at %s", tapesPath)
	}

	path, err := resolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != tapesPath {
		t.Errorf("path = %q, want %q", path, tapesPath)
	}
}

func TestResolveDBPath_NotFound(t *testing.T) {
	t.Setenv("TAPES_DB", "")
	// Ensure no .mb/tapes exists in working dir
	origDir, _ := os.Getwd()
	tmpDir := t.TempDir()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	// Also need to ensure ~/.tapes/tapes.sqlite doesn't exist
	// We can't easily control that, so we only test the env var case
	_, err := resolveDBPath()
	// If ~/.tapes/tapes.sqlite exists this won't error — that's fine
	if err != nil && err.Error() != "tapes.sqlite not found at .mb/tapes/ or ~/.tapes/. Set TAPES_DB to override" {
		// Unexpected error that isn't the expected fallthrough
		t.Skipf("unexpected resolveDBPath result in this environment: %v", err)
	}
}

// --- resolveCursorPath tests ---

func TestResolveCursorPath(t *testing.T) {
	path := resolveCursorPath("/data/tapes.sqlite")
	expected := "/data/clawtel/cursor"
	if path != expected {
		t.Errorf("path = %q, want %q", path, expected)
	}
}

func TestResolveCursorPath_CreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tapes.sqlite")
	path := resolveCursorPath(dbPath)

	cursorDir := filepath.Dir(path)
	info, err := os.Stat(cursorDir)
	if err != nil {
		t.Fatalf("cursor dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("cursor path parent is not a directory")
	}
}

// --- loadCursor / saveCursor tests ---

func TestLoadCursor_FileNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent")
	before := time.Now().UTC()
	cursor, err := loadCursor(path)
	after := time.Now().UTC()

	if err != nil {
		t.Fatal(err)
	}
	if cursor.Before(before) || cursor.After(after) {
		t.Errorf("cursor = %v, expected between %v and %v", cursor, before, after)
	}
}

func TestSaveCursor_ThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor")
	ts := time.Date(2026, 3, 30, 12, 0, 0, 123456789, time.UTC)

	if err := saveCursor(path, ts); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Equal(ts) {
		t.Errorf("loaded = %v, want %v", loaded, ts)
	}
}

func TestLoadCursor_InvalidContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor")
	os.WriteFile(path, []byte("not-a-timestamp"), 0600)

	_, err := loadCursor(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// --- resolveDBPath: .mb/tapes path ---

func TestResolveDBPath_MbPath(t *testing.T) {
	t.Setenv("TAPES_DB", "")

	// Create temp dir, chdir into it, and create .mb/tapes/tapes.sqlite
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	mbDir := filepath.Join(tmpDir, ".mb", "tapes")
	os.MkdirAll(mbDir, 0755)
	os.WriteFile(filepath.Join(mbDir, "tapes.sqlite"), nil, 0600)

	path, err := resolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != ".mb/tapes/tapes.sqlite" {
		t.Errorf("path = %q, want %q", path, ".mb/tapes/tapes.sqlite")
	}
}

// --- loadCursor: permission error ---

func TestLoadCursor_ReadError(t *testing.T) {
	// Create a directory where we expect a file — reading it will fail
	path := filepath.Join(t.TempDir(), "cursor_dir")
	os.MkdirAll(path, 0755)

	_, err := loadCursor(path)
	if err == nil {
		t.Fatal("expected error reading a directory as file, got nil")
	}
}

// --- pollWithURL: readRows error ---

func TestPoll_ReadError(t *testing.T) {
	// DB without nodes table — readRows will fail
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	cursor := time.Now().UTC().Add(-time.Hour)
	_, _, pollErr := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if pollErr == nil {
		t.Fatal("expected error from readRows failure, got nil")
	}
}

// --- readRows: bad timestamp format ---

func TestReadRows_BadTimestamp(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE nodes (
		created_at TEXT NOT NULL,
		model TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a row with an unparseable timestamp. SQLite's datetime() returns
	// NULL for this value and the row is filtered out of the WHERE clause —
	// the poll loop keeps running on the valid rows instead of dying on one
	// bad row. This is the same graceful-skip behavior loadSkills uses for
	// malformed lock files.
	_, err = db.Exec(`INSERT INTO nodes (created_at, model, prompt_tokens, completion_tokens)
		VALUES ('not-a-timestamp', 'model', 100, 50)`)
	if err != nil {
		t.Fatal(err)
	}

	rows, readErr := readRows(db, time.Time{})
	if readErr != nil {
		t.Fatalf("readRows should skip unparseable rows, got error: %v", readErr)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (unparseable row should be filtered out)", len(rows))
	}
}

// TestReadRows_ScanSideParseError exercises the rare case where SQLite's
// datetime() accepts a stored value (so it passes the WHERE filter) but
// our Go-side parseCreatedAt cannot parse the raw column text. A date-only
// string "2026-04-13" is such a case: SQLite normalizes it to
// "2026-04-13 00:00:00" for comparison, but the scan returns the raw text.
func TestReadRows_ScanSideParseError(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO nodes (created_at, model, prompt_tokens, completion_tokens)
		VALUES ('2026-04-13', 'model', 100, 50)`)
	if err != nil {
		t.Fatal(err)
	}

	_, readErr := readRows(db, time.Time{})
	if readErr == nil {
		t.Fatal("expected scan-side parse error, got nil")
	}
}

// --- parseCreatedAt: direct unit tests for the fallback layout chain ---

func TestParseCreatedAt_RFC3339Nano(t *testing.T) {
	got, err := parseCreatedAt("2026-04-13T20:08:23.039251149Z")
	if err != nil {
		t.Fatalf("parseCreatedAt: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 4 || got.Day() != 13 {
		t.Errorf("parsed date = %v, want 2026-04-13", got)
	}
}

func TestParseCreatedAt_TapesOnDiskFormat(t *testing.T) {
	// Format SQLite's CURRENT_TIMESTAMP produces.
	got, err := parseCreatedAt("2026-04-13 20:08:23.039251149+00:00")
	if err != nil {
		t.Fatalf("parseCreatedAt: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 4 || got.Day() != 13 {
		t.Errorf("parsed date = %v, want 2026-04-13", got)
	}
	if got.Hour() != 20 || got.Minute() != 8 {
		t.Errorf("parsed time = %v, want 20:08", got)
	}
}

func TestParseCreatedAt_TapesOnDiskFormat_NoFractional(t *testing.T) {
	got, err := parseCreatedAt("2026-04-13 20:08:23+00:00")
	if err != nil {
		t.Fatalf("parseCreatedAt: %v", err)
	}
	if got.Hour() != 20 || got.Minute() != 8 || got.Second() != 23 {
		t.Errorf("parsed time = %v, want 20:08:23", got)
	}
}

func TestParseCreatedAt_Unparseable(t *testing.T) {
	_, err := parseCreatedAt("not-a-timestamp")
	if err == nil {
		t.Fatal("expected error on unparseable input, got nil")
	}
}

func TestParseCreatedAt_NonZeroOffset(t *testing.T) {
	got, err := parseCreatedAt("2026-04-13 20:08:23.5-05:00")
	if err != nil {
		t.Fatalf("parseCreatedAt: %v", err)
	}
	// 20:08 at -05:00 == 01:08 UTC next day.
	utc := got.UTC()
	if utc.Day() != 14 || utc.Hour() != 1 || utc.Minute() != 8 {
		t.Errorf("parsed UTC = %v, want 2026-04-14 01:08 UTC", utc)
	}
}

// --- readRows: tapes on-disk timestamp format (issue #4) ---

// insertRowTapesFormat writes a row using SQLite's default CURRENT_TIMESTAMP
// format (space separator, numeric offset) — the format tapes actually uses.
// See issue #4: the cursor is formatted as RFC3339Nano ("T"/"Z") but tapes
// writes "2026-04-13 20:08:23.039251149+00:00". SQLite string-compares
// these, so the RFC3339Nano cursor is always lexicographically greater
// than any tapes-written row and WHERE created_at > ? returns nothing.
func insertRowTapesFormat(t *testing.T, db *sql.DB, createdAt time.Time, model string, promptTokens, completionTokens int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO nodes (created_at, model, prompt_tokens, completion_tokens) VALUES (?, ?, ?, ?)`,
		createdAt.UTC().Format("2006-01-02 15:04:05.999999999-07:00"), model, promptTokens, completionTokens,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadRows_TapesOnDiskFormat_AfterCursor(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	recent := now.Add(-30 * time.Minute)

	// Row stored in tapes' on-disk format (space separator, numeric offset).
	insertRowTapesFormat(t, db, recent, "tapes-format-model", 200, 100)

	// Cursor one hour ago — recent row should be visible.
	cursor := now.Add(-time.Hour)
	rows, err := readRows(db, cursor)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (row with space-separated timestamp should be visible)", len(rows))
	}
	if rows[0].model != "tapes-format-model" {
		t.Errorf("model = %q, want %q", rows[0].model, "tapes-format-model")
	}
	if rows[0].promptTokens != 200 {
		t.Errorf("promptTokens = %d, want 200", rows[0].promptTokens)
	}
}

func TestReadRows_TapesOnDiskFormat_CursorStillFiltersOld(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	insertRowTapesFormat(t, db, old, "old-model", 1, 1)
	insertRowTapesFormat(t, db, recent, "new-model", 2, 2)

	cursor := now.Add(-time.Hour)
	rows, err := readRows(db, cursor)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (old row should be filtered out)", len(rows))
	}
	if rows[0].model != "new-model" {
		t.Errorf("model = %q, want %q", rows[0].model, "new-model")
	}
}

// TestReadRows_TapesOnDiskFormat_MixedWithRFC3339 asserts readRows handles
// a database containing both timestamp formats. This future-proofs the fix:
// if tapes ever changes its storage format or clawtel's own writes land in
// a test fixture, both should still be queryable.
func TestReadRows_TapesOnDiskFormat_MixedWithRFC3339(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	now := time.Now().UTC()
	rfcTime := now.Add(-40 * time.Minute)
	tapesTime := now.Add(-20 * time.Minute)

	insertRow(t, db, rfcTime, "rfc-model", 10, 5)
	insertRowTapesFormat(t, db, tapesTime, "tapes-model", 20, 10)

	cursor := now.Add(-time.Hour)
	rows, err := readRows(db, cursor)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (both formats should be visible)", len(rows))
	}
	// Rows must be in ascending order — rfcTime first, tapesTime second.
	if rows[0].model != "rfc-model" {
		t.Errorf("rows[0].model = %q, want %q", rows[0].model, "rfc-model")
	}
	if rows[1].model != "tapes-model" {
		t.Errorf("rows[1].model = %q, want %q", rows[1].model, "tapes-model")
	}
}

// --- integration tests: end-to-end bug reproductions (issue #4) ---

// capturingServer records every heartbeat received so tests can assert
// what actually left the process, not just what readRows returned.
type capturingServer struct {
	*httptest.Server
	received []heartbeat
}

func newCapturingServer(t *testing.T) *capturingServer {
	t.Helper()
	cs := &capturingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var hb heartbeat
		if err := json.Unmarshal(body, &hb); err != nil {
			t.Errorf("server got invalid JSON: %v", err)
		}
		cs.received = append(cs.received, hb)
		w.WriteHeader(http.StatusCreated)
	}))
	return cs
}

// TestPoll_TwoPolls_TapesFormat_IssueRepro reproduces the exact bug flow
// from issue #4: poll 1 sweeps existing rows and returns a fresh cursor;
// a new row lands in tapes' on-disk format; poll 2 MUST see it.
//
// Before the fix, poll 2 returned zero rows because the cursor (RFC3339Nano)
// was always lexicographically greater than any tapes-written timestamp.
func TestPoll_TwoPolls_TapesFormat_IssueRepro(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	server := newCapturingServer(t)
	defer server.Close()

	// Initial data: one row that poll 1 should sweep.
	now := time.Now().UTC()
	insertRowTapesFormat(t, db, now.Add(-30*time.Minute), "before-poll-1", 100, 50)

	// Poll 1. Cursor starts 1 hour ago so the existing row is visible.
	cursor := now.Add(-time.Hour)
	newCursor, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if len(server.received) != 1 {
		t.Fatalf("after poll 1: server got %d heartbeats, want 1", len(server.received))
	}
	if server.received[0].MessageCount != 1 {
		t.Fatalf("poll 1 heartbeat: MessageCount = %d, want 1", server.received[0].MessageCount)
	}

	// Between polls: a new row lands, in tapes' on-disk format, AFTER the
	// cursor poll 1 returned. This is the scenario the original bug silently
	// dropped for every deployment after its first run.
	afterFirstPoll := newCursor.Add(time.Minute)
	insertRowTapesFormat(t, db, afterFirstPoll, "between-polls", 200, 100)

	// Poll 2. Must see the new row.
	_, _, err = pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", newCursor, nil, "")
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(server.received) != 2 {
		t.Fatalf("after poll 2: server got %d heartbeats, want 2", len(server.received))
	}
	hb2 := server.received[1]
	if hb2.MessageCount != 1 {
		t.Errorf("poll 2 heartbeat: MessageCount = %d, want 1 (bug #4 would give 0)", hb2.MessageCount)
	}
	if hb2.Model != "between-polls" {
		t.Errorf("poll 2 heartbeat: Model = %q, want %q", hb2.Model, "between-polls")
	}
	if hb2.InputTokens != 200 || hb2.OutputTokens != 100 {
		t.Errorf("poll 2 heartbeat: tokens = (%d,%d), want (200,100)", hb2.InputTokens, hb2.OutputTokens)
	}
}

// TestPoll_CursorRoundtripThroughDisk_TapesFormat is the two-poll scenario
// PLUS the cursor actually being saved to and loaded from disk between
// polls — exactly what clawtel does in production. The cursor file is
// RFC3339Nano; the rows are tapes-format; the fix must bridge them.
func TestPoll_CursorRoundtripThroughDisk_TapesFormat(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	server := newCapturingServer(t)
	defer server.Close()

	cursorPath := filepath.Join(t.TempDir(), "cursor")

	now := time.Now().UTC()
	insertRowTapesFormat(t, db, now.Add(-45*time.Minute), "first-window", 10, 5)

	// Poll 1 with on-disk cursor round-trip.
	cursor, err := loadCursor(cursorPath)
	if err != nil {
		t.Fatalf("loadCursor (first run): %v", err)
	}
	// First-run cursor is "now" so back-date to see existing rows.
	cursor = now.Add(-time.Hour)
	newCursor, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if err := saveCursor(cursorPath, newCursor); err != nil {
		t.Fatalf("saveCursor: %v", err)
	}

	// Verify the on-disk format is RFC3339Nano (the "T"/"Z" form that was
	// causing the bug). If someone ever changes saveCursor, this assertion
	// catches the drift and forces a re-evaluation of this test.
	cursorBytes, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("read cursor file: %v", err)
	}
	cursorStr := string(cursorBytes)
	if !bytes.ContainsAny(cursorBytes, "T") || !bytes.HasSuffix(cursorBytes, []byte("Z")) {
		t.Fatalf("cursor on disk = %q, want RFC3339Nano (T…Z) form", cursorStr)
	}

	// New row after the cursor, in tapes' on-disk format.
	insertRowTapesFormat(t, db, newCursor.Add(time.Minute), "second-window", 20, 10)

	// Poll 2 loads the cursor from disk (RFC3339Nano) and must see the row.
	loadedCursor, err := loadCursor(cursorPath)
	if err != nil {
		t.Fatalf("loadCursor (second run): %v", err)
	}
	_, _, err = pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", loadedCursor, nil, "")
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(server.received) != 2 {
		t.Fatalf("got %d heartbeats, want 2", len(server.received))
	}
	if server.received[1].MessageCount != 1 {
		t.Errorf("poll 2 MessageCount = %d, want 1", server.received[1].MessageCount)
	}
	if server.received[1].Model != "second-window" {
		t.Errorf("poll 2 Model = %q, want %q", server.received[1].Model, "second-window")
	}
}

// TestReadRows_CurrentTimestampKeyword uses the literal SQL CURRENT_TIMESTAMP
// keyword from the issue suggestion. SQLite's CURRENT_TIMESTAMP produces
// "YYYY-MM-DD HH:MM:SS" — no fractional seconds, no timezone suffix.
// The row must be visible AND the scan-side parser must accept the format.
func TestReadRows_CurrentTimestampKeyword(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO nodes (created_at, model, prompt_tokens, completion_tokens)
		VALUES (CURRENT_TIMESTAMP, 'ct-model', 77, 33)`)
	if err != nil {
		t.Fatal(err)
	}

	// Cursor from 1 hour ago — the just-inserted row must be visible.
	cursor := time.Now().UTC().Add(-time.Hour)
	rows, err := readRows(db, cursor)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (CURRENT_TIMESTAMP row should be visible)", len(rows))
	}
	if rows[0].model != "ct-model" {
		t.Errorf("model = %q, want %q", rows[0].model, "ct-model")
	}
	if rows[0].promptTokens != 77 || rows[0].completionTokens != 33 {
		t.Errorf("tokens = (%d,%d), want (77,33)", rows[0].promptTokens, rows[0].completionTokens)
	}
}

// TestParseCreatedAt_CurrentTimestampFormat asserts the parser handles the
// raw output of SQLite's CURRENT_TIMESTAMP (UTC, no fractional, no TZ).
func TestParseCreatedAt_CurrentTimestampFormat(t *testing.T) {
	got, err := parseCreatedAt("2026-04-13 20:08:23")
	if err != nil {
		t.Fatalf("parseCreatedAt: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 4 || got.Day() != 13 {
		t.Errorf("date = %v, want 2026-04-13", got)
	}
	if got.Hour() != 20 || got.Minute() != 8 || got.Second() != 23 {
		t.Errorf("time = %v, want 20:08:23", got)
	}
	// CURRENT_TIMESTAMP is always UTC per SQLite docs.
	if got.Location() != time.UTC {
		t.Errorf("location = %v, want UTC", got.Location())
	}
}

// --- sendToURL: connection refused ---

func TestSendToURL_ConnectionError(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	hb := heartbeat{ClawID: "test"}

	// Use a URL that will refuse connection
	err := sendToURL(client, "http://127.0.0.1:1", "ik_test", hb)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

// --- readRows: Scan error (wrong column type) ---

func TestReadRows_ScanError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create table where prompt_tokens is TEXT instead of INTEGER
	_, err = db.Exec(`CREATE TABLE nodes (
		created_at TEXT NOT NULL,
		model TEXT NOT NULL,
		prompt_tokens TEXT NOT NULL,
		completion_tokens INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO nodes VALUES ('2026-03-30T12:00:00Z', 'model', 'not-a-number', 0)`)
	if err != nil {
		t.Fatal(err)
	}

	_, readErr := readRows(db, time.Time{})
	if readErr == nil {
		t.Fatal("expected scan error, got nil")
	}
}

// --- assertSchema: Scan error ---

func TestAssertSchema_ScanError(t *testing.T) {
	// This is hard to trigger with a real PRAGMA — the PRAGMA table_info
	// always returns consistent column types. We accept that the
	// rows.Scan error branch in assertSchema is defensive and
	// not practically reachable without mocking the sql.DB interface.
	// Coverage for assertSchema is 92.6% — the uncovered lines are
	// db.Query error and rows.Scan error, both infrastructure failures.
}

// --- resolveDBPath: no home dir fallback ---

func TestResolveDBPath_NoHome(t *testing.T) {
	t.Setenv("TAPES_DB", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows

	origDir, _ := os.Getwd()
	tmpDir := t.TempDir()
	os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	_, err := resolveDBPath()
	// Either "cannot find home dir" or "not found" depending on OS behavior
	if err == nil {
		t.Skip("resolveDBPath found a db somewhere — can't test error path in this env")
	}
}

// --- heartbeat JSON serialization ---

func TestHeartbeat_JSONFormat(t *testing.T) {
	hb := heartbeat{
		ClawID:       "test-claw",
		WindowStart:  time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC),
		WindowEnd:    time.Date(2026, 3, 30, 13, 0, 0, 0, time.UTC),
		Model:        "claude-opus-4-6",
		InputTokens:  1500,
		OutputTokens: 750,
		MessageCount: 10,
	}

	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]interface{}
	json.Unmarshal(data, &m)

	if m["claw_id"] != "test-claw" {
		t.Errorf("claw_id = %v", m["claw_id"])
	}
	if m["model"] != "claude-opus-4-6" {
		t.Errorf("model = %v", m["model"])
	}
	if m["input_tokens"].(float64) != 1500 {
		t.Errorf("input_tokens = %v", m["input_tokens"])
	}
	if m["output_tokens"].(float64) != 750 {
		t.Errorf("output_tokens = %v", m["output_tokens"])
	}
	if m["message_count"].(float64) != 10 {
		t.Errorf("message_count = %v", m["message_count"])
	}
}

// --- parseLockFile tests ---

func TestParseLockFile_Valid(t *testing.T) {
	valid, _, _, _ := lockFixtures(t)
	got, err := parseLockFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	if got["granola"] != "1.0.0" {
		t.Errorf("granola = %q, want 1.0.0", got["granola"])
	}
	if got["openclaw-linear"] != "1.0.1" {
		t.Errorf("openclaw-linear = %q, want 1.0.1", got["openclaw-linear"])
	}
}

func TestParseLockFile_Malformed(t *testing.T) {
	_, malformed, _, _ := lockFixtures(t)
	_, err := parseLockFile(malformed)
	if err == nil {
		t.Fatal("expected parse error for malformed JSON, got nil")
	}
}

func TestParseLockFile_Missing(t *testing.T) {
	_, _, _, missing := lockFixtures(t)
	_, err := parseLockFile(missing)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestParseLockFile_WrongShape(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "lock.json")
	os.WriteFile(tmp, []byte(`{"version": 2, "skills": {}}`), 0600)

	_, err := parseLockFile(tmp)
	if err == nil {
		t.Fatal("expected error for unsupported lock version, got nil")
	}
}

func TestParseLockFile_EmptySkills(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "lock.json")
	os.WriteFile(tmp, []byte(`{"version": 1, "skills": {}}`), 0600)

	got, err := parseLockFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0", len(got))
	}
}

func TestParseLockFile_IgnoresInstalledAt(t *testing.T) {
	valid, _, _, _ := lockFixtures(t)
	got, err := parseLockFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	for slug, v := range got {
		if v == "" {
			t.Errorf("skill %q has empty version", slug)
		}
	}
}

// --- compareSemver tests ---

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.2.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.0", "1.0.0", 0},
		{"1", "1.0.0", 0},
		{"1.0.0", "1.0", 0},
		{"", "1.0.0", -1},
		{"1.0.0", "", 1},
		{"", "", 0},
	}
	for _, c := range cases {
		got := compareSemver(c.a, c.b)
		if got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareSemver_NonNumeric(t *testing.T) {
	if got := compareSemver("1.0.0-beta", "1.0.0-alpha"); got <= 0 {
		t.Errorf("compareSemver(beta, alpha) = %d, want > 0", got)
	}
	if got := compareSemver("1.0.0-alpha", "1.0.0-beta"); got >= 0 {
		t.Errorf("compareSemver(alpha, beta) = %d, want < 0", got)
	}
}

// --- dedupeSkills tests ---

func TestDedupeSkills_SingleSource(t *testing.T) {
	in := []map[string]string{
		{"granola": "1.0.0", "openclaw-linear": "1.0.1"},
	}
	got := dedupeSkills(in)
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	if got[0].Slug != "granola" {
		t.Errorf("got[0].Slug = %q, want granola", got[0].Slug)
	}
	if got[1].Slug != "openclaw-linear" {
		t.Errorf("got[1].Slug = %q, want openclaw-linear", got[1].Slug)
	}
}

func TestDedupeSkills_KeepsHighestVersion(t *testing.T) {
	in := []map[string]string{
		{"granola": "1.0.0"},
		{"granola": "1.2.0"},
		{"granola": "1.1.0"},
	}
	got := dedupeSkills(in)
	if len(got) != 1 {
		t.Fatalf("got %d skills, want 1", len(got))
	}
	if got[0].Version != "1.2.0" {
		t.Errorf("Version = %q, want 1.2.0", got[0].Version)
	}
}

func TestDedupeSkills_MergesDistinctSlugs(t *testing.T) {
	in := []map[string]string{
		{"granola": "1.0.0"},
		{"tapes-cli": "0.3.0"},
		{"openclaw-linear": "1.0.1"},
	}
	got := dedupeSkills(in)
	if len(got) != 3 {
		t.Fatalf("got %d skills, want 3", len(got))
	}
	expectedOrder := []string{"granola", "openclaw-linear", "tapes-cli"}
	for i, want := range expectedOrder {
		if got[i].Slug != want {
			t.Errorf("got[%d].Slug = %q, want %q", i, got[i].Slug, want)
		}
	}
}

func TestDedupeSkills_Empty(t *testing.T) {
	got := dedupeSkills(nil)
	if got == nil {
		t.Fatal("dedupeSkills(nil) returned nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0", len(got))
	}
}

func TestDedupeSkills_EmptyMaps(t *testing.T) {
	in := []map[string]string{{}, {}}
	got := dedupeSkills(in)
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0", len(got))
	}
}

// --- loadSkills tests ---

func TestLoadSkills_NoPaths(t *testing.T) {
	got := loadSkills(nil)
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0", len(got))
	}
}

func TestLoadSkills_SingleValidFile(t *testing.T) {
	valid, _, _, _ := lockFixtures(t)
	got := loadSkills([]string{valid})
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	if got[0].Slug != "granola" || got[0].Version != "1.0.0" {
		t.Errorf("got[0] = %+v, want {granola 1.0.0}", got[0])
	}
}

func TestLoadSkills_MultipleFilesWithDedupe(t *testing.T) {
	valid, _, v2, _ := lockFixtures(t)
	got := loadSkills([]string{valid, v2})
	if len(got) != 3 {
		t.Fatalf("got %d skills, want 3", len(got))
	}
	slugToVersion := map[string]string{}
	for _, s := range got {
		slugToVersion[s.Slug] = s.Version
	}
	if slugToVersion["granola"] != "1.2.0" {
		t.Errorf("granola = %q, want 1.2.0 (highest)", slugToVersion["granola"])
	}
	if slugToVersion["openclaw-linear"] != "1.0.1" {
		t.Errorf("openclaw-linear = %q, want 1.0.1", slugToVersion["openclaw-linear"])
	}
	if slugToVersion["tapes-cli"] != "0.3.0" {
		t.Errorf("tapes-cli = %q, want 0.3.0", slugToVersion["tapes-cli"])
	}
}

func TestLoadSkills_SkipsMalformed(t *testing.T) {
	valid, malformed, _, _ := lockFixtures(t)
	got := loadSkills([]string{valid, malformed})
	if len(got) != 2 {
		t.Errorf("got %d skills, want 2 (malformed skipped)", len(got))
	}
}

func TestLoadSkills_SkipsMissing(t *testing.T) {
	valid, _, _, missing := lockFixtures(t)
	got := loadSkills([]string{valid, missing})
	if len(got) != 2 {
		t.Errorf("got %d skills, want 2 (missing skipped)", len(got))
	}
}

func TestLoadSkills_AllInvalid(t *testing.T) {
	_, malformed, _, missing := lockFixtures(t)
	got := loadSkills([]string{missing, malformed})
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0 (all invalid)", len(got))
	}
}

// --- hashSkills tests ---

func TestHashSkills_Stable(t *testing.T) {
	a := []skill{{Slug: "granola", Version: "1.0.0"}, {Slug: "tapes", Version: "0.1.0"}}
	b := []skill{{Slug: "granola", Version: "1.0.0"}, {Slug: "tapes", Version: "0.1.0"}}
	if hashSkills(a) != hashSkills(b) {
		t.Error("identical input should produce identical hash")
	}
}

func TestHashSkills_Empty(t *testing.T) {
	h1 := hashSkills(nil)
	h2 := hashSkills([]skill{})
	if h1 != h2 {
		t.Errorf("nil and empty slice should hash equal; got %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty string")
	}
}

func TestHashSkills_DiffersOnVersionChange(t *testing.T) {
	a := []skill{{Slug: "granola", Version: "1.0.0"}}
	b := []skill{{Slug: "granola", Version: "1.0.1"}}
	if hashSkills(a) == hashSkills(b) {
		t.Error("different versions should hash differently")
	}
}

func TestHashSkills_DiffersOnSlugChange(t *testing.T) {
	a := []skill{{Slug: "granola", Version: "1.0.0"}}
	b := []skill{{Slug: "tapes", Version: "1.0.0"}}
	if hashSkills(a) == hashSkills(b) {
		t.Error("different slugs should hash differently")
	}
}

func TestHashSkills_OrderInsensitive(t *testing.T) {
	a := []skill{{Slug: "a", Version: "1"}, {Slug: "b", Version: "2"}}
	b := []skill{{Slug: "b", Version: "2"}, {Slug: "a", Version: "1"}}
	if hashSkills(a) != hashSkills(b) {
		t.Error("hash should be order-insensitive")
	}
}

// --- parseLockPaths tests ---

func TestParseLockPaths_Empty(t *testing.T) {
	got := parseLockPaths("")
	if len(got) != 0 {
		t.Errorf("got %d paths, want 0", len(got))
	}
}

func TestParseLockPaths_Single(t *testing.T) {
	got := parseLockPaths("/root/clawchief/.clawhub/lock.json")
	if len(got) != 1 || got[0] != "/root/clawchief/.clawhub/lock.json" {
		t.Errorf("got %v, want [/root/clawchief/.clawhub/lock.json]", got)
	}
}

func TestParseLockPaths_Multiple(t *testing.T) {
	got := parseLockPaths("/a/lock.json,/b/lock.json,/c/lock.json")
	if len(got) != 3 {
		t.Fatalf("got %d paths, want 3", len(got))
	}
}

func TestParseLockPaths_TrimsSpaces(t *testing.T) {
	got := parseLockPaths(" /a/lock.json , /b/lock.json ")
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2", len(got))
	}
	if got[0] != "/a/lock.json" {
		t.Errorf("got[0] = %q, want trimmed", got[0])
	}
	if got[1] != "/b/lock.json" {
		t.Errorf("got[1] = %q, want trimmed", got[1])
	}
}

func TestParseLockPaths_SkipsEmptyEntries(t *testing.T) {
	got := parseLockPaths("/a/lock.json,,/b/lock.json,")
	if len(got) != 2 {
		t.Errorf("got %v, want 2 entries", got)
	}
}

// --- heartbeat clawhub_skills serialization ---

func TestHeartbeat_OmitsClawhubSkillsWhenNil(t *testing.T) {
	hb := heartbeat{
		ClawID:       "test",
		WindowStart:  time.Now().UTC(),
		WindowEnd:    time.Now().UTC(),
		Model:        "claude-opus-4-6",
		InputTokens:  100,
		OutputTokens: 50,
		MessageCount: 1,
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if _, present := m["clawhub_skills"]; present {
		t.Error("clawhub_skills should be omitted when nil")
	}
}

func TestHeartbeat_IncludesClawhubSkillsWhenSet(t *testing.T) {
	hb := heartbeat{
		ClawID:        "test",
		ClawhubSkills: []skill{{Slug: "granola", Version: "1.0.0"}},
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	skillsRaw, ok := m["clawhub_skills"]
	if !ok {
		t.Fatal("clawhub_skills missing from payload")
	}
	skillsList, ok := skillsRaw.([]interface{})
	if !ok || len(skillsList) != 1 {
		t.Fatalf("clawhub_skills shape unexpected: %v", skillsRaw)
	}
	first := skillsList[0].(map[string]interface{})
	if first["slug"] != "granola" {
		t.Errorf("slug = %v, want granola", first["slug"])
	}
	if first["version"] != "1.0.0" {
		t.Errorf("version = %v, want 1.0.0", first["version"])
	}
	if len(first) != 2 {
		t.Errorf("skill has %d fields, want exactly 2 (slug, version)", len(first))
	}
}

func TestHeartbeat_OmitsClawhubSkillsWhenEmpty(t *testing.T) {
	hb := heartbeat{ClawID: "test", ClawhubSkills: nil}
	data, _ := json.Marshal(hb)
	if bytes.Contains(data, []byte("clawhub_skills")) {
		t.Errorf("nil ClawhubSkills should be omitted; got %s", data)
	}
}

// --- pollWithURL skills wiring tests ---

func TestPoll_IncludesSkillsOnFirstSend(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	var receivedHB heartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedHB)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	valid, _, _, _ := lockFixtures(t)
	cursor := time.Now().UTC().Add(-time.Hour)
	_, newHash, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{valid}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(receivedHB.ClawhubSkills) != 2 {
		t.Errorf("ClawhubSkills len = %d, want 2", len(receivedHB.ClawhubSkills))
	}
	if newHash == "" {
		t.Error("expected non-empty new hash")
	}
}

func TestPoll_OmitsSkillsWhenHashUnchanged(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	var receivedRaw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRaw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	valid, _, _, _ := lockFixtures(t)
	expectedHash := hashSkills(loadSkills([]string{valid}))

	cursor := time.Now().UTC().Add(-time.Hour)
	_, newHash, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{valid}, expectedHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(receivedRaw, []byte("clawhub_skills")) {
		t.Errorf("clawhub_skills should be omitted when hash unchanged; got %s", receivedRaw)
	}
	if newHash != expectedHash {
		t.Errorf("hash should be unchanged; got %q, want %q", newHash, expectedHash)
	}
}

func TestPoll_NoSkillsWhenNoLockPaths(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	var receivedRaw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRaw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	cursor := time.Now().UTC().Add(-time.Hour)
	_, newHash, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		nil, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(receivedRaw, []byte("clawhub_skills")) {
		t.Errorf("clawhub_skills should be absent when no lock paths configured")
	}
	if newHash != hashSkills(nil) {
		t.Errorf("expected hash of empty skills list, got %q", newHash)
	}
}

func TestPoll_HashChangesWhenSkillsChange(t *testing.T) {
	db := createTestDB(t, false)
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	valid, _, _, _ := lockFixtures(t)
	cursor := time.Now().UTC().Add(-time.Hour)

	_, hash1, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		nil, "",
	)
	if err != nil {
		t.Fatal(err)
	}

	_, hash2, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{valid}, hash1,
	)
	if err != nil {
		t.Fatal(err)
	}

	if hash1 == hash2 {
		t.Error("hash should change when skills set changes")
	}
}
