// Package jobs is the concurrency core of async dispatch: a Manager runs
// submitted work in background goroutines, lets callers poll or block for
// completion, and persists terminal jobs so their results survive a restart.
//
// Invariants:
//   - No unlocked mutation of a Job is ever visible to a reader. Every field
//     write happens under the Manager mutex, and the write set is published as
//     terminal before the job's done channel is closed.
//   - Readers (Get, Wait) always receive a COPY of the Job, never the live
//     pointer the goroutine mutates.
//   - Disk failures never fail a job; persistence and loading are best-effort.
//   - Only terminal ("done"/"error") jobs are persisted, so only they need to
//     survive a restart. A "running" job has no meaning across process death.
package jobs

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Job status values. A job starts running and reaches exactly one terminal
// state, done or error.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
)

// Job is a single unit of dispatched work. It is serialized to disk for
// terminal jobs, so every exported field carries a JSON tag. Result is nil
// until the job completes successfully; Err is set only on failure.
type Job struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Result     map[string]any `json:"result,omitempty"`
	Err        string         `json:"err,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
}

// jobState pairs the live Job the worker goroutine mutates with the channel it
// closes on completion. The channel is kept out of Job so it is neither
// serialized nor copied into snapshots handed to readers.
type jobState struct {
	job  *Job
	done chan struct{}
}

// Manager tracks jobs in memory under a single mutex and persists terminal
// jobs under <root>/jobs. The zero value is not usable; construct with
// NewManager.
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*jobState
	root string
}

// NewManager returns a Manager that persists completed jobs under
// <root>/jobs/<id>.json. The directory is created lazily on first persist and
// all disk access is best-effort, so an unwritable root degrades to
// memory-only operation rather than failing.
func NewManager(root string) *Manager {
	return &Manager{
		jobs: make(map[string]*jobState),
		root: root,
	}
}

// Submit registers a new running job, runs work in a background goroutine, and
// returns the job id immediately. When work returns, the goroutine publishes
// the terminal state under the lock (status, result/err, finish time),
// best-effort persists it, and signals any waiters. work must not be nil.
func (m *Manager) Submit(work func() (map[string]any, error)) string {
	now := time.Now()

	m.mu.Lock()
	// Generate the id and insert inside the same critical section so the
	// uniqueness check cannot race another Submit. A 128-bit random id makes
	// collisions astronomically unlikely; the loop guards against them anyway.
	var id string
	for {
		id = randHex()
		if _, exists := m.jobs[id]; !exists {
			break
		}
	}
	js := &jobState{
		job:  &Job{ID: id, Status: StatusRunning, CreatedAt: now},
		done: make(chan struct{}),
	}
	m.jobs[id] = js
	m.mu.Unlock()

	go func() {
		res, err := work()

		m.mu.Lock()
		js.job.FinishedAt = time.Now()
		if err != nil {
			js.job.Status = StatusError
			js.job.Err = err.Error()
		} else {
			js.job.Status = StatusDone
			js.job.Result = res
		}
		// Persist while still holding the lock so the snapshot written to disk
		// is consistent with the state readers observe. Best-effort: a disk
		// error must not affect the job.
		m.persist(js.job)
		// Publish completion only after all fields are set and persisted, so a
		// waiter that wakes on this channel always sees a terminal snapshot.
		close(js.done)
		m.mu.Unlock()
	}()

	return id
}

// Get returns a copy of the job with the given id. It checks memory first;
// if absent it falls back to a best-effort load of the persisted file, so a
// terminal job survives a Manager restart over the same root. The returned
// pointer is a fresh copy and is never the live job the worker mutates.
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	js, ok := m.jobs[id]
	if ok {
		cp := *js.job
		m.mu.Unlock()
		return &cp, true
	}
	m.mu.Unlock()

	// Not in memory: it may be a terminal job from a previous run. Disk access
	// touches no shared state, so it needs no lock.
	return m.load(id)
}

// Wait returns a copy of the job with the given id, blocking up to timeout for
// it to reach a terminal state. If the id is already terminal it returns
// immediately. If the timeout elapses first it returns the current snapshot
// (possibly still "running") with ok=true. An unknown id returns ok=false.
func (m *Manager) Wait(id string, timeout time.Duration) (*Job, bool) {
	m.mu.Lock()
	js, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, false
	}
	if isTerminal(js.job.Status) {
		cp := *js.job
		m.mu.Unlock()
		return &cp, true
	}
	// Capture the channel under the lock, then release it before blocking so
	// the worker goroutine can take the lock to complete the job. The channel
	// value is stable for the life of the job.
	done := js.done
	m.mu.Unlock()

	select {
	case <-done:
	case <-time.After(timeout):
	}

	m.mu.Lock()
	cp := *js.job
	m.mu.Unlock()
	return &cp, true
}

// isTerminal reports whether a status is a final state.
func isTerminal(status string) bool {
	return status == StatusDone || status == StatusError
}

// jobPath is the on-disk location of a job's persisted snapshot.
func (m *Manager) jobPath(id string) string {
	return filepath.Join(m.root, "jobs", id+".json")
}

// persist writes a terminal job to disk. It is best-effort: any failure
// (unwritable root, marshal error) is swallowed so the job still succeeds.
// Callers hold m.mu.
func (m *Manager) persist(j *Job) {
	dir := filepath.Join(m.root, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	buf, err := json.Marshal(j)
	if err != nil {
		return
	}
	_ = os.WriteFile(m.jobPath(j.ID), buf, 0o644)
}

// load reads a persisted job from disk. It is best-effort: a missing or
// corrupt file simply reports ok=false.
func (m *Manager) load(id string) (*Job, bool) {
	buf, err := os.ReadFile(m.jobPath(id))
	if err != nil {
		return nil, false
	}
	var j Job
	if err := json.Unmarshal(buf, &j); err != nil {
		return nil, false
	}
	return &j, true
}

// randHex returns a 32-character hex id from 16 random bytes. crypto/rand
// effectively never fails on supported platforms; if it does, the id falls
// back to a timestamp so Submit still yields a usable (if less random) id
// rather than panicking a library caller.
func randHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}
