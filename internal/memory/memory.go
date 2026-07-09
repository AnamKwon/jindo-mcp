// Package memory is a Go port of jindo/memory.py's SharedMemory: a file-based
// shared memory store backed by a single memory.json under a root directory,
// with cross-instance file locking around every mutation and read so callers
// observe a consistent snapshot.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SharedMemory is persistent shared memory backed by a single JSON file with
// file-level locking. Safe across multiple processes/instances — all public
// operations acquire the lock before reading, modifying, and (for mutations)
// atomically replacing memory.json.
type SharedMemory struct {
	root      string
	storePath string
	lockPath  string

	// mu guards lockFile so acquire()/release() on the same instance don't race
	// over the held fd. The cross-process serialization is done by flock itself.
	mu       sync.Mutex
	lockFile *os.File
}

// notesKey is the reserved key whose value is the notes list, excluded from
// All(). Mirrors "_notes" in memory.py.
const notesKey = "_notes"

// reservedField marks a placeholder entry created by AllocKey to reserve a key
// under the lock. Such entries are skipped by All() and Read() until real
// content is Upsert'd over them, so a reservation never surfaces as a real
// value but still participates in nextN computation while it lives.
const reservedField = "_reserved"

// LockTimeout bounds how long acquire() spins waiting for the lock before
// giving up with an error. It is a package-level var (not a const) so an
// orchestrator can tune it under load, and tests can shrink it. Unlike the
// Python side, which blocks indefinitely on its FileLock, this Go port
// fail-fasts: a mutation/read that cannot win the flock within LockTimeout
// returns an error rather than stalling dispatch forever. The default is
// generous enough to ride out normal contention (many short-held locks) while
// still surfacing a genuine deadlock/stuck holder instead of hanging.
// lockPoll is the retry interval between non-blocking attempts.
var LockTimeout = 10 * time.Second

const lockPoll = 2 * time.Millisecond

// New returns a SharedMemory rooted at root, creating the directory if needed.
func New(root string) *SharedMemory {
	// Mirror Python's mkdir(parents=True, exist_ok=True). Ignore the error to
	// keep New non-fallible like __init__; a genuinely unusable root surfaces
	// later when acquire()/load()/save() fail.
	_ = os.MkdirAll(root, 0o755)
	return &SharedMemory{
		root:      root,
		storePath: filepath.Join(root, "memory.json"),
		lockPath:  filepath.Join(root, "memory.lock"),
	}
}

// Root returns the root directory backing this store. It is the bounded
// directory an orchestrator hands to a headless agent (via a system prompt and
// --add-dir) so the agent can read prior context itself, without the
// orchestrator pulling memory.json content into its own process.
func (m *SharedMemory) Root() string {
	return m.root
}

// Write stores value under key, tagging it with author and a timestamp, then
// atomically persists the store.
func (m *SharedMemory) Write(key string, value any, author string) error {
	if err := m.acquire(); err != nil {
		return err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return err
	}
	data[key] = map[string]any{
		"value":  value,
		"author": author,
		"ts":     float64(time.Now().Unix()),
	}
	return m.save(data)
}

// Read returns the stored value for key and true, or (nil, false) if the key is
// absent, is the reserved notes key, or the stored entry is malformed.
func (m *SharedMemory) Read(key string) (any, bool) {
	if key == notesKey || key == insightsKey {
		return nil, false
	}
	if err := m.acquire(); err != nil {
		return nil, false
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return nil, false
	}
	entry, ok := data[key]
	if !ok {
		return nil, false
	}
	m2, ok := entry.(map[string]any)
	if !ok {
		return nil, false
	}
	if r, ok := m2[reservedField].(bool); ok && r {
		// A bare reservation has no real value yet.
		return nil, false
	}
	v, ok := m2["value"]
	if !ok {
		return nil, false
	}
	return v, true
}

// All returns a mapping of every user key to its unwrapped value, excluding the
// reserved control keys (_notes and _digest). Mirrors memory.py's all().
func (m *SharedMemory) All() (map[string]any, error) {
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any)
	for k, entry := range data {
		if k == notesKey || k == digestKey || k == insightsKey {
			// Control keys, not real task entries: _notes holds the notes list,
			// _digest holds the compaction fold (see compaction.go), and
			// _insights holds the cross-agent insight layer (see insights.go).
			// All are excluded so callers iterating All() see only user entries.
			continue
		}
		if m2, ok := entry.(map[string]any); ok {
			if r, ok := m2[reservedField].(bool); ok && r {
				// Skip bare AllocKey reservations; they are transient until
				// real content is Upsert'd over the same key.
				continue
			}
			if v, ok := m2["value"]; ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

// keyN parses the trailing integer index of an allocation key. It accepts both
// "task:<n>" (legacy/global) and "task:<agent>:<n>" (agent-scoped) forms and
// returns the parsed n and true; any other shape returns (0, false).
func keyN(key string) (int, bool) {
	if !strings.HasPrefix(key, "task:") {
		return 0, false
	}
	rest := key[len("task:"):]
	// The index is always the final ':'-separated segment.
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		rest = rest[i+1:]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// AllocKey reserves and returns a fresh, collision-free task key of the form
// "task:<agent>:<n>". Under the lock it loads the store, computes n as one past
// the maximum index across every existing "task:<n>" and "task:<agent>:<n>"
// key, then persists a reservation placeholder under the new key BEFORE
// releasing the lock. Because the placeholder is durable before release, the
// next allocation — even from a fresh SharedMemory over the same root, or a
// concurrent caller — observes it in the max and cannot reuse the index.
func (m *SharedMemory) AllocKey(agent string) (string, error) {
	if err := m.acquire(); err != nil {
		return "", err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return "", err
	}
	maxN := 0
	for k := range data {
		if n, ok := keyN(k); ok && n > maxN {
			maxN = n
		}
	}
	nextN := maxN + 1
	key := fmt.Sprintf("task:%s:%d", agent, nextN)
	data[key] = map[string]any{
		reservedField: true,
		"author":      agent,
		"ts":          float64(time.Now().Unix()),
	}
	if err := m.save(data); err != nil {
		return "", err
	}
	return key, nil
}

// Upsert idempotently stores value under key, tagging it with author and a
// timestamp. Callers use a stable key (e.g. an AllocKey'd "task:<agent>:<n>" or
// a dispatch-id-derived key) so a retry overwrites the same entry in place
// rather than creating a duplicate. It also promotes an AllocKey reservation
// placeholder to real content. Semantically identical persistence to Write; the
// distinct name signals idempotent-by-stable-key intent at the call site.
func (m *SharedMemory) Upsert(key string, value any, author string) error {
	return m.Write(key, value, author)
}

// OwnerOf returns the agent segment of an agent-scoped key "task:<agent>:<n>",
// or "" for the legacy "task:<n>" form or any non-allocation key.
func OwnerOf(key string) string {
	if _, ok := keyN(key); !ok {
		return ""
	}
	rest := key[len("task:"):]
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		// "task:<n>" — no agent segment.
		return ""
	}
	return rest[:i]
}

// AppendNote appends a note {author, text, ts} to the reserved notes list.
func (m *SharedMemory) AppendNote(author, text string) error {
	if err := m.acquire(); err != nil {
		return err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return err
	}
	var notes []any
	if existing, ok := data[notesKey].([]any); ok {
		notes = existing
	}
	notes = append(notes, map[string]any{
		"author": author,
		"text":   text,
		"ts":     float64(time.Now().Unix()),
	})
	data[notesKey] = notes
	return m.save(data)
}

// Notes returns the notes list in append order, or an empty slice if absent.
func (m *SharedMemory) Notes() ([]any, error) {
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return nil, err
	}
	if notes, ok := data[notesKey].([]any); ok {
		return notes, nil
	}
	return []any{}, nil
}

// Stats reports dispatch-time injection context without pulling entry content
// into the caller: records is the count of keyed data entries as All() would
// return them (reserved control keys _notes/_digest and transient AllocKey
// placeholders excluded), and hasDigest reports whether a _digest entry is
// present. Used by callers that need to decide what to inject without loading
// full entry values.
func (m *SharedMemory) Stats() (records int, hasDigest bool, err error) {
	if err := m.acquire(); err != nil {
		return 0, false, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return 0, false, err
	}
	_, hasDigest = data[digestKey]
	for k, entry := range data {
		if k == notesKey || k == digestKey || k == insightsKey {
			continue
		}
		if m2, ok := entry.(map[string]any); ok {
			if r, ok := m2[reservedField].(bool); ok && r {
				continue
			}
		}
		records++
	}
	return records, hasDigest, nil
}

// load reads memory.json into a map. A missing file yields an empty map; a
// corrupt/unreadable file is tolerated as an empty map, mirroring _read_raw.
func (m *SharedMemory) load() (map[string]any, error) {
	raw, err := os.ReadFile(m.storePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]any), nil
		}
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		// Tolerate corruption like Python's except (JSONDecodeError, OSError).
		return make(map[string]any), nil
	}
	if data == nil {
		data = make(map[string]any)
	}
	return data, nil
}

// save writes data to a temp file in the same directory and atomically renames
// it onto memory.json. On any failure the temp file is removed so no stray
// *.tmp files leak.
func (m *SharedMemory) save(data map[string]any) error {
	buf, err := json.Marshal(data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.root, "memory-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// CreateTemp yields 0600; match the 0644 a Python-side write produces so
	// cross-process readers keep working after a Go write wins the race.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush to stable storage before the rename so a crash cannot leave
	// memory.json pointing at a partially-written file (parity with the
	// Python side's flush+fsync before os.replace).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, m.storePath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// acquire takes an exclusive advisory (flock) lock on the lock file. It opens
// (creating if needed) the lock file, then spins on a non-blocking LOCK_EX,
// retrying while another holder blocks it, until it wins or lockTimeout
// elapses. On success the held *os.File is stored so release() can unlock it.
// Unlike the previous O_EXCL create-and-remove scheme, flock is released by the
// kernel when the holding process dies, so a crashed holder never deadlocks the
// store — that is the crash-safety win, and why release() must NOT remove the
// lock file.
func (m *SharedMemory) acquire() error {
	fd, err := os.OpenFile(m.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(LockTimeout)
	for {
		err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			m.mu.Lock()
			m.lockFile = fd
			m.mu.Unlock()
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			fd.Close()
			return err
		}
		if time.Now().After(deadline) {
			fd.Close()
			return errors.New("memory: timed out acquiring lock " + m.lockPath)
		}
		time.Sleep(lockPoll)
	}
}

// release drops the advisory lock and closes the fd. It deliberately does NOT
// remove the lock file: the file's existence is harmless, and keeping it lets
// flock's kernel-side auto-release remain the sole owner of lock lifetime.
func (m *SharedMemory) release() {
	m.mu.Lock()
	fd := m.lockFile
	m.lockFile = nil
	m.mu.Unlock()
	if fd == nil {
		return
	}
	syscall.Flock(int(fd.Fd()), syscall.LOCK_UN)
	fd.Close()
}
