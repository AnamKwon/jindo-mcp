package jobs

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSubmitWaitDone(t *testing.T) {
	m := NewManager(t.TempDir())

	id := m.Submit(func() (map[string]any, error) {
		return map[string]any{"answer": 42}, nil
	})

	j, ok := m.Wait(id, time.Second)
	if !ok {
		t.Fatalf("Wait ok=false for known id")
	}
	if j.Status != StatusDone {
		t.Fatalf("status = %q, want %q", j.Status, StatusDone)
	}
	if j.Err != "" {
		t.Fatalf("Err = %q, want empty", j.Err)
	}
	if got := j.Result["answer"]; got != 42 {
		t.Fatalf("Result[answer] = %v, want 42", got)
	}
	if j.FinishedAt.IsZero() {
		t.Fatalf("FinishedAt not set on terminal job")
	}
	if j.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt not set")
	}
}

func TestSubmitError(t *testing.T) {
	m := NewManager(t.TempDir())

	id := m.Submit(func() (map[string]any, error) {
		return nil, errors.New("boom")
	})

	j, ok := m.Wait(id, time.Second)
	if !ok {
		t.Fatalf("Wait ok=false for known id")
	}
	if j.Status != StatusError {
		t.Fatalf("status = %q, want %q", j.Status, StatusError)
	}
	if j.Err != "boom" {
		t.Fatalf("Err = %q, want %q", j.Err, "boom")
	}
	if j.Result != nil {
		t.Fatalf("Result = %v, want nil on error", j.Result)
	}
}

func TestWaitTimeoutRunningSnapshot(t *testing.T) {
	m := NewManager(t.TempDir())

	release := make(chan struct{})
	id := m.Submit(func() (map[string]any, error) {
		<-release
		return map[string]any{"ok": true}, nil
	})
	// Let the job finish and stop touching the temp dir before the test ends,
	// so TempDir cleanup does not race the worker's persist.
	defer func() {
		close(release)
		m.Wait(id, time.Second)
	}()

	start := time.Now()
	j, ok := m.Wait(id, 20*time.Millisecond)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("Wait ok=false for known running id")
	}
	if j.Status != StatusRunning {
		t.Fatalf("status = %q, want %q on timeout", j.Status, StatusRunning)
	}
	if j.Result != nil {
		t.Fatalf("Result = %v, want nil while running", j.Result)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("Wait returned after %v, want >= timeout", elapsed)
	}
}

func TestUnknownID(t *testing.T) {
	m := NewManager(t.TempDir())

	if _, ok := m.Get("does-not-exist"); ok {
		t.Fatalf("Get ok=true for unknown id")
	}
	if _, ok := m.Wait("does-not-exist", 10*time.Millisecond); ok {
		t.Fatalf("Wait ok=true for unknown id")
	}
}

func TestPersistenceSurvivesFreshManager(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)

	id := m.Submit(func() (map[string]any, error) {
		return map[string]any{"n": "value"}, nil
	})
	if _, ok := m.Wait(id, time.Second); !ok {
		t.Fatalf("job did not complete")
	}

	// A fresh Manager over the same root has no in-memory record and must
	// recover the terminal job from disk.
	fresh := NewManager(root)
	j, ok := fresh.Get(id)
	if !ok {
		t.Fatalf("fresh Get ok=false; persisted job not recovered")
	}
	if j.Status != StatusDone {
		t.Fatalf("recovered status = %q, want %q", j.Status, StatusDone)
	}
	if got := j.Result["n"]; got != "value" {
		t.Fatalf("recovered Result[n] = %v, want %q", got, "value")
	}
}

func TestGetReturnsCopyNotLivePointer(t *testing.T) {
	m := NewManager(t.TempDir())
	id := m.Submit(func() (map[string]any, error) {
		return map[string]any{"x": 1}, nil
	})
	if _, ok := m.Wait(id, time.Second); !ok {
		t.Fatalf("job did not complete")
	}

	a, _ := m.Get(id)
	b, _ := m.Get(id)
	if a == b {
		t.Fatalf("Get returned the same pointer twice; must be a copy")
	}
	// Mutating a returned copy's scalar fields must not affect a later Get.
	a.Status = "tampered"
	c, _ := m.Get(id)
	if c.Status != StatusDone {
		t.Fatalf("mutation of returned copy leaked into store: %q", c.Status)
	}
}

func TestConcurrentSubmitsAndWaits(t *testing.T) {
	m := NewManager(t.TempDir())

	const n = 50
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		i := i
		ids[i] = m.Submit(func() (map[string]any, error) {
			time.Sleep(time.Duration(i%5) * time.Millisecond)
			return map[string]any{"i": i}, nil
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, ok := m.Wait(ids[i], time.Second)
			if !ok {
				t.Errorf("Wait ok=false for id %d", i)
				return
			}
			if j.Status != StatusDone {
				t.Errorf("job %d status = %q, want done", i, j.Status)
				return
			}
			if got := j.Result["i"]; got != i {
				t.Errorf("job %d Result[i] = %v, want %d", i, got, i)
			}
		}()
	}
	// Concurrent readers via Get while jobs may still be completing.
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := m.Get(ids[i]); !ok {
				t.Errorf("Get ok=false for id %d", i)
			}
		}()
	}
	wg.Wait()

	// All ids must be distinct.
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = struct{}{}
	}
}

// Confirms best-effort persistence: an unwritable root must not fail the job.
func TestUnwritableRootDoesNotFailJob(t *testing.T) {
	// A path whose parent is a regular file cannot host a directory, so
	// MkdirAll fails and persist must swallow it.
	bad := fmt.Sprintf("%s/not-a-dir.txt/sub", t.TempDir())
	m := NewManager(bad)
	id := m.Submit(func() (map[string]any, error) {
		return map[string]any{"ok": 1}, nil
	})
	j, ok := m.Wait(id, time.Second)
	if !ok || j.Status != StatusDone {
		t.Fatalf("job did not succeed despite unwritable root: ok=%v status=%q", ok, j.Status)
	}
}
