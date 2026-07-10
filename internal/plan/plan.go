// Package plan is the persistent, step-gated state behind jindo's plan-execution
// loop. A Manager holds one active plan under a root directory and serializes it
// to <root>/plan.json, so a host can drive the plan ONE step at a time: ask for
// the next runnable step (Next), dispatch it, record the outcome (Record), and
// adapt the remaining plan (Revise) — surviving a process restart in between.
//
// Invariants:
//   - Every read-modify-write of the plan happens under the Manager mutex, so a
//     concurrent Record/Revise never observes or writes a torn plan.
//   - Persistence is best-effort-robust: a corrupt or absent plan.json makes
//     Load report ok=false rather than panicking, and a write failure surfaces as
//     the operation's error without corrupting the in-memory intent.
//   - A step is runnable only when it is pending AND every id in its DependsOn is
//     done; Next returns steps in slice order, so the plan's authored order is
//     the execution order among otherwise-runnable steps.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Step status values. A step starts pending and is driven to a terminal
// done/failed by Record; failed steps may be retried (Attempts tracks tries).
const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Step is one node of the active plan. It mirrors agentproto.PlanStep's authored
// fields (id/title/prompt/difficulty/suggested_model/suggested_verify/depends_on)
// and adds the execution state the loop advances: Status, Attempts, and a free
// Note recording the last outcome. Every field is serialized, so a step round-
// trips through plan.json unchanged.
type Step struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	Difficulty      string   `json:"difficulty"`
	SuggestedModel  string   `json:"suggested_model"`
	SuggestedVerify []string `json:"suggested_verify"`
	DependsOn       []string `json:"depends_on"`
	Status          string   `json:"status"`
	Attempts        int      `json:"attempts"`
	Note            string   `json:"note,omitempty"`
}

// State is the whole active plan: the goal it decomposes, the caller's clarified
// intent (Spec) anchoring re-plans, the integration gate proving the whole goal
// (VerifyCmds), its ordered steps, and when it was established. It is exactly
// what plan.json holds.
type State struct {
	Goal string `json:"goal"`
	// Spec is the caller-provided clarified intent anchoring re-plans; empty
	// when none was supplied.
	Spec string `json:"spec,omitempty"`
	// VerifyCmds is the integration gate for the WHOLE goal (distinct from each
	// step's SuggestedVerify, which gates only that step).
	VerifyCmds []string  `json:"verify_cmds,omitempty"`
	Steps      []Step    `json:"steps"`
	CreatedAt  time.Time `json:"created_at"`
}

// Manager guards the single active plan under a root directory. The zero value
// is not usable; construct with NewManager.
type Manager struct {
	mu   sync.Mutex
	root string
}

// NewManager returns a Manager that persists the active plan to
// <root>/plan.json. The directory is created lazily on first Save and all disk
// access is best-effort, so an unwritable root surfaces as a Save error rather
// than a panic.
func NewManager(root string) *Manager {
	return &Manager{root: root}
}

// planPath is the on-disk location of the active plan.
func (m *Manager) planPath() string {
	return filepath.Join(m.root, "plan.json")
}

// Save establishes (or replaces) the active plan with goal + steps and writes
// plan.json. Steps with no Status are defaulted to pending; a caller may preset
// a status (e.g. to seed an already-partly-done plan). Callers do not hold m.mu.
func (m *Manager) Save(goal string, steps []Step) error {
	return m.SaveWith(goal, "", nil, steps)
}

// SaveWith is Save plus the caller's clarified intent (spec) and the plan's
// integration gate (verifyCmds), persisting both on the active plan state. Steps
// with no Status are defaulted to pending; a caller may preset a status (e.g. to
// seed an already-partly-done plan). Callers do not hold m.mu.
func (m *Manager) SaveWith(goal, spec string, verifyCmds []string, steps []Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	norm := make([]Step, len(steps))
	copy(norm, steps)
	for i := range norm {
		if norm[i].Status == "" {
			norm[i].Status = StatusPending
		}
	}
	st := State{Goal: goal, Spec: spec, VerifyCmds: verifyCmds, Steps: norm, CreatedAt: time.Now()}
	return m.write(st)
}

// Load reads the active plan from plan.json. A missing or corrupt file reports
// ok=false (never a panic), so a caller can treat "no plan" and "unreadable
// plan" alike: there is nothing to drive.
func (m *Manager) Load() (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.read()
}

// Next returns the FIRST pending step (in slice order) whose every DependsOn id
// is done, the count of pending steps remaining, and whether a runnable step was
// found. A step blocked only by unmet dependencies is skipped, not returned: if
// pending steps remain but none are runnable, ok is false while remaining>0, so
// the caller can distinguish "plan blocked" from "plan complete" (remaining 0).
func (m *Manager) Next() (Step, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.read()
	if !ok {
		return Step{}, 0, false
	}

	// Index terminal-done ids so a dependency check is O(deps) per step.
	done := make(map[string]bool, len(st.Steps))
	remaining := 0
	for _, s := range st.Steps {
		if s.Status == StatusDone {
			done[s.ID] = true
		}
		if s.Status == StatusPending {
			remaining++
		}
	}

	for _, s := range st.Steps {
		if s.Status != StatusPending {
			continue
		}
		if depsMet(s.DependsOn, done) {
			return s, remaining, true
		}
	}
	return Step{}, remaining, false
}

// depsMet reports whether every id in deps is a done step.
func depsMet(deps []string, done map[string]bool) bool {
	for _, d := range deps {
		if !done[d] {
			return false
		}
	}
	return true
}

// Record sets the step's Status (done/failed) and Note and persists the plan. A
// failed record increments Attempts (a done one does not), so the loop can see
// how many times a step was tried. An unknown id is an error and nothing is
// written. Returns the updated step.
func (m *Manager) Record(id, status, note string) (Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.read()
	if !ok {
		return Step{}, fmt.Errorf("plan: no active plan")
	}
	for i := range st.Steps {
		if st.Steps[i].ID != id {
			continue
		}
		st.Steps[i].Status = status
		st.Steps[i].Note = note
		if status == StatusFailed {
			st.Steps[i].Attempts++
		}
		if err := m.write(st); err != nil {
			return Step{}, err
		}
		return st.Steps[i], nil
	}
	return Step{}, fmt.Errorf("plan: unknown step id %q", id)
}

// Revise adaptively re-plans the REMAINING plan: it appends add (each defaulted
// to pending), applies the set fields of each update to the step sharing its id,
// and removes every id in removeIDs, then persists. update is deliberately
// field-merging, not whole-step replacing, so a caller can nudge one attribute
// without clobbering execution state: a step's Status is only changed when the
// update explicitly sets a non-empty Status (so a done step is not silently
// reset), and empty update fields leave the existing values intact.
func (m *Manager) Revise(add []Step, update []Step, removeIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.read()
	if !ok {
		return fmt.Errorf("plan: no active plan")
	}

	// Apply updates by id (field-merge, preserving execution state).
	for _, u := range update {
		for i := range st.Steps {
			if st.Steps[i].ID == u.ID {
				mergeStep(&st.Steps[i], u)
				break
			}
		}
	}

	// Remove requested ids.
	if len(removeIDs) > 0 {
		rm := make(map[string]bool, len(removeIDs))
		for _, id := range removeIDs {
			rm[id] = true
		}
		kept := st.Steps[:0]
		for _, s := range st.Steps {
			if !rm[s.ID] {
				kept = append(kept, s)
			}
		}
		st.Steps = kept
	}

	// Append new steps (defaulted to pending).
	for _, a := range add {
		if a.Status == "" {
			a.Status = StatusPending
		}
		st.Steps = append(st.Steps, a)
	}

	return m.write(st)
}

// mergeStep applies the non-empty fields of src onto dst, preserving dst's
// values (including execution state) wherever src leaves a field empty. Status
// is merged like any other field: only a non-empty src.Status changes it, so a
// caller must be explicit to reset a done/failed step.
func mergeStep(dst *Step, src Step) {
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Prompt != "" {
		dst.Prompt = src.Prompt
	}
	if src.Difficulty != "" {
		dst.Difficulty = src.Difficulty
	}
	if src.SuggestedModel != "" {
		dst.SuggestedModel = src.SuggestedModel
	}
	if src.SuggestedVerify != nil {
		dst.SuggestedVerify = src.SuggestedVerify
	}
	if src.DependsOn != nil {
		dst.DependsOn = src.DependsOn
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Note != "" {
		dst.Note = src.Note
	}
}

// write serializes st to plan.json. It creates the root lazily and returns any
// disk/marshal error so the caller can surface a persist failure. Callers hold
// m.mu.
func (m *Manager) write(st State) error {
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file, flush to stable storage, then rename onto plan.json
	// so a crash mid-write can never leave a truncated file that read() would
	// treat as "no active plan" (silently losing the step loop's durable state).
	// Mirrors memory.save's temp+Sync+Rename discipline.
	tmp, err := os.CreateTemp(m.root, "plan-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, m.planPath()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// read loads plan.json. A missing or corrupt file reports ok=false. Callers
// hold m.mu.
func (m *Manager) read() (State, bool) {
	buf, err := os.ReadFile(m.planPath())
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(buf, &st); err != nil {
		return State{}, false
	}
	return st, true
}
