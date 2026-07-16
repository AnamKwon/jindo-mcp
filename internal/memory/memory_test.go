package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewCanonicalizesRelativeRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	target := filepath.Join(t.TempDir(), "nested")
	rel, err := filepath.Rel(cwd, target)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}

	m := New(rel)
	want, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if m.Root() != want {
		t.Fatalf("Root() = %q, want canonical absolute path %q", m.Root(), want)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Write("k", "v1", "alice"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, ok := m.Read("k")
	if !ok {
		t.Fatalf("Read(k): expected ok")
	}
	if got != "v1" {
		t.Fatalf("Read(k) = %v, want v1", got)
	}
	if _, ok := m.Read("missing"); ok {
		t.Fatalf("Read(missing): expected not ok")
	}
}

func TestAllExcludesNotes(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Write("a", "1", "x"); err != nil {
		t.Fatalf("Write a: %v", err)
	}
	if err := m.Write("b", "2", "x"); err != nil {
		t.Fatalf("Write b: %v", err)
	}
	if err := m.AppendNote("x", "hello"); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("All len = %d, want 2 (%v)", len(all), all)
	}
	if all["a"] != "1" || all["b"] != "2" {
		t.Fatalf("All values wrong: %v", all)
	}
	if _, present := all["_notes"]; present {
		t.Fatalf("All must exclude _notes: %v", all)
	}
}

func TestStatsCountsExcludeReservedAndReflectDigest(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Write("a", "1", "alice"); err != nil {
		t.Fatalf("Write a: %v", err)
	}
	if err := m.Write("b", "2", "bob"); err != nil {
		t.Fatalf("Write b: %v", err)
	}
	if err := m.AppendNote("alice", "hello"); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}

	records, hasDigest, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if records != 2 {
		t.Fatalf("Stats records = %d, want 2 (must exclude _notes)", records)
	}
	if hasDigest {
		t.Fatalf("Stats hasDigest = true, want false before any _digest entry exists")
	}

	seedEntry(t, m, digestKey, map[string]any{"folded": true}, "orch", 100)

	records, hasDigest, err = m.Stats()
	if err != nil {
		t.Fatalf("Stats after digest: %v", err)
	}
	if records != 2 {
		t.Fatalf("Stats records = %d, want 2 (must exclude _digest)", records)
	}
	if !hasDigest {
		t.Fatalf("Stats hasDigest = false, want true after _digest entry seeded")
	}
}

func TestAppendNoteAndNotes(t *testing.T) {
	m := New(t.TempDir())
	if err := m.AppendNote("alice", "first"); err != nil {
		t.Fatalf("AppendNote 1: %v", err)
	}
	if err := m.AppendNote("bob", "second"); err != nil {
		t.Fatalf("AppendNote 2: %v", err)
	}
	notes, err := m.Notes()
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("Notes len = %d, want 2", len(notes))
	}
	want := []struct{ author, text string }{
		{"alice", "first"},
		{"bob", "second"},
	}
	for i, w := range want {
		entry, ok := notes[i].(map[string]any)
		if !ok {
			t.Fatalf("note %d not a map: %T", i, notes[i])
		}
		if entry["author"] != w.author {
			t.Fatalf("note %d author = %v, want %s", i, entry["author"], w.author)
		}
		if entry["text"] != w.text {
			t.Fatalf("note %d text = %v, want %s", i, entry["text"], w.text)
		}
	}

	// Notes() on a fresh empty store returns an empty (non-nil) slice.
	empty, err := New(t.TempDir()).Notes()
	if err != nil {
		t.Fatalf("Notes empty: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("Notes on empty store = %v, want empty slice", empty)
	}
}

func TestCrossInstanceSharing(t *testing.T) {
	dir := t.TempDir()
	m1 := New(dir)
	m2 := New(dir)

	if err := m1.Write("k", "v1", "a"); err != nil {
		t.Fatalf("m1.Write: %v", err)
	}
	got, ok := m2.Read("k")
	if !ok || got != "v1" {
		t.Fatalf("m2.Read(k) = %v,%v, want v1,true", got, ok)
	}

	if err := m2.Write("k2", "v2", "b"); err != nil {
		t.Fatalf("m2.Write: %v", err)
	}
	got, ok = m1.Read("k2")
	if !ok || got != "v2" {
		t.Fatalf("m1.Read(k2) = %v,%v, want v2,true", got, ok)
	}
}

func TestAtomicityNoCorruption(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	if err := m.Write("k", "v", "a"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	sawStore := false
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			t.Fatalf("leftover temp file: %s", name)
		}
		// The flock lock file intentionally persists after release (flock is
		// released by the kernel, not by removing the file), so its presence is
		// expected and is not an error.
		if name == "memory.json" {
			sawStore = true
		}
	}
	if !sawStore {
		t.Fatalf("memory.json missing after Write; dir=%v", entries)
	}

	// memory.json must be valid JSON that re-loads.
	raw, err := os.ReadFile(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("ReadFile store: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("memory.json not valid JSON: %v", err)
	}
	if _, ok := data["k"]; !ok {
		t.Fatalf("memory.json missing key k: %v", data)
	}
}

// TestFlockSerializes verifies that flock provides exclusive, cross-instance
// serialization: while one instance holds the lock, a second acquire attempt
// blocks and times out within the bound; after release it succeeds. Since
// acquire/release are unexported we drive them directly (same package).
func TestFlockSerializes(t *testing.T) {
	dir := t.TempDir()
	m1 := New(dir)
	m2 := New(dir)

	// Shrink the (now 10s default) timeout so the block-and-timeout path is
	// exercised quickly; restore it afterward.
	saved := LockTimeout
	LockTimeout = 200 * time.Millisecond
	defer func() { LockTimeout = saved }()

	if err := m1.acquire(); err != nil {
		t.Fatalf("m1.acquire: %v", err)
	}

	// A second acquirer must block and eventually time out with an error while
	// m1 holds the lock.
	start := time.Now()
	err := m2.acquire()
	elapsed := time.Since(start)
	if err == nil {
		m2.release()
		t.Fatalf("m2.acquire succeeded while m1 holds the lock; want timeout error")
	}
	if elapsed < LockTimeout {
		t.Fatalf("m2.acquire returned after %v, want >= LockTimeout %v", elapsed, LockTimeout)
	}
	if elapsed > LockTimeout+time.Second {
		t.Fatalf("m2.acquire took %v, unexpectedly long past LockTimeout %v", elapsed, LockTimeout)
	}

	// After release, acquire must succeed.
	m1.release()
	if err := m2.acquire(); err != nil {
		t.Fatalf("m2.acquire after release: %v", err)
	}
	m2.release()
}

// TestFlockCrashSafety documents the crash-safety win: the lock file persists
// after release (not removed), and a stale lock file left by a "crashed"
// holder does not deadlock a fresh acquire, because the kernel releases the
// advisory lock when the holding fd closes.
func TestFlockCrashSafety(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	// Simulate a crashed prior process: a lock file exists but no live flock.
	if fd, err := os.OpenFile(filepath.Join(dir, "memory.lock"), os.O_CREATE|os.O_RDWR, 0o644); err != nil {
		t.Fatalf("pre-create lock file: %v", err)
	} else {
		fd.Close()
	}
	if err := m.acquire(); err != nil {
		t.Fatalf("acquire over stale lock file must succeed: %v", err)
	}
	m.release()
}

// TestAllocKeyMonotonic asserts repeated AllocKey calls yield a strictly
// increasing, non-colliding sequence task:<agent>:1,2,3,...
func TestAllocKeyMonotonic(t *testing.T) {
	m := New(t.TempDir())
	for i := 1; i <= 5; i++ {
		got, err := m.AllocKey("alice")
		if err != nil {
			t.Fatalf("AllocKey #%d: %v", i, err)
		}
		want := fmt.Sprintf("task:alice:%d", i)
		if got != want {
			t.Fatalf("AllocKey #%d = %q, want %q", i, got, want)
		}
	}
}

// TestAllocKeyFreshProcessContinuation verifies nextN derives from the
// PERSISTED store, not an in-memory counter: a second SharedMemory over the
// same root (simulating a fresh process) continues the sequence without reusing
// any prior key.
func TestAllocKeyFreshProcessContinuation(t *testing.T) {
	dir := t.TempDir()
	m1 := New(dir)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		k, err := m1.AllocKey("bob")
		if err != nil {
			t.Fatalf("m1.AllocKey: %v", err)
		}
		seen[k] = true
	}

	// Fresh instance over the same root — must not reuse and must continue.
	m2 := New(dir)
	k, err := m2.AllocKey("bob")
	if err != nil {
		t.Fatalf("m2.AllocKey: %v", err)
	}
	if seen[k] {
		t.Fatalf("m2.AllocKey reused a prior key: %q", k)
	}
	if k != "task:bob:4" {
		t.Fatalf("m2.AllocKey = %q, want task:bob:4 (continued sequence)", k)
	}

	// Cross-agent continuation: nextN spans all agents' keys.
	k2, err := m2.AllocKey("carol")
	if err != nil {
		t.Fatalf("m2.AllocKey carol: %v", err)
	}
	if k2 != "task:carol:5" {
		t.Fatalf("m2.AllocKey carol = %q, want task:carol:5", k2)
	}
}

// TestAllocKeyConcurrentUnique launches N concurrent AllocKey calls across a
// mix of agents and asserts every returned key is unique.
func TestAllocKeyConcurrentUnique(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	const n = 40
	agents := []string{"alice", "bob", "carol"}
	keys := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := m.AllocKey(agents[i%len(agents)])
			if err != nil {
				t.Errorf("AllocKey #%d: %v", i, err)
				return
			}
			keys[i] = k
		}(i)
	}
	wg.Wait()

	uniq := map[string]bool{}
	for i, k := range keys {
		if k == "" {
			t.Fatalf("key #%d empty (AllocKey errored)", i)
		}
		if uniq[k] {
			t.Fatalf("duplicate key allocated: %q", k)
		}
		uniq[k] = true
	}
	if len(uniq) != n {
		t.Fatalf("got %d unique keys, want %d", len(uniq), n)
	}
}

// TestUpsertIdempotent asserts upserting the same stable key twice yields ONE
// entry (no duplicate), with the latest value/author winning, and that it
// promotes an AllocKey reservation to real content so All()/Read surface it.
func TestUpsertIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	key, err := m.AllocKey("alice")
	if err != nil {
		t.Fatalf("AllocKey: %v", err)
	}

	// Before Upsert, the bare reservation must not surface as a real entry.
	if _, ok := m.Read(key); ok {
		t.Fatalf("Read(%q) returned ok for a bare reservation", key)
	}
	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, present := all[key]; present {
		t.Fatalf("All surfaced a bare reservation: %v", all)
	}

	if err := m.Upsert(key, "first", "alice"); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if err := m.Upsert(key, "second", "bob"); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	all, err = m.All()
	if err != nil {
		t.Fatalf("All after upserts: %v", err)
	}
	count := 0
	for k := range all {
		if k == key {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("All has %d entries for %q, want 1 (no dup)", count, key)
	}
	if all[key] != "second" {
		t.Fatalf("All[%q] = %v, want latest value 'second'", key, all[key])
	}
	got, ok := m.Read(key)
	if !ok || got != "second" {
		t.Fatalf("Read(%q) = %v,%v, want second,true", key, got, ok)
	}
}

// TestUpsertDistinctKeysNoClobber verifies an agent's Upsert to its own key
// never clobbers a different key, and OwnerOf parses the agent segment.
func TestUpsertDistinctKeysNoClobber(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	ka, err := m.AllocKey("alice")
	if err != nil {
		t.Fatalf("AllocKey alice: %v", err)
	}
	kb, err := m.AllocKey("bob")
	if err != nil {
		t.Fatalf("AllocKey bob: %v", err)
	}
	if err := m.Upsert(ka, "A", "alice"); err != nil {
		t.Fatalf("Upsert alice: %v", err)
	}
	if err := m.Upsert(kb, "B", "bob"); err != nil {
		t.Fatalf("Upsert bob: %v", err)
	}
	if v, ok := m.Read(ka); !ok || v != "A" {
		t.Fatalf("Read(%q) = %v,%v, want A,true", ka, v, ok)
	}
	if v, ok := m.Read(kb); !ok || v != "B" {
		t.Fatalf("Read(%q) = %v,%v, want B,true", kb, v, ok)
	}
	if owner := OwnerOf(ka); owner != "alice" {
		t.Fatalf("OwnerOf(%q) = %q, want alice", ka, owner)
	}
	if owner := OwnerOf(kb); owner != "bob" {
		t.Fatalf("OwnerOf(%q) = %q, want bob", kb, owner)
	}
	if owner := OwnerOf("task:7"); owner != "" {
		t.Fatalf("OwnerOf(legacy task:7) = %q, want empty", owner)
	}
	if owner := OwnerOf("k"); owner != "" {
		t.Fatalf("OwnerOf(non-alloc key) = %q, want empty", owner)
	}
}

// TestConcurrencyStress proves multi-agent parallel writes (AllocKey + Upsert +
// AppendNote) never lose, duplicate, or corrupt entries under concurrent access.
// Safe under `go test -race`: goroutine i writes only keys[i] (no mutex needed
// for the indexed slice) and all shared-state reads are post-WaitGroup.
func TestConcurrencyStress(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	const N = 50
	agents := []string{"claude", "codex", "agy"}

	// keys[i] is written by goroutine i only; no lock needed.
	keys := make([]string, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agent := agents[i%len(agents)]

			// 1. Reserve a collision-free key.
			key, err := m.AllocKey(agent)
			if err != nil {
				t.Errorf("goroutine %d AllocKey: %v", i, err)
				return
			}
			keys[i] = key

			// 2. Write the entry for this goroutine.
			if err := m.Upsert(key, map[string]any{"i": i, "owner": agent}, agent); err != nil {
				t.Errorf("goroutine %d Upsert(%q): %v", i, key, err)
			}

			// 3. Append a note.
			if err := m.AppendNote(agent, fmt.Sprintf("note-%d", i)); err != nil {
				t.Errorf("goroutine %d AppendNote: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// --- post-concurrency assertions ---

	// All N real entries must be present (reservations are hidden by All()).
	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != N {
		t.Fatalf("All len = %d, want %d (lost or duplicate entries)", len(all), N)
	}

	// Every allocated key must be unique.
	keySet := make(map[string]struct{}, N)
	for i, k := range keys {
		if k == "" {
			t.Fatalf("keys[%d] is empty (AllocKey must have errored)", i)
		}
		if _, dup := keySet[k]; dup {
			t.Fatalf("duplicate key at index %d: %q", i, k)
		}
		keySet[k] = struct{}{}
	}
	if len(keySet) != N {
		t.Fatalf("unique key count = %d, want %d", len(keySet), N)
	}

	// Each entry must contain the correct goroutine index and owner (no cross-
	// goroutine clobber). JSON numbers unmarshal as float64.
	for i, key := range keys {
		agent := agents[i%len(agents)]
		v, ok := m.Read(key)
		if !ok {
			t.Fatalf("Read(%q) for goroutine %d: not found", key, i)
		}
		entry, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("Read(%q): value is %T, want map[string]any", key, v)
		}
		gotI, ok := entry["i"].(float64)
		if !ok {
			t.Fatalf("Read(%q)[\"i\"] is %T, want float64", key, entry["i"])
		}
		if int(gotI) != i {
			t.Fatalf("Read(%q)[\"i\"] = %v, want %d (cross-goroutine clobber)", key, gotI, i)
		}
		gotOwner, ok := entry["owner"].(string)
		if !ok {
			t.Fatalf("Read(%q)[\"owner\"] is %T, want string", key, entry["owner"])
		}
		if gotOwner != agent {
			t.Fatalf("Read(%q)[\"owner\"] = %q, want %q", key, gotOwner, agent)
		}
	}

	// All N notes must be present (append-only, none lost).
	notes, err := m.Notes()
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	if len(notes) != N {
		t.Fatalf("Notes len = %d, want %d (some AppendNote calls lost)", len(notes), N)
	}

	// The exact set of note texts must be {note-0 .. note-(N-1)}.
	noteSet := make(map[string]struct{}, N)
	for _, n := range notes {
		nm, ok := n.(map[string]any)
		if !ok {
			t.Fatalf("note is %T, want map[string]any", n)
		}
		text, ok := nm["text"].(string)
		if !ok {
			t.Fatalf("note[\"text\"] is %T, want string", nm["text"])
		}
		noteSet[text] = struct{}{}
	}
	for i := 0; i < N; i++ {
		want := fmt.Sprintf("note-%d", i)
		if _, ok := noteSet[want]; !ok {
			t.Fatalf("note %q missing from Notes()", want)
		}
	}

	// The store must re-load cleanly from disk (persistence + no corruption).
	m2 := New(dir)
	all2, err := m2.All()
	if err != nil {
		t.Fatalf("m2.All: %v", err)
	}
	if len(all2) != N {
		t.Fatalf("m2.All len = %d, want %d (reload lost entries or store corrupt)", len(all2), N)
	}
}

// seedEntry writes a REAL entry wrapper {value,author,ts} directly under key,
// with a caller-chosen ts, by loading and re-saving the store under the lock.
// It is a test-only helper (same package) so tests can control ts determinism
// without a production hook; it reuses the exact acquire/load/save path Compact
// relies on, so what it writes is byte-identical to what Write would persist.
func seedEntry(t *testing.T, m *SharedMemory, key string, value any, author string, ts float64) {
	t.Helper()
	if err := m.acquire(); err != nil {
		t.Fatalf("seedEntry acquire: %v", err)
	}
	defer m.release()
	data, err := m.load()
	if err != nil {
		t.Fatalf("seedEntry load: %v", err)
	}
	data[key] = map[string]any{"value": value, "author": author, "ts": ts}
	if err := m.save(data); err != nil {
		t.Fatalf("seedEntry save: %v", err)
	}
}

// seedReservation writes a value-less _reserved placeholder wrapper directly
// under key with a caller-chosen ts, mirroring what AllocKey persists.
func seedReservation(t *testing.T, m *SharedMemory, key string, ts float64) {
	t.Helper()
	if err := m.acquire(); err != nil {
		t.Fatalf("seedReservation acquire: %v", err)
	}
	defer m.release()
	data, err := m.load()
	if err != nil {
		t.Fatalf("seedReservation load: %v", err)
	}
	data[key] = map[string]any{reservedField: true, "author": "orch", "ts": ts}
	if err := m.save(data); err != nil {
		t.Fatalf("seedReservation save: %v", err)
	}
}

// loadStore returns the raw store map (including control keys) for assertions.
func loadStore(t *testing.T, m *SharedMemory) map[string]any {
	t.Helper()
	if err := m.acquire(); err != nil {
		t.Fatalf("loadStore acquire: %v", err)
	}
	defer m.release()
	data, err := m.load()
	if err != nil {
		t.Fatalf("loadStore load: %v", err)
	}
	return data
}

// countTmp fails if any *.tmp file leaked into dir.
func countTmp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// TestCompactCapFoldsColdTail seeds more real entries than MaxEntries with
// distinct ts and asserts the oldest are folded into _digest, the newest
// MaxEntries survive, and counts are reported correctly.
func TestCompactCapFoldsColdTail(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	// ts 1..5; MaxEntries=2 keeps k4,k5 (newest), folds k1,k2,k3.
	for i := 1; i <= 5; i++ {
		seedEntry(t, m, fmt.Sprintf("task:a:%d", i), fmt.Sprintf("v%d", i), "a", float64(i))
	}

	res, err := m.Compact(CompactOptions{MaxEntries: 2})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.EntriesBefore != 5 {
		t.Fatalf("EntriesBefore = %d, want 5", res.EntriesBefore)
	}
	if res.EntriesAfter != 2 {
		t.Fatalf("EntriesAfter = %d, want 2", res.EntriesAfter)
	}
	if res.Folded != 3 {
		t.Fatalf("Folded = %d, want 3", res.Folded)
	}
	if !res.Digested {
		t.Fatalf("Digested = false, want true")
	}

	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// _digest is a control key; All() excludes it (like _notes), so every key
	// it returns is a real entry.
	if _, leaked := all["_digest"]; leaked {
		t.Fatalf("All() leaked control key _digest: %v", all)
	}
	if len(all) != 2 {
		t.Fatalf("real entries after compact = %d, want 2 (%v)", len(all), all)
	}
	// Newest two must survive; oldest three must be gone as live keys.
	if _, ok := m.Read("task:a:5"); !ok {
		t.Fatalf("newest task:a:5 must survive")
	}
	if _, ok := m.Read("task:a:4"); !ok {
		t.Fatalf("newest task:a:4 must survive")
	}
	if _, ok := m.Read("task:a:1"); ok {
		t.Fatalf("oldest task:a:1 must be folded, not live")
	}

	// A _digest entry must exist with count==3 and body listing the cold tail.
	// It is excluded from All(), so read it explicitly.
	dv, ok := m.Read("_digest")
	if !ok {
		t.Fatalf("_digest missing from store")
	}
	inner, ok := dv.(map[string]any)
	if !ok {
		t.Fatalf("_digest value is %T, want map", dv)
	}
	if c, _ := inner["count"].(float64); int(c) != 3 {
		t.Fatalf("_digest count = %v, want 3", inner["count"])
	}
	body, _ := inner["body"].(string)
	for _, want := range []string{"task:a:1", "task:a:2", "task:a:3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("_digest body missing %q: %q", want, body)
		}
	}
	countTmp(t, dir)
}

// TestCompactKeepsFrontierReservation: a reservation above every kept real key
// may be held by an in-flight dispatch (Go AllocKey or Python
// reserve_task_key) — Compact must keep it so the next allocation cannot hand
// out the same key; superseded reservations still drop.
func TestCompactKeepsFrontierReservation(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	seedEntry(t, m, "task:a:2", "v2", "a", 2.0)
	// Superseded reservation (index below the real task:a:2) and frontier
	// reservation (highest index, as an in-flight AllocKey leaves it).
	seedReservation(t, m, "task:a:1", 1.0)
	seedReservation(t, m, "task:a:3", 3.0)

	if _, err := m.Compact(CompactOptions{MaxEntries: 10}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after := loadStore(t, m)
	if _, ok := after["task:a:1"]; ok {
		t.Fatalf("superseded reservation task:a:1 must be dropped")
	}
	if _, ok := after["task:a:3"]; !ok {
		t.Fatalf("frontier reservation task:a:3 must survive compaction")
	}
	// The next allocation must scan past the kept reservation, not reuse it.
	key, err := m.AllocKey("b")
	if err != nil {
		t.Fatalf("AllocKey: %v", err)
	}
	if key != "task:b:4" {
		t.Fatalf("AllocKey after compact = %q, want task:b:4", key)
	}
	countTmp(t, dir)
}

// TestCompactKeepsCrossPrefixFrontierReservation: the frontier rule is
// per-prefix. A Python "task:4" reservation is not covered by a real
// "task:alice:5" — Python's reserve_task_key scans only its own "task:<n>"
// form, so dropping it against the global max would let Python hand out
// "task:4" to a second dispatcher.
func TestCompactKeepsCrossPrefixFrontierReservation(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	seedEntry(t, m, "task:alice:5", "v5", "alice", 5.0)
	seedEntry(t, m, "task:3", "v3", "py", 3.0)
	// Python-form frontier reservation: above its own prefix max (3), below
	// the global max (5). Must survive.
	seedReservation(t, m, "task:4", 4.0)

	if _, err := m.Compact(CompactOptions{MaxEntries: 10}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after := loadStore(t, m)
	if _, ok := after["task:4"]; !ok {
		t.Fatalf("cross-prefix frontier reservation task:4 must survive compaction")
	}
	countTmp(t, dir)
}

// TestCompactTTLDropsCompletedOld seeds completed-old, completed-fresh, and
// uncompleted-old entries and asserts only completed AND expired ones drop.
func TestCompactTTLDropsCompletedOld(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	now := int64(1000)
	ttl := int64(100) // cutoff = 900; ts < 900 is expired

	completed := func(res string) map[string]any { return map[string]any{"result": res} }
	pending := map[string]any{"result": nil}

	seedEntry(t, m, "task:a:1", completed("done-old"), "a", 800)  // completed + expired -> DROP
	seedEntry(t, m, "task:a:2", completed("done-new"), "a", 950)  // completed + fresh   -> keep
	seedEntry(t, m, "task:a:3", pending, "a", 800)                // old but not completed -> keep
	seedEntry(t, m, "task:a:4", "raw-string", "a", 800)           // no result field       -> keep

	res, err := m.Compact(CompactOptions{TTLSeconds: ttl, Now: now})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.EntriesAfter != 3 {
		t.Fatalf("EntriesAfter = %d, want 3", res.EntriesAfter)
	}
	if _, ok := m.Read("task:a:1"); ok {
		t.Fatalf("completed+expired task:a:1 must be dropped")
	}
	for _, k := range []string{"task:a:2", "task:a:3", "task:a:4"} {
		if _, ok := m.Read(k); !ok {
			t.Fatalf("%s must be kept", k)
		}
	}
	countTmp(t, dir)
}

// TestCompactTrimsNotes seeds more notes than MaxNotes and asserts the last-N
// survive in order.
func TestCompactTrimsNotes(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	for i := 0; i < 5; i++ {
		if err := m.AppendNote("a", fmt.Sprintf("note-%d", i)); err != nil {
			t.Fatalf("AppendNote: %v", err)
		}
	}
	res, err := m.Compact(CompactOptions{MaxNotes: 2})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.NotesBefore != 5 || res.NotesAfter != 2 {
		t.Fatalf("NotesBefore/After = %d/%d, want 5/2", res.NotesBefore, res.NotesAfter)
	}
	notes, err := m.Notes()
	if err != nil {
		t.Fatalf("Notes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("Notes len = %d, want 2", len(notes))
	}
	// Last-N: note-3, note-4 in order.
	for i, want := range []string{"note-3", "note-4"} {
		nm := notes[i].(map[string]any)
		if nm["text"] != want {
			t.Fatalf("note[%d] = %v, want %s", i, nm["text"], want)
		}
	}
	countTmp(t, dir)
}

// TestCompactSummarizeSentinel asserts an injected Summarize supplies the digest
// body verbatim.
func TestCompactSummarizeSentinel(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	for i := 1; i <= 3; i++ {
		seedEntry(t, m, fmt.Sprintf("task:a:%d", i), fmt.Sprintf("v%d", i), "a", float64(i))
	}
	const sentinel = "SUMMARY-SENTINEL-XYZ"
	res, err := m.Compact(CompactOptions{
		MaxEntries: 1,
		Summarize:  func(coldTail string) (string, error) { return sentinel, nil },
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Folded != 2 {
		t.Fatalf("Folded = %d, want 2", res.Folded)
	}
	dv, _ := m.Read("_digest")
	inner := dv.(map[string]any)
	if inner["body"] != sentinel {
		t.Fatalf("_digest body = %q, want sentinel %q", inner["body"], sentinel)
	}
}

// TestCompactSummarizeErrorFallback asserts a failing Summarize falls back to
// the deterministic cold-tail text (no data loss).
func TestCompactSummarizeErrorFallback(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	seedEntry(t, m, "task:a:1", "v1", "a", 1)
	seedEntry(t, m, "task:a:2", "v2", "a", 2)

	res, err := m.Compact(CompactOptions{
		MaxEntries: 1,
		Summarize:  func(string) (string, error) { return "IGNORED", fmt.Errorf("boom") },
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Folded != 1 {
		t.Fatalf("Folded = %d, want 1", res.Folded)
	}
	dv, _ := m.Read("_digest")
	body := dv.(map[string]any)["body"].(string)
	if body == "IGNORED" {
		t.Fatalf("fallback failed: body used errored summarizer output")
	}
	if !strings.Contains(body, "task:a:1") {
		t.Fatalf("fallback body must contain deterministic cold-tail: %q", body)
	}
}

// TestCompactDigestMerges asserts a second Compact extends the existing _digest
// rather than replacing it: counts accumulate and both bodies survive.
func TestCompactDigestMerges(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	seedEntry(t, m, "task:a:1", "v1", "a", 1)
	seedEntry(t, m, "task:a:2", "v2", "a", 2)
	if _, err := m.Compact(CompactOptions{MaxEntries: 1}); err != nil {
		t.Fatalf("Compact 1: %v", err)
	}
	// Now one real entry (task:a:2) + a digest with count 1. Add another old
	// entry and compact again.
	seedEntry(t, m, "task:a:3", "v3", "a", 3)
	// ts order: task:a:2 (ts2), task:a:3 (ts3); MaxEntries=1 folds the oldest
	// live real entry (task:a:2) into the digest.
	res, err := m.Compact(CompactOptions{MaxEntries: 1})
	if err != nil {
		t.Fatalf("Compact 2: %v", err)
	}
	if !res.Digested {
		t.Fatalf("Digested = false, want true")
	}
	dv, _ := m.Read("_digest")
	inner := dv.(map[string]any)
	if c, _ := inner["count"].(float64); int(c) != 2 {
		t.Fatalf("merged _digest count = %v, want 2", inner["count"])
	}
	body := inner["body"].(string)
	if !strings.Contains(body, "task:a:1") || !strings.Contains(body, "task:a:2") {
		t.Fatalf("merged _digest body must contain both folds: %q", body)
	}
}

// TestMaybeCompactThreshold asserts MaybeCompact only fires over-threshold.
func TestMaybeCompactThreshold(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	seedEntry(t, m, "task:a:1", "v1", "a", 1)
	seedEntry(t, m, "task:a:2", "v2", "a", 2)

	// Under threshold: no compaction.
	did, err := m.MaybeCompact(CompactOptions{MaxEntries: 5})
	if err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if did {
		t.Fatalf("MaybeCompact fired under threshold")
	}

	// Over threshold: fires.
	did, err = m.MaybeCompact(CompactOptions{MaxEntries: 1})
	if err != nil {
		t.Fatalf("MaybeCompact over: %v", err)
	}
	if !did {
		t.Fatalf("MaybeCompact did not fire over threshold")
	}
	all, _ := m.All()
	realCount := 0
	for k := range all {
		if k != "_digest" {
			realCount++
		}
	}
	if realCount != 1 {
		t.Fatalf("after MaybeCompact real entries = %d, want 1", realCount)
	}
}

// TestCompactConcurrentWriter runs a writer goroutine (AllocKey+Upsert) against
// a concurrent Compact and asserts the store parses cleanly afterward, no *.tmp
// leaks, and the writer's entry is present live or legitimately folded into the
// digest — never lost or corrupt. Both paths take the same flock, so each is
// fully serialized before or after the other.
func TestCompactConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	// Seed a baseline so Compact has real work regardless of interleaving.
	for i := 1; i <= 4; i++ {
		seedEntry(t, m, fmt.Sprintf("task:seed:%d", i), fmt.Sprintf("v%d", i), "seed", float64(i))
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var writerKey string
	var writerErr error
	go func() {
		defer wg.Done()
		k, err := m.AllocKey("writer")
		if err != nil {
			writerErr = err
			return
		}
		writerKey = k
		if err := m.Upsert(k, "written", "writer"); err != nil {
			writerErr = err
		}
	}()

	var compactErr error
	go func() {
		defer wg.Done()
		_, err := m.Compact(CompactOptions{MaxEntries: 2, MaxNotes: 10})
		compactErr = err
	}()

	wg.Wait()
	if writerErr != nil {
		t.Fatalf("writer: %v", writerErr)
	}
	if compactErr != nil {
		t.Fatalf("compact: %v", compactErr)
	}

	// Store must reload cleanly from a fresh instance (no corruption).
	m2 := New(dir)
	all, err := m2.All()
	if err != nil {
		t.Fatalf("reload All: %v", err)
	}

	// The writer's entry is either live or folded into the digest — never lost.
	_, live := all[writerKey]
	foldedIn := false
	if dv, ok := all["_digest"].(map[string]any); ok {
		if body, ok := dv["body"].(string); ok && strings.Contains(body, writerKey) {
			foldedIn = true
		}
	}
	if !live && !foldedIn {
		t.Fatalf("writer key %q neither live nor folded into digest: %v", writerKey, all)
	}
	countTmp(t, dir)
}

// TestConcurrentWritesNoCorruption spins concurrent Writes through the flock
// path and asserts the store stays valid JSON with every key present — flock
// must serialize the read-modify-write.
func TestConcurrentWritesNoCorruption(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := m.Write(fmt.Sprintf("k%d", i), i, "w"); err != nil {
				t.Errorf("Write k%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != n {
		t.Fatalf("All len = %d, want %d (lost updates -> corruption)", len(all), n)
	}
}
