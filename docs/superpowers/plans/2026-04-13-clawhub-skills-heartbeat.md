# clawhub Skills in Heartbeat — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `clawhub_skills` field to the heartbeat payload that lists clawhub-installed skills (slug + version) discovered from one or more `lock.json` files configured via `CLAWTEL_CLAWHUB_LOCKS`.

**Architecture:** Read-only filesystem discovery (no execution, no `SKILL.md` content). Multiple lock files are deduped into a flat sorted list; if the same slug appears at different versions, keep the highest semver and warn. The list is hashed with sha256; the field is omitted from the heartbeat when the hash matches the in-memory last-sent hash, so steady-state heartbeats stay small. On restart the in-memory hash resets, forcing a re-send.

**Tech Stack:** Pure Go, stdlib only (`crypto/sha256`, `encoding/json`, `os`, `strings`, `strconv`, `sort`). No new dependencies. Project convention: single `main.go` plus `main_test.go`. Test fixtures live in `testdata/`.

---

## File Structure

- **Modify:** `main.go`
  - Add `skill` struct, `clawhubSkill` field on `heartbeat` (`omitempty`).
  - Add helpers: `parseLockFile`, `compareSemver`, `dedupeSkills`, `loadSkills`, `hashSkills`, `parseLockPaths`.
  - Extend `pollWithURL` signature with `lockPaths []string` and `lastHash string`; return new `(time.Time, string, error)`.
  - Update `main()` to read `CLAWTEL_CLAWHUB_LOCKS`, log audit lines, hold `lastSentSkillsHash` in memory.
  - Update top-of-file SECURITY MODEL comment.
- **Modify:** `main_test.go` — add tests for every new helper plus integration tests for `pollWithURL` with skills wiring.
- **Create:** `testdata/lock_valid.json` — well-formed lock with two skills.
- **Create:** `testdata/lock_malformed.json` — invalid JSON to verify the heartbeat loop survives parse failure.
- **Create:** `testdata/lock_v2.json` — second valid lock with one overlapping slug at a higher version, to test dedupe.
- **Modify:** `AGENTS.md` — document the new env var, new file reads, and privacy invariants.

The file count stays small. All logic lives in `main.go` to match project convention.

---

## Type definitions (used across tasks)

```go
// skill is the per-skill payload sent to claw.tech.
// ONLY slug and version. Never installedAt, paths, hashes, or anything else.
type skill struct {
    Slug    string `json:"slug"`
    Version string `json:"version"`
}

// lockFile is the on-disk shape of <workdir>/.clawhub/lock.json.
// Only the version field per skill is read. installedAt is intentionally ignored.
type lockFile struct {
    Version int                       `json:"version"`
    Skills  map[string]lockFileEntry  `json:"skills"`
}

type lockFileEntry struct {
    Version string `json:"version"`
    // installedAt is deliberately NOT included in this struct.
}
```

These are added in Task 1 and referenced unchanged thereafter.

---

### Task 1: Add `skill` and `lockFile` types plus `parseLockFile`

**Files:**
- Modify: `main.go` (add types and one function near other helpers, after `resolveCursorPath`)
- Modify: `main_test.go` (append new tests at end)
- Create: `testdata/lock_valid.json`
- Create: `testdata/lock_malformed.json`
- Create: `testdata/lock_v2.json`

- [ ] **Step 1: Create the `testdata/lock_valid.json` fixture**

```json
{
  "version": 1,
  "skills": {
    "granola":         { "version": "1.0.0", "installedAt": 1775635621739 },
    "openclaw-linear": { "version": "1.0.1", "installedAt": 1775635629099 }
  }
}
```

- [ ] **Step 2: Create the `testdata/lock_malformed.json` fixture**

```
{ this is not valid json
```

- [ ] **Step 3: Create the `testdata/lock_v2.json` fixture**

```json
{
  "version": 1,
  "skills": {
    "granola":   { "version": "1.2.0", "installedAt": 1775635621740 },
    "tapes-cli": { "version": "0.3.0", "installedAt": 1775635629100 }
  }
}
```

- [ ] **Step 4: Write the failing tests for `parseLockFile`**

Append to `main_test.go`:

```go
// --- parseLockFile tests ---

func TestParseLockFile_Valid(t *testing.T) {
	got, err := parseLockFile("testdata/lock_valid.json")
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
	_, err := parseLockFile("testdata/lock_malformed.json")
	if err == nil {
		t.Fatal("expected parse error for malformed JSON, got nil")
	}
}

func TestParseLockFile_Missing(t *testing.T) {
	_, err := parseLockFile("testdata/does_not_exist.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestParseLockFile_WrongShape(t *testing.T) {
	// version 2 should be rejected — protect against future format drift
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
	// Verify the parsed result has only version, not installedAt.
	got, err := parseLockFile("testdata/lock_valid.json")
	if err != nil {
		t.Fatal(err)
	}
	// got is map[string]string of slug -> version; installedAt cannot
	// possibly be present. This test documents intent.
	for slug, v := range got {
		if v == "" {
			t.Errorf("skill %q has empty version", slug)
		}
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test -v -run TestParseLockFile ./...`
Expected: FAIL with "undefined: parseLockFile"

- [ ] **Step 6: Add the types and `parseLockFile` to `main.go`**

Insert after `resolveCursorPath` (around line 386):

```go
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
```

- [ ] **Step 7: Run the tests and verify they pass**

Run: `go test -v -run TestParseLockFile ./...`
Expected: PASS for all six tests.

- [ ] **Step 8: Commit**

```bash
git add main.go main_test.go testdata/
git commit -m "feat: parse clawhub lock.json files (slug+version only)"
```

---

### Task 2: Add `compareSemver` for highest-version dedupe

**Files:**
- Modify: `main.go` (add helper near `parseLockFile`)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
// --- compareSemver tests ---

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.2.0", "1.10.0", -1}, // numeric, not lexical
		{"2.0.0", "1.99.99", 1},
		{"1.0", "1.0.0", 0},     // missing components treated as 0
		{"1", "1.0.0", 0},
		{"1.0.0", "1.0", 0},
		{"", "1.0.0", -1},       // empty < anything
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
	// Non-numeric components fall back to string compare for that segment.
	// We don't need full semver-pre-release support; we just need stable behavior.
	got := compareSemver("1.0.0-beta", "1.0.0-alpha")
	if got == 0 {
		t.Error("compareSemver should distinguish 1.0.0-beta from 1.0.0-alpha")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestCompareSemver ./...`
Expected: FAIL with "undefined: compareSemver"

- [ ] **Step 3: Add `compareSemver` to `main.go`**

Add after `parseLockFile`:

```go
// compareSemver returns -1 if a<b, 0 if a==b, 1 if a>b.
// Compares dotted components numerically when possible, lexically otherwise.
// Missing trailing components are treated as 0 ("1.0" == "1.0.0").
// This is intentionally simple — the lockfile values we see are plain semver
// like "1.0.0" or "1.0.1". Pre-release suffixes fall through to string compare.
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
		var ap, bp string
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
```

Add `"strconv"` and `"strings"` to the import block at the top of `main.go`.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestCompareSemver ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add compareSemver helper for lockfile dedupe"
```

---

### Task 3: Add `dedupeSkills` to merge multiple lock maps

**Files:**
- Modify: `main.go` (add helper after `compareSemver`)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
// --- dedupeSkills tests ---

func TestDedupeSkills_SingleSource(t *testing.T) {
	in := []map[string]string{
		{"granola": "1.0.0", "openclaw-linear": "1.0.1"},
	}
	got := dedupeSkills(in)
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	// Sorted by slug
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
	// Verify sorted order
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestDedupeSkills ./...`
Expected: FAIL with "undefined: dedupeSkills"

- [ ] **Step 3: Add `dedupeSkills` to `main.go`**

Add after `compareSemver`:

```go
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
			if compareSemver(version, cur) > 0 {
				log.Printf("clawhub skill %q: keeping %s over %s (higher semver)", slug, version, cur)
				merged[slug] = version
			} else if compareSemver(version, cur) < 0 {
				log.Printf("clawhub skill %q: keeping %s over %s (higher semver)", slug, cur, version)
			}
			// equal: no-op
		}
	}
	out := make([]skill, 0, len(merged))
	for slug, version := range merged {
		out = append(out, skill{Slug: slug, Version: version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}
```

Add `"sort"` to the import block.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestDedupeSkills ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: dedupe clawhub skills across lock files (highest semver wins)"
```

---

### Task 4: Add `loadSkills` orchestrator that handles missing/malformed files

**Files:**
- Modify: `main.go` (add helper after `dedupeSkills`)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
// --- loadSkills tests ---

func TestLoadSkills_NoPaths(t *testing.T) {
	got := loadSkills(nil)
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0", len(got))
	}
}

func TestLoadSkills_SingleValidFile(t *testing.T) {
	got := loadSkills([]string{"testdata/lock_valid.json"})
	if len(got) != 2 {
		t.Fatalf("got %d skills, want 2", len(got))
	}
	if got[0].Slug != "granola" || got[0].Version != "1.0.0" {
		t.Errorf("got[0] = %+v, want {granola 1.0.0}", got[0])
	}
}

func TestLoadSkills_MultipleFilesWithDedupe(t *testing.T) {
	got := loadSkills([]string{
		"testdata/lock_valid.json",
		"testdata/lock_v2.json",
	})
	// Expect: granola (1.2.0 wins), openclaw-linear (1.0.1), tapes-cli (0.3.0)
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
	// Malformed file is logged and skipped — must not crash.
	got := loadSkills([]string{
		"testdata/lock_valid.json",
		"testdata/lock_malformed.json",
	})
	// Should still get the 2 valid skills
	if len(got) != 2 {
		t.Errorf("got %d skills, want 2 (malformed skipped)", len(got))
	}
}

func TestLoadSkills_SkipsMissing(t *testing.T) {
	got := loadSkills([]string{
		"testdata/lock_valid.json",
		"testdata/does_not_exist.json",
	})
	if len(got) != 2 {
		t.Errorf("got %d skills, want 2 (missing skipped)", len(got))
	}
}

func TestLoadSkills_AllInvalid(t *testing.T) {
	got := loadSkills([]string{
		"testdata/does_not_exist.json",
		"testdata/lock_malformed.json",
	})
	if len(got) != 0 {
		t.Errorf("got %d skills, want 0 (all invalid)", len(got))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestLoadSkills ./...`
Expected: FAIL with "undefined: loadSkills"

- [ ] **Step 3: Add `loadSkills` to `main.go`**

Add after `dedupeSkills`:

```go
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
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestLoadSkills ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: loadSkills orchestrator skips bad lock files"
```

---

### Task 5: Add `hashSkills` for change detection

**Files:**
- Modify: `main.go` (add helper after `loadSkills`)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
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
	// loadSkills always returns sorted, but hashSkills should sort defensively
	// so callers can't break the contract by passing unsorted input.
	a := []skill{{Slug: "a", Version: "1"}, {Slug: "b", Version: "2"}}
	b := []skill{{Slug: "b", Version: "2"}, {Slug: "a", Version: "1"}}
	if hashSkills(a) != hashSkills(b) {
		t.Error("hash should be order-insensitive")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestHashSkills ./...`
Expected: FAIL with "undefined: hashSkills"

- [ ] **Step 3: Add `hashSkills` to `main.go`**

Add after `loadSkills`:

```go
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
```

Add `"crypto/sha256"` and `"encoding/hex"` to the import block.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestHashSkills ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: hashSkills for change detection across heartbeats"
```

---

### Task 6: Add `parseLockPaths` for `CLAWTEL_CLAWHUB_LOCKS` env var

**Files:**
- Modify: `main.go` (add helper after `hashSkills`)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestParseLockPaths ./...`
Expected: FAIL with "undefined: parseLockPaths"

- [ ] **Step 3: Add `parseLockPaths` to `main.go`**

Add after `hashSkills`:

```go
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
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestParseLockPaths ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: parse CLAWTEL_CLAWHUB_LOCKS env var into path list"
```

---

### Task 7: Add `ClawhubSkills` field to heartbeat struct (omitempty)

**Files:**
- Modify: `main.go` (extend `heartbeat` struct around line 52)
- Modify: `main_test.go` (append new tests)

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
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
		// ClawhubSkills not set
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
	// Confirm no extra fields leaked
	if len(first) != 2 {
		t.Errorf("skill has %d fields, want exactly 2 (slug, version)", len(first))
	}
}

func TestHeartbeat_OmitsClawhubSkillsWhenEmpty(t *testing.T) {
	// Distinguish "no field" from "empty list". We want omitted when empty
	// so the receiver's last-known state is preserved.
	hb := heartbeat{ClawID: "test", ClawhubSkills: nil}
	data, _ := json.Marshal(hb)
	if bytes.Contains(data, []byte("clawhub_skills")) {
		t.Errorf("nil ClawhubSkills should be omitted; got %s", data)
	}
}
```

You'll also need `"bytes"` already imported in `main_test.go`. Add it to the import block if missing.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -v -run TestHeartbeat ./...`
Expected: FAIL — `ClawhubSkills` is undefined.

- [ ] **Step 3: Modify the `heartbeat` struct in `main.go`**

Replace the struct (around lines 50-60) with:

```go
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
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -v -run TestHeartbeat ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add optional clawhub_skills field to heartbeat payload"
```

---

### Task 8: Wire skills into `pollWithURL`

`pollWithURL` needs to load skills, hash them, and only attach when the hash changed since the last successful send. We'll thread the prior hash in and the new hash out through the return value.

**Files:**
- Modify: `main.go` (`poll`, `pollWithURL`)
- Modify: `main_test.go` (update existing poll tests; add new ones)

- [ ] **Step 1: Update existing `pollWithURL` callers in tests**

Existing poll tests call `pollWithURL` with 6 args. The new signature will add `lockPaths []string` and `lastSkillsHash string`, and return `(time.Time, string, error)`. Update each existing call site:

In `TestPoll_Success`:
```go
newCursor, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
```

In `TestPoll_SendError`:
```go
_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
```

In `TestPoll_EmptyDB`:
```go
_, _, err := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
```

In `TestPoll_ReadError`:
```go
_, _, pollErr := pollWithURL(db, server.Client(), server.URL, "ik_test", "test-claw", cursor, nil, "")
```

- [ ] **Step 2: Add new tests for skills wiring**

Append to `main_test.go`:

```go
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

	cursor := time.Now().UTC().Add(-time.Hour)
	_, newHash, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{"testdata/lock_valid.json"}, "",
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

	// Pre-compute the hash that loadSkills would produce
	expectedHash := hashSkills(loadSkills([]string{"testdata/lock_valid.json"}))

	cursor := time.Now().UTC().Add(-time.Hour)
	_, newHash, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{"testdata/lock_valid.json"}, expectedHash,
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
	// Even with no lock paths, hash of empty list is deterministic
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

	cursor := time.Now().UTC().Add(-time.Hour)

	// First call: no skills
	_, hash1, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		nil, "",
	)
	if err != nil {
		t.Fatal(err)
	}

	// Second call: with skills
	_, hash2, err := pollWithURL(
		db, server.Client(), server.URL, "ik_test", "test-claw", cursor,
		[]string{"testdata/lock_valid.json"}, hash1,
	)
	if err != nil {
		t.Fatal(err)
	}

	if hash1 == hash2 {
		t.Error("hash should change when skills set changes")
	}
}
```

- [ ] **Step 3: Run the updated tests to verify they fail**

Run: `go test -v -run TestPoll ./...`
Expected: FAIL — signature mismatch and undefined behavior.

- [ ] **Step 4: Modify `poll` and `pollWithURL` in `main.go`**

Replace the existing `poll` and `pollWithURL` functions with:

```go
// poll reads new rows since cursor, aggregates, and sends a heartbeat.
// A heartbeat is always sent — even with zero new rows — so that
// claw.tech can distinguish "online but idle" from "offline".
// Returns the new cursor timestamp and the new skills hash on success.
func poll(db *sql.DB, client *http.Client, ingestKey, clawID string, cursor time.Time, lockPaths []string, lastSkillsHash string) (time.Time, string, error) {
	return pollWithURL(db, client, ingestEndpoint, ingestKey, clawID, cursor, lockPaths, lastSkillsHash)
}

// pollWithURL is the testable version of poll that accepts a custom endpoint URL.
func pollWithURL(db *sql.DB, client *http.Client, url, ingestKey, clawID string, cursor time.Time, lockPaths []string, lastSkillsHash string) (time.Time, string, error) {
	windowStart := cursor
	windowEnd := time.Now().UTC()

	rows, err := readRows(db, cursor)
	if err != nil {
		return cursor, lastSkillsHash, fmt.Errorf("read: %v", err)
	}

	hb := aggregate(clawID, windowStart, windowEnd, rows)

	skills := loadSkills(lockPaths)
	newHash := hashSkills(skills)
	if newHash != lastSkillsHash {
		hb.ClawhubSkills = skills
	}

	if err := sendToURL(client, url, ingestKey, hb); err != nil {
		return cursor, lastSkillsHash, fmt.Errorf("send: %v", err)
	}

	if len(rows) > 0 {
		log.Printf("sent: %d turns, %d in, %d out, model=%s",
			hb.MessageCount, hb.InputTokens, hb.OutputTokens, hb.Model)
	}
	if hb.ClawhubSkills != nil {
		log.Printf("sent: %d clawhub skills (hash changed)", len(hb.ClawhubSkills))
	}

	return windowEnd, newHash, nil
}
```

- [ ] **Step 5: Run the full test suite to verify all poll tests pass**

Run: `go test -v -run TestPoll ./...`
Expected: PASS for all poll tests.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: include clawhub_skills in heartbeat when set changes"
```

---

### Task 9: Wire `CLAWTEL_CLAWHUB_LOCKS` into `main()` with audit logs

**Files:**
- Modify: `main.go` (`main` function body)

Note: `main` is excluded from coverage targets per AGENTS.md, so no new tests are required for this task. The behavior is fully covered by Task 8's poll tests.

- [ ] **Step 1: Update the security model comment at the top of `main.go`**

Replace lines 1-23 of `main.go` with:

```go
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
```

- [ ] **Step 2: Update the `main()` function to read the env var and emit audit logs**

In `main()`, after the existing `log.Printf("sends:  ...")` line (around line 98), insert:

```go
	lockPaths := parseLockPaths(os.Getenv("CLAWTEL_CLAWHUB_LOCKS"))
	if len(lockPaths) > 0 {
		log.Printf("clawhub locks: %d paths configured", len(lockPaths))
		log.Printf("clawhub:  reads lock.json fields: version, skills.<slug>.version (nothing else)")
		log.Printf("clawhub:  NOTE: lock.json contains \"installedAt\" timestamp, clawtel does NOT read it")
	}
```

- [ ] **Step 3: Add an in-memory `lastSkillsHash` and pass it to `poll`**

Inside `main()`, before the `for { select ... }` loop, add:

```go
	var lastSkillsHash string
```

Replace the existing `case <-ticker.C:` block with:

```go
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
```

- [ ] **Step 4: Run the full test suite**

Run: `go test -v ./...`
Expected: PASS for all tests.

- [ ] **Step 5: Verify the binary builds**

Run: `CGO_ENABLED=0 go build -ldflags="-s -w" -o clawtel .`
Expected: build succeeds, no errors.

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat: wire CLAWTEL_CLAWHUB_LOCKS through main loop with audit logs"
```

---

### Task 10: Update `AGENTS.md` to document the new env var and read surface

**Files:**
- Modify: `AGENTS.md`

- [ ] **Step 1: Add lock.json to the security constraints section**

In `AGENTS.md`, in the **Security constraints** section, add this bullet after the existing "Never read or access..." bullet:

```markdown
- **Never read** any field from `.clawhub/lock.json` other than top-level `version` and `skills.<slug>.version`. Never read `installedAt`, never read `SKILL.md` content from the workdir
```

- [ ] **Step 2: Add the new env var to the environment table**

In the **Environment variables** table, add a new row:

```markdown
| `CLAWTEL_CLAWHUB_LOCKS` | No | Comma-separated absolute paths to `.clawhub/lock.json` files. When set, slug+version of each installed clawhub skill is added to the heartbeat |
```

- [ ] **Step 3: Update the architecture diagram comment**

Right under the existing architecture diagram, add this paragraph:

```markdown
When `CLAWTEL_CLAWHUB_LOCKS` is set, clawtel also reads `.clawhub/lock.json` files at the configured paths and adds an optional `clawhub_skills` array (slug + version only) to the heartbeat. The field is omitted when unchanged since the last successful send.
```

- [ ] **Step 4: Verify the file renders correctly**

Run: `cat AGENTS.md | head -80`
Visually scan: new bullet appears in security constraints, new row in env vars table, new paragraph after architecture diagram.

- [ ] **Step 5: Commit**

```bash
git add AGENTS.md
git commit -m "docs: document CLAWTEL_CLAWHUB_LOCKS env var and lock.json read surface"
```

---

### Task 11: Run the full coverage report and verify gates

**Files:**
- None modified.

- [ ] **Step 1: Run the test suite with coverage**

Run: `go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out`

Expected: All tests pass. Per-function coverage shows 100% for:
- `parseLockFile`
- `compareSemver`
- `dedupeSkills`
- `loadSkills`
- `hashSkills`
- `parseLockPaths`
- `aggregate`
- `readRows`
- `assertSchema` (modulo the documented PRAGMA scan defensiveness)

- [ ] **Step 2: If any business-logic function is below 100%, add a test for the gap**

Inspect the per-function output. Any new helper below 100% needs an additional test case before this task can complete.

- [ ] **Step 3: Verify the binary still builds clean**

Run: `CGO_ENABLED=0 go build -ldflags="-s -w" -o clawtel .`
Expected: succeeds.

- [ ] **Step 4: Smoke test with a real lock fixture**

Run: `CLAW_INGEST_KEY=ik_dummy CLAW_ID=test-claw CLAWTEL_CLAWHUB_LOCKS="$(pwd)/testdata/lock_valid.json" TAPES_DB=/dev/null ./clawtel`

Expected: startup logs include the audit lines:
```
clawtel: clawhub locks: 1 paths configured
clawtel: clawhub:  reads lock.json fields: version, skills.<slug>.version (nothing else)
clawtel: clawhub:  NOTE: lock.json contains "installedAt" timestamp, clawtel does NOT read it
```

The process will fail to open `/dev/null` as SQLite — that's fine. We're only validating the audit log emission.

- [ ] **Step 5: Final commit if any gap-fillers were added**

```bash
git add main_test.go
git commit -m "test: backfill coverage gaps in clawhub skills helpers"
```

If no gaps, this task is complete with no commit.

---

## Self-Review Checklist (run before handoff)

- **Spec coverage:** Every section of issue #2 is implemented:
  - New env var `CLAWTEL_CLAWHUB_LOCKS`: Tasks 6, 9
  - Behavior on each tick (parse, dedupe, hash, omit-when-unchanged): Tasks 1, 3, 5, 8
  - Heartbeat payload extension (additive `clawhub_skills`): Task 7
  - Cursor / state (in-memory `lastSkillsHash`, reset on restart): Task 9 (deliberately in-memory rather than file — see "Deviations" below)
  - Audit surface startup logs: Task 9
  - Privacy invariants 1–5: enforced by the type design (`lockFileEntry` has only `Version`) and Task 4 (silent skip on parse failure)
  - Test fixtures `valid.json` and `malformed.json`: Task 1 (also adds `lock_v2.json` for dedupe coverage)
  - Companion server work bdougie/claw.tech#54 is out of scope here; the field is `omitempty` so older servers tolerate it.
- **Placeholders:** None. Every step shows the actual code.
- **Type consistency:** `skill`, `lockFile`, `lockFileEntry`, `ClawhubSkills` used identically in every task.

## Deviations from the spec (intentional)

The spec mentions a `<cursor_dir>/skills_hash` file. This plan keeps the last-sent hash in memory only because the only stated behavior ("reset on restart") is achieved naturally with in-memory state, and a file with no read path is dead weight. If the receiver later wants on-disk audit, we can add it without changing the wire format.
