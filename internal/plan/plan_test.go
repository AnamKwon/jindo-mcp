package plan

import (
	"testing"
)

// steps builds a small plan: s1 (no deps), s2 depends on s1.
func twoStep() []Step {
	return []Step{
		{ID: "s1", Title: "first", Prompt: "do first", Difficulty: "standard", SuggestedVerify: []string{"go build ./..."}},
		{ID: "s2", Title: "second", Prompt: "do second", Difficulty: "hard", DependsOn: []string{"s1"}},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Save("the goal", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, ok := m.Load()
	if !ok {
		t.Fatalf("Load: ok=false after Save")
	}
	if st.Goal != "the goal" {
		t.Errorf("goal = %q, want %q", st.Goal, "the goal")
	}
	if len(st.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(st.Steps))
	}
	if st.Steps[0].Status != StatusPending || st.Steps[1].Status != StatusPending {
		t.Errorf("saved steps not defaulted to pending: %+v", st.Steps)
	}
	if st.Steps[1].Prompt != "do second" || st.Steps[1].DependsOn[0] != "s1" {
		t.Errorf("step fields did not round-trip: %+v", st.Steps[1])
	}
	if st.CreatedAt.IsZero() {
		t.Errorf("CreatedAt not set")
	}
}

func TestLoadAbsent(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, ok := m.Load(); ok {
		t.Fatalf("Load: ok=true with no plan.json")
	}
}

func TestNextOrderAndDeps(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// s1 is runnable first (no deps); s2 is blocked on s1.
	step, remaining, ok := m.Next()
	if !ok || step.ID != "s1" {
		t.Fatalf("Next = (%q,%v), want s1 runnable", step.ID, ok)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2", remaining)
	}

	// Record s1 done -> s2 becomes runnable.
	if _, err := m.Record("s1", StatusDone, "ok"); err != nil {
		t.Fatalf("Record s1: %v", err)
	}
	step, remaining, ok = m.Next()
	if !ok || step.ID != "s2" {
		t.Fatalf("Next after s1 done = (%q,%v), want s2 runnable", step.ID, ok)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1", remaining)
	}
}

func TestNextBlockedReturnsRemaining(t *testing.T) {
	m := NewManager(t.TempDir())
	// Only s2, which depends on a never-satisfied s1: pending but not runnable.
	if err := m.Save("g", []Step{{ID: "s2", Prompt: "p", DependsOn: []string{"s1"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	step, remaining, ok := m.Next()
	if ok {
		t.Fatalf("Next = %q runnable, want blocked", step.ID)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1 (blocked, not complete)", remaining)
	}
}

func TestNextAllDone(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := m.Record("s1", StatusDone, ""); err != nil {
		t.Fatalf("Record s1: %v", err)
	}
	if _, err := m.Record("s2", StatusDone, ""); err != nil {
		t.Fatalf("Record s2: %v", err)
	}
	_, remaining, ok := m.Next()
	if ok {
		t.Fatalf("Next returned runnable step when all done")
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
}

// setMaxAttempts overrides the package attempt cap for the duration of a test,
// restoring the prior value on cleanup so retry tests stay fast and isolated.
func setMaxAttempts(t *testing.T, n int) {
	t.Helper()
	prev := MaxAttempts
	MaxAttempts = n
	t.Cleanup(func() { MaxAttempts = prev })
}

func TestNextReoffersFailedUnderCap(t *testing.T) {
	setMaxAttempts(t, 2)
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// s1 fails once; under the cap (attempts 1 < 2) it must be re-offered.
	if _, err := m.Record("s1", StatusFailed, "boom"); err != nil {
		t.Fatalf("Record s1 failed: %v", err)
	}
	step, remaining, ok := m.Next()
	if !ok || step.ID != "s1" {
		t.Fatalf("Next after 1 failure = (%q,%v), want s1 re-offered", step.ID, ok)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2 (neither step done)", remaining)
	}
}

func TestNextBlocksFailedAtCap(t *testing.T) {
	setMaxAttempts(t, 2)
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Fail s1 up to the cap; it is then terminally blocked, no longer offered.
	for i := 0; i < 2; i++ {
		if _, err := m.Record("s1", StatusFailed, "boom"); err != nil {
			t.Fatalf("Record s1 failed: %v", err)
		}
	}
	step, remaining, ok := m.Next()
	if ok {
		t.Fatalf("Next = %q runnable, want blocked (s1 hit cap, s2 waits on s1)", step.ID)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2 (blocked, not complete)", remaining)
	}
}

func TestNextGatesDependentOnRetryableDep(t *testing.T) {
	setMaxAttempts(t, 3)
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// s1 failed but retryable: it is re-offered, but s2 (depends_on s1) must NOT
	// be — a retryable failed dep is not done, so dependents keep waiting.
	if _, err := m.Record("s1", StatusFailed, "boom"); err != nil {
		t.Fatalf("Record s1 failed: %v", err)
	}
	step, _, ok := m.Next()
	if !ok || step.ID != "s1" {
		t.Fatalf("Next = (%q,%v), want s1 (retryable), never s2", step.ID, ok)
	}
}

func TestRecordDoneFailedAttempts(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A failed record increments Attempts and sets the note.
	s, err := m.Record("s1", StatusFailed, "boom")
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}
	if s.Status != StatusFailed || s.Attempts != 1 || s.Note != "boom" {
		t.Errorf("failed record = %+v, want failed/attempts=1/note=boom", s)
	}
	// Retry then fail again -> attempts=2.
	s, _ = m.Record("s1", StatusFailed, "again")
	if s.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", s.Attempts)
	}
	// Done does not increment attempts.
	s, _ = m.Record("s1", StatusDone, "fixed")
	if s.Status != StatusDone || s.Attempts != 2 {
		t.Errorf("done record = %+v, want done/attempts=2", s)
	}

	// Unknown id is an error.
	if _, err := m.Record("nope", StatusDone, ""); err == nil {
		t.Fatalf("Record unknown id: want error")
	}
}

func TestRevise(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Save("g", twoStep()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Finish s1 so we can confirm update does not reset a done step.
	if _, err := m.Record("s1", StatusDone, "done"); err != nil {
		t.Fatalf("Record s1: %v", err)
	}

	add := []Step{{ID: "s3", Prompt: "new work"}}
	update := []Step{
		{ID: "s1", Title: "renamed"},         // status empty => stays done
		{ID: "s2", Prompt: "revised second"}, // still pending
	}
	if err := m.Revise(add, update, []string{}); err != nil {
		t.Fatalf("Revise: %v", err)
	}

	st, _ := m.Load()
	byID := map[string]Step{}
	for _, s := range st.Steps {
		byID[s.ID] = s
	}
	if byID["s1"].Status != StatusDone {
		t.Errorf("s1 status = %q, want done (update must not reset)", byID["s1"].Status)
	}
	if byID["s1"].Title != "renamed" {
		t.Errorf("s1 title = %q, want renamed", byID["s1"].Title)
	}
	if byID["s2"].Prompt != "revised second" {
		t.Errorf("s2 prompt = %q, want revised second", byID["s2"].Prompt)
	}
	if byID["s3"].Status != StatusPending {
		t.Errorf("added s3 status = %q, want pending", byID["s3"].Status)
	}

	// Remove s2.
	if err := m.Revise(nil, nil, []string{"s2"}); err != nil {
		t.Fatalf("Revise remove: %v", err)
	}
	st, _ = m.Load()
	for _, s := range st.Steps {
		if s.ID == "s2" {
			t.Fatalf("s2 still present after remove")
		}
	}

	// After s1 done and s2 removed, the added s3 (no deps) is next runnable.
	step, _, ok := m.Next()
	if !ok || step.ID != "s3" {
		t.Fatalf("Next after revise = (%q,%v), want s3", step.ID, ok)
	}
}
