package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"jindo/internal/agent"
	"jindo/internal/memory"
	"jindo/internal/policy"
	"jindo/internal/routing"
	"jindo/internal/tmux"
)

// fakeTmux records every tmux invocation, so tests can assert that Dispatch —
// which in the headless model is NOT the execution path — drives no tmux at all.
type fakeTmux struct {
	calls          [][]string
	hasSessionCode int
}

func (f *fakeTmux) seam(args ...string) (string, int, error) {
	f.calls = append(f.calls, args)
	if len(args) > 0 && args[0] == "has-session" {
		return "", f.hasSessionCode, nil
	}
	return "", 0, nil
}

// newFakeManager builds a TmuxManager wired to a fakeTmux seam that recognizes
// all three real agents.
func newFakeManager(hasSessionCode int) (*tmux.TmuxManager, *fakeTmux) {
	f := &fakeTmux{hasSessionCode: hasSessionCode}
	m := tmux.New("jindo", []string{"claude", "codex", "agy"})
	m.Tmux = f.seam
	return m, f
}

// spyMem wraps a real SharedMemory to (a) satisfy the lean dispatchMem surface
// and (b) flag any Read/All content pull. It exposes Read/All so the leanness
// invariant is observable: if Dispatch ever called them, readCalls would be
// non-zero. Dispatch is wired to spyMem via the unexported o.mem field.
type spyMem struct {
	inner        *memory.SharedMemory
	readCalls    int
	compactCalls int
}

func (s *spyMem) Root() string { return s.inner.Root() }
func (s *spyMem) AllocKey(agent string) (string, error) {
	return s.inner.AllocKey(agent)
}
func (s *spyMem) AppendNote(author, text string) error { return s.inner.AppendNote(author, text) }
func (s *spyMem) Upsert(key string, value any, author string) error {
	return s.inner.Upsert(key, value, author)
}

// MaybeCompact delegates to the real store AND records that Dispatch invoked it,
// so a test can assert automatic per-dispatch compaction happened as a side
// effect (spyMem satisfies the widened dispatchMem via this method).
func (s *spyMem) MaybeCompact(opts memory.CompactOptions) (bool, error) {
	s.compactCalls++
	return s.inner.MaybeCompact(opts)
}

// Stats delegates to the real store. It is a read-only count/digest probe (no
// entry content pulled), so unlike Read/All it does NOT bump readCalls — the
// leanness invariant permits it (spyMem satisfies the widened dispatchMem via
// this method).
func (s *spyMem) Stats() (int, bool, error) { return s.inner.Stats() }

// RetrieveInsights/AddInsight delegate to the real store. They form the curated
// insight channel (the documented exception to the no-content invariant), so
// like Stats they do NOT bump readCalls: reading a bounded top-K of the
// purpose-built learning tier is exactly what this surface is for, distinct from
// a Read/All content pull. (spyMem satisfies the widened dispatchMem via these.)
func (s *spyMem) RetrieveInsights(task string, k int) ([]memory.Insight, error) {
	return s.inner.RetrieveInsights(task, k)
}
func (s *spyMem) AddInsight(text, agent, model string, tags []string) (bool, error) {
	return s.inner.AddInsight(text, agent, model, tags)
}

// Read/All are present only so a bug that pulled content would register. They
// are never expected to be called during Dispatch.
func (s *spyMem) Read(key string) (any, bool)  { s.readCalls++; return s.inner.Read(key) }
func (s *spyMem) All() (map[string]any, error) { s.readCalls++; return s.inner.All() }

// cannedJSON is a realistic agent stdout: prose followed by a final JSON block
// carrying the response contract plus two memory updates (a note and a keyed
// value).
const cannedJSON = `I read the shared memory and did the work.

{"status":"ok","result":"did X","summary":"refactored the scheduler","memory_updates":[{"note":"decided Y"},{"key":"decision","value":"Z"}]}`

// newRecordingAdapter injects cannedJSON and records the argv RunWith was asked
// to run, using a real cliAdapter with its Exec seam overridden.
func newRecordingAdapter(t *testing.T, name, canned string, argvOut *[]string) agent.Adapter {
	t.Helper()
	ad, err := agent.GetAdapter(name)
	if err != nil {
		t.Fatalf("GetAdapter(%q) error: %v", name, err)
	}
	ad.Exec = func(argv []string) (string, error) {
		*argvOut = argv
		return canned, nil
	}
	return ad
}

func TestDispatch(t *testing.T) {
	// Route explicitly to the claude CLI family so the memory-extras assertions
	// (--append-system-prompt / --add-dir) are deterministic regardless of which
	// tier the task scores; codex would (correctly) receive no extras.
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"
	const explicitAgent = "claude"

	want, err := routing.Select(task, explicitAgent, "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}
	if want.Agent != "claude" {
		t.Fatalf("test precondition: routed agent %q is not claude; extras assertions assume the claude CLI", want.Agent)
	}

	mem := memory.New(t.TempDir())
	spy := &spyMem{inner: mem}
	mgr, fake := newFakeManager(1)

	var argv []string
	o := New(mem, mgr)
	o.mem = spy // route Dispatch's memory calls through the leanness spy
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		if name != want.Agent {
			t.Fatalf("GetAdapter got %q, want routed agent %q", name, want.Agent)
		}
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	got, err := o.Dispatch(task, explicitAgent, "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// Returned routing matches routing.Select; result/status/summary parsed.
	if got.Agent != want.Agent || got.Model != want.Model || got.Difficulty != want.Difficulty {
		t.Fatalf("route mismatch: got %+v, want agent=%s model=%s difficulty=%s",
			got, want.Agent, want.Model, want.Difficulty)
	}
	if got.Result != "did X" {
		t.Fatalf("result = %q, want %q (parsed from JSON)", got.Result, "did X")
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}

	// Key is agent-partitioned, first allocation.
	wantKey := "task:" + want.Agent + ":1"
	if got.Key != wantKey {
		t.Fatalf("key = %q, want %q", got.Key, wantKey)
	}

	// The adapter ran the task with the routed model AND received the memory
	// extras: --append-system-prompt <prompt> and --add-dir <memDir>.
	if len(argv) == 0 || argv[len(argv)-1] != task {
		t.Fatalf("adapter argv did not end with task: %v", argv)
	}
	if !containsPair(argv, "--model", want.Model) {
		t.Fatalf("adapter argv missing model %q: %v", want.Model, argv)
	}
	if !hasFlag(argv, "--append-system-prompt") {
		t.Fatalf("adapter argv missing --append-system-prompt: %v", argv)
	}
	if !containsPair(argv, "--add-dir", mem.Root()) {
		t.Fatalf("adapter argv missing --add-dir %q: %v", mem.Root(), argv)
	}

	// Tmux is NOT the execution path: Dispatch drove no tmux at all.
	if len(fake.calls) != 0 {
		t.Fatalf("expected zero tmux calls (tmux is not the execution path), got %v", fake.calls)
	}

	// Leanness: Dispatch never pulled memory content (Read/All) into itself.
	if spy.readCalls != 0 {
		t.Fatalf("leanness violated: Dispatch made %d Read/All content pulls", spy.readCalls)
	}

	// The dispatch record is readable and holds the populated result under the
	// owned key, authored by the executing agent.
	stored, ok := mem.Read(wantKey)
	if !ok {
		t.Fatalf("Read(%s) not found", wantKey)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored %s is %T, want map", wantKey, stored)
	}
	if entry["result"] != "did X" {
		t.Fatalf("stored result = %v, want %q", entry["result"], "did X")
	}

	// The note "decided Y" was appended.
	notes, err := mem.Notes()
	if err != nil {
		t.Fatalf("Notes error: %v", err)
	}
	if !notesContain(notes, "decided Y") {
		t.Fatalf("notes missing %q: %v", "decided Y", notes)
	}

	// The keyed update {key:"decision", value:"Z"} was persisted under an
	// AGENT-OWNED key (its named key "decision" is not agent-scoped, so Dispatch
	// relabels it to a fresh task:<agent>:<n>). Find the "Z" value and assert
	// its owner is the routed agent — and that it is NOT under the literal
	// "decision" key.
	if _, ok := mem.Read("decision"); ok {
		t.Fatalf("keyed update leaked under unscoped key \"decision\"")
	}
	all, err := mem.All()
	if err != nil {
		t.Fatalf("All error: %v", err)
	}
	foundZ := false
	for k, v := range all {
		if v == "Z" {
			foundZ = true
			if memory.OwnerOf(k) != want.Agent {
				t.Fatalf("keyed update under %q owned by %q, want %q", k, memory.OwnerOf(k), want.Agent)
			}
		}
	}
	if !foundZ {
		t.Fatalf("keyed update value Z not persisted: %v", all)
	}
}

// TestDispatchIdempotentResultUpsert verifies the intent+result live under the
// SAME dispatch key (no duplicate) — the result Upsert overwrites the intent in
// place rather than creating a second entry.
func TestDispatchIdempotentResultUpsert(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	var argv []string
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	got, err := o.Dispatch(task, "", "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// Exactly one dispatch record under the owned key, carrying the result.
	stored, ok := mem.Read(got.Key)
	if !ok {
		t.Fatalf("Read(%s) not found", got.Key)
	}
	entry := stored.(map[string]any)
	if entry["result"] != "did X" {
		t.Fatalf("stored result = %v, want %q (result overwrote intent in place)", entry["result"], "did X")
	}
	if entry["result"] == nil {
		t.Fatalf("dispatch key still holds nil intent result — result Upsert did not overwrite")
	}
}

// TestDispatchRationalePersistsInMemory verifies the routing rationale — the
// "why" behind an agent choice — survives the full round trip through the
// shared-memory store: it is present in the initial intent record (written
// before the agent runs), still present in the final authoritative record
// (written after the agent runs), and summarized in the dispatch note. Both
// stored records are read back through mem.Read, so — unlike the in-process
// route.Rationale value — they come back JSON round-tripped as
// map[string]any; the comparison below normalizes the expected Rationale the
// same way before comparing.
func TestDispatchRationalePersistsInMemory(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	want, err := routing.Select(task, "", "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}

	// Normalize the expected rationale through the same JSON round trip the
	// store applies, so it compares equal to what mem.Read returns.
	rawWant, err := json.Marshal(want.Rationale)
	if err != nil {
		t.Fatalf("json.Marshal(want.Rationale) error: %v", err)
	}
	var wantRationale map[string]any
	if err := json.Unmarshal(rawWant, &wantRationale); err != nil {
		t.Fatalf("json.Unmarshal(want.Rationale) error: %v", err)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	var argv []string
	var intentEntry map[string]any
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		// The intent Upsert (below) runs before Dispatch calls GetAdapter, so
		// the dispatch key already holds the intent record at this point —
		// snapshot it here before the (also key-owning) result Upsert
		// overwrites it.
		wantKey := "task:" + name + ":1"
		if stored, ok := mem.Read(wantKey); ok {
			intentEntry, _ = stored.(map[string]any)
		}
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	got, err := o.Dispatch(task, "", "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	if intentEntry == nil {
		t.Fatalf("intent record for %s not found before agent run", got.Key)
	}
	if !reflect.DeepEqual(intentEntry["rationale"], map[string]any(wantRationale)) {
		t.Fatalf("intent rationale = %#v, want %#v", intentEntry["rationale"], wantRationale)
	}

	stored, ok := mem.Read(got.Key)
	if !ok {
		t.Fatalf("Read(%s) not found", got.Key)
	}
	finalEntry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored %s is %T, want map", got.Key, stored)
	}
	if !reflect.DeepEqual(finalEntry["rationale"], map[string]any(wantRationale)) {
		t.Fatalf("final rationale = %#v, want %#v", finalEntry["rationale"], wantRationale)
	}

	notes, err := mem.Notes()
	if err != nil {
		t.Fatalf("Notes error: %v", err)
	}
	foundSummary := false
	for _, n := range notes {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		text, ok := m["text"].(string)
		if !ok {
			continue
		}
		if strings.Contains(text, "total=") && strings.Contains(text, "matched=") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Fatalf("no dispatch note contains the rationale summary (total=/matched=): %v", notes)
	}
}

func TestDispatchAllocatesFreshKeyAcrossCalls(t *testing.T) {
	const task = "add a helper"

	want, err := routing.Select(task, "", "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	var argv []string
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	first, err := o.Dispatch(task, "", "")
	if err != nil {
		t.Fatalf("first Dispatch error: %v", err)
	}
	second, err := o.Dispatch(task, "", "")
	if err != nil {
		t.Fatalf("second Dispatch error: %v", err)
	}

	if first.Key == second.Key {
		t.Fatalf("keys collided: both %q", first.Key)
	}
	if !strings.HasPrefix(first.Key, "task:"+want.Agent+":") {
		t.Fatalf("first key %q not agent-partitioned for %q", first.Key, want.Agent)
	}

	for _, k := range []string{first.Key, second.Key} {
		stored, ok := mem.Read(k)
		if !ok {
			t.Fatalf("Read(%s) not found", k)
		}
		entry := stored.(map[string]any)
		if entry["result"] != "did X" {
			t.Fatalf("stored %s result = %v, want %q", k, entry["result"], "did X")
		}
	}
}

// captureAdapter is a mocked Adapter that records exactly the (task, model,
// extra) triple handed to RunWith, so a test can assert the per-agent dispatch
// contract (which extras are passed and whether the instruction was prefixed
// into the task) at the RunWith boundary, independent of argv assembly.
type captureAdapter struct {
	name     string
	canned   string
	gotTask  string
	gotModel string
	gotExtra []string
}

func (c *captureAdapter) Name() string                             { return c.name }
func (c *captureAdapter) BuildCommand(task, model string) []string { return nil }
func (c *captureAdapter) BuildCommandWith(task, model string, extra []string) []string {
	return nil
}
func (c *captureAdapter) Run(task, model string) (string, error) { return c.canned, nil }
func (c *captureAdapter) RunWith(task, model string, extra []string) (string, error) {
	c.gotTask, c.gotModel, c.gotExtra = task, model, extra
	return c.canned, nil
}

// sysPromptMarker is a stable substring of agentproto.BuildSystemPrompt's output
// ("STEP 1 - READ SHARED MEMORY FIRST."), used to prove whether the instruction
// was prefixed into the dispatched task string.
const sysPromptMarker = "READ SHARED MEMORY"

// TestDispatchAgyPrefixesInsteadOfSystemPromptFlag is the orchestrator-level
// regression for the live bug. For an agy-routed dispatch: the extras must carry
// --add-dir but NEVER --append-system-prompt, and the task handed to RunWith
// must be PREFIXED with the memory instruction (agy has no system-prompt flag).
// The persisted dispatch record must still hold the ORIGINAL (unprefixed) task.
func TestDispatchAgyPrefixesInsteadOfSystemPromptFlag(t *testing.T) {
	const task = "add a helper" // scores trivial -> agy is a valid explicit route

	want, err := routing.Select(task, "agy", "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}
	if want.Agent != "agy" {
		t.Fatalf("test precondition: forced route did not yield agy, got %q", want.Agent)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "agy", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	got, err := o.Dispatch(task, "agy", "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// (a) extras: --add-dir present, --append-system-prompt absent.
	if !containsPair(cap.gotExtra, "--add-dir", mem.Root()) {
		t.Fatalf("agy extras missing --add-dir %q: %v", mem.Root(), cap.gotExtra)
	}
	if hasFlag(cap.gotExtra, "--append-system-prompt") {
		t.Fatalf("agy extras must NEVER contain --append-system-prompt: %v", cap.gotExtra)
	}
	// LIVE-CONFIRMED regression: agy does NOT operate on the real process cwd
	// by default (or when only the nested memory subdirectory is granted) — it
	// silently redirects all work into its own default scratch directory and
	// still reports success there. Granting --add-dir for the actual cwd fixes
	// this; --dangerously-skip-permissions is agy's only available bypass for
	// the same unanswerable-approval-prompt problem as claude/codex.
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if !containsPair(cap.gotExtra, "--add-dir", wantCwd) {
		t.Fatalf("agy extras missing --add-dir for the real cwd %q: %v", wantCwd, cap.gotExtra)
	}
	if !hasFlag(cap.gotExtra, "--dangerously-skip-permissions") {
		t.Fatalf("agy extras missing --dangerously-skip-permissions: %v", cap.gotExtra)
	}

	// (b) the dispatched task was prefixed with the memory instruction and still
	// ends with the original task text.
	if !strings.Contains(cap.gotTask, sysPromptMarker) {
		t.Fatalf("agy dispatched task not prefixed with instruction (marker %q): %q", sysPromptMarker, cap.gotTask)
	}
	if !strings.HasSuffix(cap.gotTask, task) {
		t.Fatalf("agy dispatched task does not end with original task: %q", cap.gotTask)
	}

	// (c) persisted record holds the ORIGINAL (unprefixed) task.
	stored, ok := mem.Read(got.Key)
	if !ok {
		t.Fatalf("Read(%s) not found", got.Key)
	}
	entry := stored.(map[string]any)
	if entry["task"] != task {
		t.Fatalf("persisted task = %v, want original %q (not the prefixed blob)", entry["task"], task)
	}
}

// TestDispatchClaudeUsesFlagNotPrefix is the parallel claude contract: the
// instruction rides --append-system-prompt (extras carry it) and the task handed
// to RunWith is NOT prefixed with the instruction marker.
func TestDispatchClaudeUsesFlagNotPrefix(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	want, err := routing.Select(task, "claude", "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}
	if want.Agent != "claude" {
		t.Fatalf("test precondition: forced route did not yield claude, got %q", want.Agent)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "claude", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	if _, err := o.Dispatch(task, "claude", ""); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	if !hasFlag(cap.gotExtra, "--append-system-prompt") {
		t.Fatalf("claude extras must contain --append-system-prompt: %v", cap.gotExtra)
	}
	if !containsPair(cap.gotExtra, "--add-dir", mem.Root()) {
		t.Fatalf("claude extras missing --add-dir %q: %v", mem.Root(), cap.gotExtra)
	}
	// LIVE-CONFIRMED regression: without --permission-mode acceptEdits, claude's
	// headless file writes silently stall on an unanswerable approval prompt
	// ("It looks like you haven't granted permission...") and no work is done.
	if !containsPair(cap.gotExtra, "--permission-mode", "acceptEdits") {
		t.Fatalf("claude extras missing --permission-mode acceptEdits: %v", cap.gotExtra)
	}
	// claude's task is NOT prefixed: the instruction goes via the flag, so the
	// dispatched task equals the original and carries no marker.
	if cap.gotTask != task {
		t.Fatalf("claude dispatched task = %q, want unchanged original %q", cap.gotTask, task)
	}
	if strings.Contains(cap.gotTask, sysPromptMarker) {
		t.Fatalf("claude dispatched task must NOT be prefixed with instruction: %q", cap.gotTask)
	}
	// Defense-in-depth: claude also gets --disallowedTools for every sensitive
	// pattern, so even a task that says nothing about a sensitive path cannot
	// have claude write/edit one mid-task.
	if !containsPair(cap.gotExtra, "--disallowedTools", "Write(.env)") {
		t.Fatalf("claude extras missing --disallowedTools Write(.env): %v", cap.gotExtra)
	}
	if !hasFlag(cap.gotExtra, "Edit(.env)") {
		t.Fatalf("claude extras missing Edit(.env): %v", cap.gotExtra)
	}
	// Isolation: claude must never reload the host's project .mcp.json (which
	// registers jindo itself) — --strict-mcp-config with no --mcp-config yields
	// zero MCP servers for the sub-agent, preventing recursion.
	if !hasFlag(cap.gotExtra, "--strict-mcp-config") {
		t.Fatalf("claude extras missing --strict-mcp-config: %v", cap.gotExtra)
	}
}

// TestDispatchBlocksSensitivePathBeforeAnyAdapterCall is the regression guard
// for the sensitive-file dispatch policy: a task referencing a sensitive path
// must be refused by Dispatch itself, for ANY agent, without ever invoking
// GetAdapter/RunWith. This is the CLI-agnostic gate — codex and agy have no
// per-file deny flag of their own, so this check is the only thing that
// protects them.
func TestDispatchBlocksSensitivePathBeforeAnyAdapterCall(t *testing.T) {
	const task = "write SECRET=1 into .env so the server can read it"

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	adapterCalled := false
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		adapterCalled = true
		return &captureAdapter{name: name, canned: cannedJSON}, nil
	}

	for _, agentName := range []string{"claude", "codex", "agy", ""} {
		adapterCalled = false
		_, err := o.Dispatch(task, agentName, "")
		if err == nil {
			t.Fatalf("Dispatch(%q, %q): expected error, got nil", task, agentName)
		}
		var blocked *policy.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("Dispatch(%q, %q): error = %v, want *policy.BlockedError", task, agentName, err)
		}
		if adapterCalled {
			t.Fatalf("Dispatch(%q, %q): adapter was called, want no agent invocation on a blocked task", task, agentName)
		}
	}

	// Memory must carry no trace of the blocked attempt (no key allocated).
	all, err := mem.All()
	if err != nil {
		t.Fatalf("All error: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("blocked dispatch left memory entries: %v", all)
	}
}

// TestDispatchAllowsOrdinaryTaskThroughPolicyGate confirms the gate is
// specific: an ordinary task is unaffected and still reaches the adapter.
func TestDispatchAllowsOrdinaryTaskThroughPolicyGate(t *testing.T) {
	const task = "add a health check endpoint to the server"

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	var argv []string
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	if _, err := o.Dispatch(task, "", ""); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
}

// TestDispatchCodexPrefixesWithSandboxFlag is the codex contract regression for
// two live bugs: (1) codex received NO instruction (its CLI has no
// system-prompt/--add-dir flag; its only instruction channel is the [PROMPT]
// positional argument, so the memory instruction is PREFIXED into the
// dispatched task, like agy); and (2) codex's default sandbox is
// directory-trust-dependent and silently refuses file writes ("read-only
// sandbox and approvals are disabled") outside a trusted directory, so it must
// be elevated via `-s workspace-write` (scoped to the working directory, NOT
// the "danger-full-access" mode). The persisted dispatch record must still
// hold the ORIGINAL (unprefixed) task.
func TestDispatchCodexPrefixesWithSandboxFlag(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	want, err := routing.Select(task, "codex", "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}
	if want.Agent != "codex" {
		t.Fatalf("test precondition: forced route did not yield codex, got %q", want.Agent)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "codex", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	got, err := o.Dispatch(task, "codex", "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// (a) codex gets the sandbox-elevation flag but NEVER --add-dir or
	// --append-system-prompt (codex has no such flags).
	if !containsPair(cap.gotExtra, "-s", "workspace-write") {
		t.Fatalf("codex extras missing -s workspace-write: %v", cap.gotExtra)
	}
	if hasFlag(cap.gotExtra, "--add-dir") || hasFlag(cap.gotExtra, "--append-system-prompt") {
		t.Fatalf("codex extras must never contain --add-dir/--append-system-prompt: %v", cap.gotExtra)
	}
	// Isolation: codex must skip ~/.codex/config.toml (registers jindo+serena)
	// so the sub-agent doesn't recursively reload the host's MCP servers.
	if !hasFlag(cap.gotExtra, "--ignore-user-config") {
		t.Fatalf("codex extras missing --ignore-user-config: %v", cap.gotExtra)
	}

	// (b) the dispatched task was prefixed with the memory instruction and still
	// ends with the original task text.
	if !strings.Contains(cap.gotTask, sysPromptMarker) {
		t.Fatalf("codex dispatched task not prefixed with instruction (marker %q): %q", sysPromptMarker, cap.gotTask)
	}
	if !strings.HasSuffix(cap.gotTask, task) {
		t.Fatalf("codex dispatched task does not end with original task: %q", cap.gotTask)
	}

	// (c) persisted record holds the ORIGINAL (unprefixed) task.
	stored, ok := mem.Read(got.Key)
	if !ok {
		t.Fatalf("Read(%s) not found", got.Key)
	}
	entry := stored.(map[string]any)
	if entry["task"] != task {
		t.Fatalf("persisted task = %v, want original %q (not the prefixed blob)", entry["task"], task)
	}
}

// TestDispatchAuthoritativeRecordWinsOverOwnKeyUpdate is the regression guard for
// the memory-clobber-ordering bug: an agent's memory_updates that names the
// dispatch's OWN key must NOT overwrite the authoritative structured record.
//
// The fan-out relabel guard deliberately does NOT relabel a key already owned by
// the executing agent (OwnerOf == route.Agent), and the dispatch's own key is
// owned by that same agent — so a stray scalar update naming it would be written
// under that key. The fix orders the authoritative full-result Upsert AFTER the
// fan-out loop, so the structured record is always the final state of the key.
//
// The mocked adapter must target the dispatch key from the start, so we predict
// it: over a FRESH memory root with no prior entries, the first dispatch to any
// agent allocates task:<agent>:1 (AllocKey computes one past the max index across
// all keys). We craft the canned JSON with that predicted key, run Dispatch, then
// assert (a) the returned Key matches the prediction (self-checking) and (b) the
// stored value is the full record, not the stray scalar.
func TestDispatchAuthoritativeRecordWinsOverOwnKeyUpdate(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"
	const explicitAgent = "claude"

	want, err := routing.Select(task, explicitAgent, "")
	if err != nil {
		t.Fatalf("routing.Select error: %v", err)
	}
	if want.Agent != "claude" {
		t.Fatalf("test precondition: routed agent %q is not claude", want.Agent)
	}

	// Predicted first-allocation key over a fresh memory root.
	predictedKey := "task:" + want.Agent + ":1"

	// Canned response whose memory_updates names the dispatch's OWN key with a
	// stray scalar — exactly the live-reproduced clobber shape.
	canned := `Done.

{"status":"ok","result":"parsed ISO date via helper","summary":"added date helper","memory_updates":[{"key":"` + predictedKey + `","value":"stray-clobber-value"}]}`

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	var argv []string
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, canned, &argv), nil
	}

	got, err := o.Dispatch(task, explicitAgent, "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// Self-check: the key prediction that the canned payload targeted must match
	// the key Dispatch actually allocated, or the test would be vacuous.
	if got.Key != predictedKey {
		t.Fatalf("key prediction wrong: got %q, want %q — canned payload targeted the wrong key", got.Key, predictedKey)
	}

	stored, ok := mem.Read(predictedKey)
	if !ok {
		t.Fatalf("Read(%s) not found", predictedKey)
	}
	// The authoritative record must win: a full map, not the stray scalar string.
	if s, isStr := stored.(string); isStr {
		t.Fatalf("dispatch key clobbered by stray scalar %q — authoritative record did not write last", s)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored %s is %T, want map (authoritative full record)", predictedKey, stored)
	}
	if entry["result"] != "parsed ISO date via helper" {
		t.Fatalf("stored result = %v, want %q (full authoritative record)", entry["result"], "parsed ISO date via helper")
	}
	if entry["status"] != "ok" {
		t.Fatalf("stored status = %v, want ok", entry["status"])
	}
	if entry["summary"] != "added date helper" {
		t.Fatalf("stored summary = %v, want %q", entry["summary"], "added date helper")
	}
	if entry["agent"] != want.Agent || entry["model"] != want.Model || entry["difficulty"] != want.Difficulty {
		t.Fatalf("stored routing fields mismatch: got agent=%v model=%v difficulty=%v",
			entry["agent"], entry["model"], entry["difficulty"])
	}
	if entry["task"] != task {
		t.Fatalf("stored task = %v, want %q", entry["task"], task)
	}
}

// TestDispatchAutoCompactsMemoryWithoutManualCall is the regression for wiring
// MaybeCompact into Dispatch: the shared store must self-bound on a normal
// dispatch, with NO manual Compact call by the caller. It seeds MaxEntries worth
// of real entries directly (fast/deterministic — far cheaper than running 200+
// mocked dispatches), then performs a single real Dispatch. That dispatch's own
// writes (intent record + fan-out relabel of the canned "decision" update) push
// the real-entry count past the 200 cap, so MaybeCompact must fold the cold tail
// into _digest purely as a side effect of Dispatch.
func TestDispatchAutoCompactsMemoryWithoutManualCall(t *testing.T) {
	const maxEntries = 200 // must match Dispatch's hardcoded default
	// digestKey mirrors memory's unexported reserved digest key (a fixed control
	// key); the memory package does not export it, so we restate the literal here
	// rather than widen that package's surface just for this test.
	const digestKey = "_digest"

	mem := memory.New(t.TempDir())
	spy := &spyMem{inner: mem}

	// Seed exactly maxEntries real entries so a single subsequent dispatch (which
	// adds its own record plus the fan-out relabel) crosses the cap. Keys use the
	// "task:<agent>:<n>" shape so AllocKey's max-index scan advances past them and
	// the dispatch allocates a fresh, non-colliding key.
	for i := 1; i <= maxEntries; i++ {
		key := fmt.Sprintf("task:seed:%d", i)
		if err := mem.Upsert(key, map[string]any{"result": fmt.Sprintf("seeded %d", i)}, "seed"); err != nil {
			t.Fatalf("seed Upsert(%s) error: %v", key, err)
		}
	}

	mgr, _ := newFakeManager(0)
	var argv []string
	o := New(mem, mgr)
	o.mem = spy // route Dispatch's memory calls (incl. MaybeCompact) through the spy
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	// A single ordinary dispatch. The test itself NEVER calls mem.Compact.
	if _, err := o.Dispatch("add a helper", "", ""); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// (c) compaction happened purely as a Dispatch side effect: the spy saw the
	// MaybeCompact call, and the test made no manual Compact call.
	if spy.compactCalls == 0 {
		t.Fatalf("Dispatch did not invoke MaybeCompact (compactCalls=0)")
	}

	all, err := mem.All()
	if err != nil {
		t.Fatalf("All error: %v", err)
	}

	// (b) the folded cold tail lives under the reserved _digest entry. All()
	// excludes control keys, so read it explicitly.
	digest, ok := mem.Read(digestKey)
	if !ok {
		t.Fatalf("_digest entry missing after auto-compaction: %v keys present", len(all))
	}
	inner, ok := digest.(map[string]any)
	if !ok {
		t.Fatalf("_digest value is %T, want map with folded body", digest)
	}
	if c, _ := inner["count"].(float64); c <= 0 {
		t.Fatalf("_digest count = %v, want > 0 (cold tail folded)", inner["count"])
	}

	// (a) live real entry count (everything All returns except the _digest entry)
	// is capped at MaxEntries — Compact keeps exactly the newest MaxEntries.
	realCount := 0
	for k := range all {
		if k == digestKey {
			continue
		}
		realCount++
	}
	if realCount != maxEntries {
		t.Fatalf("live real entry count = %d, want %d (capped by auto-compaction)", realCount, maxEntries)
	}
}

// TestDispatchWritesStructuredDispatchLog verifies the JSONL dispatch log: after
// a single dispatch (with a STUBBED Route returning a known rationale and a
// canned agent response carrying known memory_updates + status), one JSON line
// lands in <memDir>/dispatch.log next to memory.json, and it carries the routing
// rationale, the injection context (injected_records/injected_digest captured
// pre-run via mem.Stats), the memory_updates summary, and the outcome.
func TestDispatchWritesStructuredDispatchLog(t *testing.T) {
	mem := memory.New(t.TempDir())
	spy := &spyMem{inner: mem}

	// Seed 3 real records so the pre-run injection count is predictable: these 3
	// plus the intent record Dispatch writes before RunWith => injected_records
	// must equal 4. AllocKey scans the max index across ALL keys, so with seeds at
	// index 3 the dispatch is handed task:claude:4.
	for i := 1; i <= 3; i++ {
		if err := mem.Upsert(fmt.Sprintf("task:seed:%d", i), map[string]any{"result": "x"}, "seed"); err != nil {
			t.Fatalf("seed Upsert error: %v", err)
		}
	}

	wantRationale := routing.Rationale{
		Matched:        map[string]float64{"concurrency": 2.0},
		Total:          2.0,
		Threshold:      "hard",
		ThresholdValue: 1.5,
		Tier:           "hard",
	}

	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	o.mem = spy
	o.Route = func(task, agentName, priority, model string) (routing.Selection, error) {
		return routing.Selection{
			Agent:      "claude",
			Model:      "opus",
			Difficulty: "hard",
			Rationale:  wantRationale,
		}, nil
	}
	cannedWithMemoryUsed := `I read the shared memory and did the work.

{"status":"ok","result":"did X","summary":"refactored the scheduler","memory_updates":[{"note":"decided Y"},{"key":"decision","value":"Z"}],"memory_used":["task:seed:1","_digest"]}`

	o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			t.Fatalf("GetAdapter(%q) error: %v", name, err)
		}
		// Sleep so the measured author-run latency is deterministically > 0,
		// proving DurationMs times the actual RunWith call rather than being a
		// stray zero value.
		ad.Exec = func(argv []string) (string, error) {
			time.Sleep(2 * time.Millisecond)
			return cannedWithMemoryUsed, nil
		}
		return ad, nil
	}

	if _, err := o.Dispatch("refactor the scheduler", "", ""); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	// Leanness holds: Stats is not a content pull, so Dispatch made zero Read/All.
	if spy.readCalls != 0 {
		t.Fatalf("leanness violated: Dispatch made %d Read/All content pulls", spy.readCalls)
	}

	// The log lands next to memory.json under the memory root.
	raw, err := os.ReadFile(filepath.Join(mem.Root(), "dispatch.log"))
	if err != nil {
		t.Fatalf("read dispatch.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("dispatch.log has %d lines, want 1: %q", len(lines), string(raw))
	}

	var entry dispatchLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("dispatch.log line is not valid JSON: %v (%q)", err, lines[0])
	}

	// Routing rationale round-tripped (matched signals/total/threshold/tier).
	if !reflect.DeepEqual(entry.Rationale, wantRationale) {
		t.Fatalf("logged rationale = %#v, want %#v", entry.Rationale, wantRationale)
	}
	// Injection context: 3 seeds + the intent record = 4, no digest present.
	if entry.InjectedRecords != 4 {
		t.Fatalf("injected_records = %d, want 4", entry.InjectedRecords)
	}
	if entry.InjectedDigest {
		t.Fatalf("injected_digest = true, want false (no _digest seeded)")
	}
	// memory_updates summary reflects the canned response (a note + a keyed value).
	if entry.MemoryUpdates.Count != 2 {
		t.Fatalf("memory_updates count = %d, want 2", entry.MemoryUpdates.Count)
	}
	if !containsStr(entry.MemoryUpdates.Keys, "decision") {
		t.Fatalf("memory_updates keys = %v, want to include %q", entry.MemoryUpdates.Keys, "decision")
	}
	if !containsStr(entry.MemoryUpdates.Notes, "decided Y") {
		t.Fatalf("memory_updates notes = %v, want to include %q", entry.MemoryUpdates.Notes, "decided Y")
	}
	// memory_used flows through from the agent's response into the log entry.
	if !containsStr(entry.MemoryUsed, "task:seed:1") || !containsStr(entry.MemoryUsed, "_digest") {
		t.Fatalf("memory_used = %v, want to include %q and %q", entry.MemoryUsed, "task:seed:1", "_digest")
	}
	// Outcome fields.
	if entry.Status != "ok" {
		t.Fatalf("status = %q, want ok", entry.Status)
	}
	if entry.Summary != "refactored the scheduler" {
		t.Fatalf("summary = %q, want %q", entry.Summary, "refactored the scheduler")
	}
	// Identity fields carried through.
	if entry.Key != "task:claude:4" || entry.Agent != "claude" || entry.Model != "opus" || entry.Difficulty != "hard" {
		t.Fatalf("identity fields mismatch: key=%q agent=%q model=%q difficulty=%q",
			entry.Key, entry.Agent, entry.Model, entry.Difficulty)
	}
	if entry.Timestamp == "" {
		t.Fatalf("timestamp empty, want RFC3339")
	}
	// duration_ms measures the author adapter run alone; the fake adapter sleeps
	// 2ms so a real elapsed time (not a stray zero) must be recorded.
	if entry.DurationMs < 1 {
		t.Fatalf("duration_ms = %d, want >= 1", entry.DurationMs)
	}
}

// TestDispatchWritesErrorDispatchLogOnRunWithFailure closes the T2 caveat: when
// ad.RunWith fails, Dispatch still writes one dispatch.log line — status
// "error", the RunWith failure in the summary, and the same rationale/injection
// context it would have logged on success — even though it returns early with
// an error and never reaches ParseResponse.
func TestDispatchWritesErrorDispatchLogOnRunWithFailure(t *testing.T) {
	mem := memory.New(t.TempDir())

	wantRationale := routing.Rationale{
		Matched:        map[string]float64{"concurrency": 2.0},
		Total:          2.0,
		Threshold:      "hard",
		ThresholdValue: 1.5,
		Tier:           "hard",
	}

	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	o.Route = func(task, agentName, priority, model string) (routing.Selection, error) {
		return routing.Selection{
			Agent:      "claude",
			Model:      "opus",
			Difficulty: "hard",
			Rationale:  wantRationale,
		}, nil
	}
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			t.Fatalf("GetAdapter(%q) error: %v", name, err)
		}
		ad.Exec = func(argv []string) (string, error) {
			return "", errors.New("cli exited: rate limited")
		}
		return ad, nil
	}

	if _, err := o.Dispatch("refactor the scheduler", "", ""); err == nil {
		t.Fatalf("Dispatch error = nil, want RunWith failure surfaced")
	}

	raw, err := os.ReadFile(filepath.Join(mem.Root(), "dispatch.log"))
	if err != nil {
		t.Fatalf("read dispatch.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("dispatch.log has %d lines, want 1: %q", len(lines), string(raw))
	}

	var entry dispatchLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("dispatch.log line is not valid JSON: %v (%q)", err, lines[0])
	}

	if entry.Status != "error" {
		t.Fatalf("status = %q, want error", entry.Status)
	}
	if !strings.Contains(entry.Summary, "rate limited") {
		t.Fatalf("summary = %q, want it to contain the RunWith error", entry.Summary)
	}
	if !reflect.DeepEqual(entry.Rationale, wantRationale) {
		t.Fatalf("logged rationale = %#v, want %#v", entry.Rationale, wantRationale)
	}
	if entry.Agent != "claude" || entry.Model != "opus" || entry.Difficulty != "hard" {
		t.Fatalf("identity fields mismatch: agent=%q model=%q difficulty=%q",
			entry.Agent, entry.Model, entry.Difficulty)
	}
	if entry.MemoryUpdates.Count != 0 {
		t.Fatalf("memory_updates count = %d, want 0 (RunWith failed before any response)", entry.MemoryUpdates.Count)
	}
	// duration_ms is recorded even on a RunWith failure (the run was still timed).
	if entry.DurationMs < 0 {
		t.Fatalf("duration_ms = %d, want >= 0", entry.DurationMs)
	}
}

// TestDispatchModelPinsAuthorModel proves DispatchModel threads the caller's
// model pin through the Route seam into the author run: the pinned model reaches
// Route, is echoed into the Selection, and shows up both in Result.Model and in
// the argv the adapter is invoked with (claude passes it as --model <id>).
func TestDispatchModelPinsAuthorModel(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)

	const pinned = "claude-opus-4-8"
	var gotRouteModel string
	o.Route = func(task, agentName, priority, model string) (routing.Selection, error) {
		gotRouteModel = model
		// Echo the pinned model verbatim, as routing.SelectModel does on the pin path.
		return routing.Selection{Agent: "claude", Model: model, Difficulty: "explicit"}, nil
	}

	var gotArgv []string
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			t.Fatalf("GetAdapter(%q) error: %v", name, err)
		}
		ad.Exec = func(argv []string) (string, error) {
			gotArgv = argv
			return `{"status":"ok","result":"did X","summary":"s"}`, nil
		}
		return ad, nil
	}

	res, err := o.DispatchModel("refactor the scheduler", "claude", "", pinned, "", "", false, nil, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	if gotRouteModel != pinned {
		t.Fatalf("Route received model %q, want %q", gotRouteModel, pinned)
	}
	if res.Model != pinned {
		t.Fatalf("Result.Model = %q, want %q", res.Model, pinned)
	}
	found := false
	for _, a := range gotArgv {
		if a == pinned {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("adapter argv %v does not contain pinned model %q", gotArgv, pinned)
	}
}

// TestDispatchModelGuidanceAppendsToSystemPrompt proves DispatchModel's
// guidance param is appended, clearly delimited, onto the base
// agentproto.BuildSystemPrompt system prompt the author actually receives (via
// claude's --append-system-prompt flag) — and that guidance == "" leaves the
// system prompt byte-identical to the pre-guidance behavior (no section added).
func TestDispatchModelGuidanceAppendsToSystemPrompt(t *testing.T) {
	const task = "write a function"
	const guidance = "Follow idiomatic Python: type hints, docstrings, snake_case."

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "claude", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	if _, err := o.DispatchModel(task, "claude", "", "", guidance, "", false, nil, ""); err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}

	idx := -1
	for i, a := range cap.gotExtra {
		if a == "--append-system-prompt" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(cap.gotExtra) {
		t.Fatalf("claude extras missing --append-system-prompt value: %v", cap.gotExtra)
	}
	sysPrompt := cap.gotExtra[idx+1]
	if !strings.Contains(sysPrompt, "TASK-SPECIFIC GUIDANCE") {
		t.Fatalf("system prompt missing TASK-SPECIFIC GUIDANCE section: %q", sysPrompt)
	}
	if !strings.Contains(sysPrompt, guidance) {
		t.Fatalf("system prompt missing guidance text %q: %q", guidance, sysPrompt)
	}

	// A second dispatch with guidance == "" must produce NO guidance section.
	mem2 := memory.New(t.TempDir())
	cap2 := &captureAdapter{name: "claude", canned: cannedJSON}
	o2 := New(mem2, mgr)
	o2.GetAdapter = func(name string) (agent.Adapter, error) { return cap2, nil }
	if _, err := o2.DispatchModel(task, "claude", "", "", "", "", false, nil, ""); err != nil {
		t.Fatalf("DispatchModel (no guidance) error: %v", err)
	}
	idx2 := -1
	for i, a := range cap2.gotExtra {
		if a == "--append-system-prompt" {
			idx2 = i
			break
		}
	}
	if idx2 == -1 || idx2+1 >= len(cap2.gotExtra) {
		t.Fatalf("claude extras missing --append-system-prompt value: %v", cap2.gotExtra)
	}
	if strings.Contains(cap2.gotExtra[idx2+1], "TASK-SPECIFIC GUIDANCE") {
		t.Fatalf("system prompt must NOT contain a guidance section when guidance is empty: %q", cap2.gotExtra[idx2+1])
	}
}

// TestAppendDispatchLogRotatesPastMaxSize shrinks maxDispatchLogSize (via the
// package var, restored after the test) so a handful of small entries can
// exercise rotation without writing 5MB of fixture data. Once the log crosses
// the cap, the next append must rotate the old content to dispatch.log.1
// (overwriting any prior .1) before writing, keeping dispatch.log itself
// bounded to what was written since the last rotation.
func TestAppendDispatchLogRotatesPastMaxSize(t *testing.T) {
	dir := t.TempDir()

	oldMax := maxDispatchLogSize
	maxDispatchLogSize = 1 // smaller than any single marshaled entry: every append after the first rotates
	defer func() { maxDispatchLogSize = oldMax }()

	entry := dispatchLogEntry{Timestamp: "t", Key: "k", Task: "task", Agent: "a", Model: "m", Status: "ok"}

	const n = 5
	for i := 0; i < n; i++ {
		if err := appendDispatchLog(dir, entry); err != nil {
			t.Fatalf("appendDispatchLog[%d]: %v", i, err)
		}
	}

	// With a 1-byte cap, every append past the first finds the log already
	// over cap and rotates it to dispatch.log.1 first, so dispatch.log always
	// holds exactly the single most recent entry, never the full history.
	raw, err := os.ReadFile(filepath.Join(dir, "dispatch.log"))
	if err != nil {
		t.Fatalf("read dispatch.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("dispatch.log has %d lines after %d appends, want 1 (rotation should keep it bounded): %q", len(lines), n, string(raw))
	}

	rotated, err := os.ReadFile(filepath.Join(dir, "dispatch.log.1"))
	if err != nil {
		t.Fatalf("read dispatch.log.1: %v", err)
	}
	rotatedLines := strings.Split(strings.TrimSpace(string(rotated)), "\n")
	if len(rotatedLines) != 1 {
		t.Fatalf("dispatch.log.1 has %d lines, want 1 (one generation, overwritten each rotation): %q", len(rotatedLines), string(rotated))
	}
}

// TestRotateDispatchLogIfNeededMissingFileIsNoop covers the best-effort
// contract: rotating a dispatch.log that does not exist yet (first-ever
// dispatch in a fresh memory dir) must be silently skipped, not error or
// panic, since appendDispatchLog calls it unconditionally before every write.
func TestRotateDispatchLogIfNeededMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	rotateDispatchLogIfNeeded(dir) // must not panic or create anything
	if _, err := os.Stat(filepath.Join(dir, "dispatch.log")); err == nil {
		t.Fatalf("rotateDispatchLogIfNeeded unexpectedly created dispatch.log")
	}
	if _, err := os.Stat(filepath.Join(dir, "dispatch.log.1")); err == nil {
		t.Fatalf("rotateDispatchLogIfNeeded unexpectedly created dispatch.log.1")
	}
}

// containsStr reports whether ss contains s.
func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// notesContain reports whether any note's text equals text.
func notesContain(notes []any, text string) bool {
	for _, n := range notes {
		if m, ok := n.(map[string]any); ok {
			if s, ok := m["text"].(string); ok && s == text {
				return true
			}
		}
	}
	return false
}

// containsPair reports whether argv contains flag immediately followed by value.
func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// hasFlag reports whether argv contains flag.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// scriptedAdapter is a test Adapter whose RunWith pops the next canned output
// from a shared per-agent queue and bumps a shared invocation counter, so a
// review-pipeline test can (a) hand the author and the cross-model reviewers
// DIFFERENT scripted outputs by agent name, (b) vary a reviewer's output between
// the first review and the re-review, and (c) assert the EXACT number of adapter
// invocations (proving no extra revision round happened).
//
// Since review:true now fans out to MULTIPLE reviewers concurrently, RunWith is
// called from several goroutines at once; mu (shared across all adapters serving
// one scriptedGetAdapter) guards both the shared counter increment and the
// per-agent slice pop so `go test -race` stays clean.
type scriptedAdapter struct {
	name  string
	outs  *[]string
	calls *int
	mu    *sync.Mutex
}

func (a *scriptedAdapter) Name() string                             { return a.name }
func (a *scriptedAdapter) BuildCommand(task, model string) []string { return nil }
func (a *scriptedAdapter) BuildCommandWith(task, model string, extra []string) []string {
	return nil
}
func (a *scriptedAdapter) Run(task, model string) (string, error) { return "", nil }
func (a *scriptedAdapter) RunWith(task, model string, extra []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*a.calls++
	if len(*a.outs) == 0 {
		return "", fmt.Errorf("scriptedAdapter %q: no more scripted outputs", a.name)
	}
	out := (*a.outs)[0]
	*a.outs = (*a.outs)[1:]
	return out, nil
}

// scriptedGetAdapter builds an o.GetAdapter that serves scriptedAdapters backed
// by per-agent output queues, sharing a single call counter AND a single mutex
// across all agents (so concurrent reviewer goroutines are race-clean).
func scriptedGetAdapter(queues map[string][]string, calls *int) func(string) (agent.Adapter, error) {
	q := make(map[string]*[]string, len(queues))
	for name, outs := range queues {
		cp := append([]string(nil), outs...)
		q[name] = &cp
	}
	mu := &sync.Mutex{}
	return func(name string) (agent.Adapter, error) {
		ptr, ok := q[name]
		if !ok {
			return nil, fmt.Errorf("no scripted adapter for %q", name)
		}
		return &scriptedAdapter{name: name, outs: ptr, calls: calls, mu: mu}, nil
	}
}

const (
	// A hard task that routes to claude when forced, with a cross-model reviewer.
	reviewTask     = "refactor the concurrent scheduler to fix the race condition and deadlock"
	reviewApproved = `{"verdict":"approved","findings":[],"summary":"looks correct"}`
	reviewCritical = `{"verdict":"changes_requested","findings":[{"severity":"critical","title":"race remains","message":"guard the map write"}],"summary":"one critical issue"}`
)

// readReview reads the "review" sub-map recorded on the authoritative record at
// key, failing the test if it is absent or malformed.
func readReview(t *testing.T, mem *memory.SharedMemory, key string) map[string]any {
	t.Helper()
	stored, ok := mem.Read(key)
	if !ok {
		t.Fatalf("Read(%s) not found", key)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored %s is %T, want map", key, stored)
	}
	rev, ok := entry["review"].(map[string]any)
	if !ok {
		t.Fatalf("authoritative record %s has no review sub-map: %v", key, entry)
	}
	return rev
}

// TestDispatchWithReviewApproved: (i) when EVERY cross-model reviewer approves (no
// critical finding), the dispatch stays ok, the AGGREGATE review is recorded on the
// authoritative record and in the dispatch log with the comma-joined reviewer list,
// and the reviewers are the cross-model agents (not the author).
func TestDispatchWithReviewApproved(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	// author claude -> reviewers agy + codex, both approve.
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
		"agy":    {reviewApproved},
		"codex":  {reviewApproved},
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	// One author run + two concurrent reviews = 3 adapter invocations.
	if calls != 3 {
		t.Fatalf("adapter invocations = %d, want 3 (author + two reviews)", calls)
	}

	rev := readReview(t, mem, got.Key)
	// The aggregate reviewer_agent is the comma-joined sorted reviewer list.
	if rev["reviewer_agent"] != "agy,codex" {
		t.Fatalf("aggregate reviewer_agent = %v, want %q", rev["reviewer_agent"], "agy,codex")
	}
	if rev["verdict"] != "approved" {
		t.Fatalf("aggregate verdict = %v, want approved", rev["verdict"])
	}
	if rr, _ := rev["revision_rounds"].(float64); rr != 0 {
		t.Fatalf("revision_rounds = %v, want 0", rev["revision_rounds"])
	}

	// The dispatch.log line carries the aggregate review too.
	entry := readOnlyLogEntry(t, mem)
	if entry.Review == nil {
		t.Fatalf("dispatch.log entry has no review field")
	}
	if entry.Review.ReviewerAgent != "agy,codex" || entry.Review.Verdict != "approved" {
		t.Fatalf("logged review = %+v, want reviewer %q verdict approved", entry.Review, "agy,codex")
	}

	// Per-reviewer outcomes are exposed on the Result.
	if len(got.Reviews) != 2 {
		t.Fatalf("Result.Reviews len = %d, want 2", len(got.Reviews))
	}
}

// TestDispatchWithReviewPopulatesResultReview pins the host-visibility gap fix:
// a review:true dispatch must carry EVERY reviewer's outcome on the returned
// Result.Reviews — not just in memory/log — so the caller can inspect WHAT each
// reviewer found. A review-off Dispatch must leave Result.Reviews empty (legacy
// shape).
func TestDispatchWithReviewPopulatesResultReview(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
		"agy":    {reviewApproved},
		"codex":  {reviewApproved},
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if len(got.Reviews) != 2 {
		t.Fatalf("review:true dispatch: len(Result.Reviews) = %d, want 2", len(got.Reviews))
	}
	for _, r := range got.Reviews {
		if r.Verdict == "" {
			t.Fatalf("Result.Reviews entry has empty verdict: %+v", r)
		}
		if r.ReviewerAgent == "claude" {
			t.Fatalf("reviewer must not be the author: %+v", r)
		}
	}

	// A review-OFF dispatch must not carry any reviews (legacy Result shape).
	calls = 0
	off := New(mem, mgr)
	off.GetAdapter = scriptedGetAdapter(map[string][]string{"claude": {cannedJSON}}, &calls)
	res, err := off.Dispatch(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if len(res.Reviews) != 0 {
		t.Fatalf("review-off dispatch: Result.Reviews = %+v, want empty", res.Reviews)
	}
}

// TestRunReviewsConcurrentMultiReviewer exercises the concurrent multi-reviewer
// fan-out directly: for author claude both cross-model reviewers (agy, codex)
// review at once. When both approve, the status is ok and Result.Reviews carries
// both in sorted order, each "approved". When ONE reviewer flags critical, a
// single revision round runs (author invoked twice) and, when the re-review
// approves, the status is ok with the revision reflected in the aggregate record.
func TestRunReviewsConcurrentMultiReviewer(t *testing.T) {
	t.Run("all approve", func(t *testing.T) {
		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		calls := 0
		o := New(mem, mgr)
		o.GetAdapter = scriptedGetAdapter(map[string][]string{
			"claude": {cannedJSON},
			"agy":    {reviewApproved},
			"codex":  {reviewApproved},
		}, &calls)

		got, err := o.DispatchWithReview(reviewTask, "claude", "")
		if err != nil {
			t.Fatalf("DispatchWithReview error: %v", err)
		}
		if got.Status != "ok" {
			t.Fatalf("status = %q, want ok", got.Status)
		}
		if len(got.Reviews) != 2 {
			t.Fatalf("len(Reviews) = %d, want 2 (agy, codex)", len(got.Reviews))
		}
		if got.Reviews[0].ReviewerAgent != "agy" || got.Reviews[1].ReviewerAgent != "codex" {
			t.Fatalf("reviewers not in sorted order: %q, %q", got.Reviews[0].ReviewerAgent, got.Reviews[1].ReviewerAgent)
		}
		for _, r := range got.Reviews {
			if r.Verdict != "approved" {
				t.Fatalf("reviewer %q verdict = %q, want approved", r.ReviewerAgent, r.Verdict)
			}
		}
	})

	t.Run("one critical forces a revision round", func(t *testing.T) {
		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		calls := 0
		o := New(mem, mgr)
		o.GetAdapter = scriptedGetAdapter(map[string][]string{
			"claude": {cannedJSON, cannedJSON},
			"agy":    {reviewApproved, reviewApproved},
			"codex":  {reviewCritical, reviewApproved},
		}, &calls)

		got, err := o.DispatchWithReview(reviewTask, "claude", "")
		if err != nil {
			t.Fatalf("DispatchWithReview error: %v", err)
		}
		if got.Status != "ok" {
			t.Fatalf("status = %q, want ok (re-review approved)", got.Status)
		}
		// Author invoked twice (original + revision); 2 reviewers x 2 rounds = 4
		// reviews, plus 2 author runs = 6 total.
		if calls != 6 {
			t.Fatalf("adapter invocations = %d, want 6 (author twice + 2 reviewers x 2 rounds)", calls)
		}
		rev := readReview(t, mem, got.Key)
		if rr, _ := rev["revision_rounds"].(float64); rr != 1 {
			t.Fatalf("aggregate revision_rounds = %v, want 1", rev["revision_rounds"])
		}
	})
}

// TestDispatchWithReviewCriticalThenApproved: (ii) a critical finding from ANY
// reviewer forces ONE revision; the re-review (all reviewers) approves, so the
// dispatch is ok with revision_rounds=1.
func TestDispatchWithReviewCriticalThenApproved(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	// One reviewer (codex) flags critical in round 1 then approves; the other
	// (agy) approves both rounds. A single critical is enough to force a revision.
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON, cannedJSON},         // author #1, author #2 (revision)
		"agy":    {reviewApproved, reviewApproved}, // round1, round2
		"codex":  {reviewCritical, reviewApproved}, // round1 critical, round2 approved
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok (re-review approved)", got.Status)
	}
	// author#1 + 2 reviews + author#2 + 2 re-reviews = 6 invocations.
	if calls != 6 {
		t.Fatalf("adapter invocations = %d, want 6 (author, 2 reviews, revised author, 2 re-reviews)", calls)
	}

	rev := readReview(t, mem, got.Key)
	if rr, _ := rev["revision_rounds"].(float64); rr != 1 {
		t.Fatalf("revision_rounds = %v, want 1", rev["revision_rounds"])
	}
	if rev["final_status"] != "ok" {
		t.Fatalf("review final_status = %v, want ok", rev["final_status"])
	}
}

// TestDispatchWithReviewStillCritical: (iii) a critical finding that SURVIVES the
// single revision round gates the dispatch (status review_failed) and there is NO
// second revision round — exactly 6 adapter invocations.
func TestDispatchWithReviewStillCritical(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON, cannedJSON},
		"agy":    {reviewApproved, reviewApproved},
		"codex":  {reviewCritical, reviewCritical}, // critical persists after the revision
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "review_failed" {
		t.Fatalf("status = %q, want review_failed (critical survived the revision)", got.Status)
	}
	// Exactly one revision round: author, 2 reviews, revised author, 2 re-reviews. NO more.
	if calls != 6 {
		t.Fatalf("adapter invocations = %d, want 6 (no second revision round)", calls)
	}

	rev := readReview(t, mem, got.Key)
	if rr, _ := rev["revision_rounds"].(float64); rr != 1 {
		t.Fatalf("revision_rounds = %v, want 1", rev["revision_rounds"])
	}
	if rev["final_status"] != "review_failed" {
		t.Fatalf("review final_status = %v, want review_failed", rev["final_status"])
	}
}

// TestDispatchNoReviewLogLineHasNoReviewKey: (iv) the review-OFF path (plain
// Dispatch) writes a dispatch.log line with NO "review" key — the backward-compat
// byte invariant — and never invokes a reviewer.
func TestDispatchNoReviewLogLineHasNoReviewKey(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
	}, &calls)

	if _, err := o.Dispatch(reviewTask, "claude", ""); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	// Only the author ran: no reviewer adapter was fetched or invoked.
	if calls != 1 {
		t.Fatalf("adapter invocations = %d, want 1 (author only, no review)", calls)
	}

	raw, err := os.ReadFile(filepath.Join(mem.Root(), "dispatch.log"))
	if err != nil {
		t.Fatalf("read dispatch.log: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	// Byte-level backward compat: the review key must be entirely absent (omitempty).
	if strings.Contains(line, "\"review\"") {
		t.Fatalf("review-OFF dispatch.log line contains a review key: %s", line)
	}
	var entry dispatchLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("dispatch.log line is not valid JSON: %v (%q)", err, line)
	}
	if entry.Review != nil {
		t.Fatalf("review-OFF entry has non-nil Review: %+v", entry.Review)
	}
}

// TestDispatchWithReviewAdapterErrorPreservesAuthorStatus: (v) the reviewer
// adapter itself failing (RunWith error) is a best-effort reviewer failure —
// the author's dispatch status/result must be preserved unchanged and the
// review sub-record must be marked errored:true.
func TestDispatchWithReviewAdapterErrorPreservesAuthorStatus(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
		// No scripted output for EITHER reviewer -> both RunWith calls error, so
		// EVERY per-reviewer record is errored and the aggregate is errored:true.
		"agy":   {},
		"codex": {},
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok (author status preserved despite reviewer failure)", got.Status)
	}

	rev := readReview(t, mem, got.Key)
	if rev["errored"] != true {
		t.Fatalf("review errored = %v, want true", rev["errored"])
	}
	if rev["final_status"] != "ok" {
		t.Fatalf("review final_status = %v, want ok", rev["final_status"])
	}
}

// TestDispatchWithReviewUnparseableOutputPreservesAuthorStatus: (vi) a
// reviewer that runs successfully but emits output with no parseable review
// JSON block is also a best-effort reviewer failure with the same contract.
func TestDispatchWithReviewUnparseableOutputPreservesAuthorStatus(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
		// Both reviewers emit output with no parseable review JSON block.
		"agy":   {"I looked at it but forgot to emit any JSON block."},
		"codex": {"No JSON here either, sorry."},
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok (author status preserved despite unparseable review)", got.Status)
	}

	rev := readReview(t, mem, got.Key)
	if rev["errored"] != true {
		t.Fatalf("review errored = %v, want true", rev["errored"])
	}
	if rev["final_status"] != "ok" {
		t.Fatalf("review final_status = %v, want ok", rev["final_status"])
	}
}

// TestDispatchWithReviewNoCandidatePreservesAuthorStatus: (vii) SelectReviewers
// itself failing (no cross-model reviewers available) — routing's real config
// always has 3 agents so this can't happen through real routing from this
// package's tests, hence the selectReviewers seam is stubbed to force it. Same
// best-effort contract as an adapter/parse failure: author status preserved,
// review record errored:true.
func TestDispatchWithReviewNoCandidatePreservesAuthorStatus(t *testing.T) {
	saved := selectReviewers
	selectReviewers = func(task, priority, excludeAuthor string) ([]routing.Selection, error) {
		return nil, errors.New("routing: no cross-model reviewers available excluding \"claude\"")
	}
	t.Cleanup(func() { selectReviewers = saved })

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON},
	}, &calls)

	got, err := o.DispatchWithReview(reviewTask, "claude", "")
	if err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok (author status preserved when no reviewer is available)", got.Status)
	}
	// Only the author ran: no reviewer adapter was ever fetched/invoked.
	if calls != 1 {
		t.Fatalf("adapter invocations = %d, want 1 (author only, SelectReviewer failed before any reviewer adapter call)", calls)
	}

	rev := readReview(t, mem, got.Key)
	if rev["errored"] != true {
		t.Fatalf("review errored = %v, want true", rev["errored"])
	}
	if rev["final_status"] != "ok" {
		t.Fatalf("review final_status = %v, want ok", rev["final_status"])
	}
}

// TestReviewerArgsAreReadOnlyVariant: (viii) the reviewer invocation must use
// the READ-ONLY per-CLI privilege grant from buildDispatchArgs(reviewMode=true),
// not the author's write-capable grant — captured via a captureAdapter swapped
// in for whichever agent SelectReviewer resolves to.
func TestReviewerArgsAreReadOnlyVariant(t *testing.T) {
	revSel, err := routing.SelectReviewer(reviewTask, "", "claude")
	if err != nil {
		t.Fatalf("SelectReviewer error: %v", err)
	}

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	authorCap := &captureAdapter{name: "claude", canned: cannedJSON}
	reviewCap := &captureAdapter{name: revSel.Agent, canned: reviewApproved}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		if name == "claude" {
			return authorCap, nil
		}
		if name == revSel.Agent {
			return reviewCap, nil
		}
		return nil, fmt.Errorf("unexpected adapter %q", name)
	}

	if _, err := o.DispatchWithReview(reviewTask, "claude", ""); err != nil {
		t.Fatalf("DispatchWithReview error: %v", err)
	}

	switch revSel.Agent {
	case "claude":
		if !containsPair(reviewCap.gotExtra, "--permission-mode", "plan") {
			t.Fatalf("reviewer (claude) extras missing --permission-mode plan: %v", reviewCap.gotExtra)
		}
		if containsPair(reviewCap.gotExtra, "--permission-mode", "acceptEdits") {
			t.Fatalf("reviewer (claude) must NOT get the author's acceptEdits grant: %v", reviewCap.gotExtra)
		}
		if !hasFlag(reviewCap.gotExtra, "Write") {
			t.Fatalf("reviewer (claude) extras missing --disallowedTools Write: %v", reviewCap.gotExtra)
		}
	case "codex":
		if !containsPair(reviewCap.gotExtra, "-s", "read-only") {
			t.Fatalf("reviewer (codex) extras missing -s read-only: %v", reviewCap.gotExtra)
		}
		if containsPair(reviewCap.gotExtra, "-s", "workspace-write") {
			t.Fatalf("reviewer (codex) must NOT get the author's workspace-write sandbox: %v", reviewCap.gotExtra)
		}
	case "agy":
		// agy has no scoped read-only flag; the grant is unchanged from the
		// author's (see buildDispatchArgs doc) and the reviewer relies on the
		// prompt-level instruction instead.
		if !hasFlag(reviewCap.gotExtra, "--dangerously-skip-permissions") {
			t.Fatalf("reviewer (agy) extras missing --dangerously-skip-permissions: %v", reviewCap.gotExtra)
		}
	}
}

// readOnlyLogEntry reads the single dispatch.log line and unmarshals it.
func readOnlyLogEntry(t *testing.T, mem *memory.SharedMemory) dispatchLogEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mem.Root(), "dispatch.log"))
	if err != nil {
		t.Fatalf("read dispatch.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("dispatch.log has %d lines, want 1: %q", len(lines), string(raw))
	}
	var entry dispatchLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("dispatch.log line is not valid JSON: %v (%q)", err, lines[0])
	}
	return entry
}

// TestValidateVerifyCmds pins the security contract of the objective verify gate:
// an allowlisted single-program command is accepted, while anything carrying a
// shell metacharacter/operator or a non-allowlisted first token is refused BEFORE
// anything could run. These are the exact rejection cases the design calls out.
func TestValidateVerifyCmds(t *testing.T) {
	accept := []string{"go test ./...", "npm test", "golangci-lint run", "python3 -m pytest", "gosec ./...", "govulncheck ./..."}
	for _, cmd := range accept {
		if err := ValidateVerifyCmds([]string{cmd}); err != nil {
			t.Fatalf("ValidateVerifyCmds(%q) = %v, want nil (allowlisted, shell-free)", cmd, err)
		}
	}
	// A whole list of valid commands is accepted together.
	if err := ValidateVerifyCmds(accept); err != nil {
		t.Fatalf("ValidateVerifyCmds(%v) = %v, want nil", accept, err)
	}
	// A nil/empty list is trivially valid (no verify requested).
	if err := ValidateVerifyCmds(nil); err != nil {
		t.Fatalf("ValidateVerifyCmds(nil) = %v, want nil", err)
	}

	reject := []string{
		"rm -rf /",            // non-allowlisted program
		"go test | tee x",     // pipe metacharacter
		"echo $(whoami)",      // command substitution ($ and parens)
		"foo && bar",          // command chain + non-allowlisted
		"curl http://x/y.sh",  // non-allowlisted program
		"gosec ./... | tee x", // allowlisted program, but pipe metacharacter must still reject
	}
	for _, cmd := range reject {
		if err := ValidateVerifyCmds([]string{cmd}); err == nil {
			t.Fatalf("ValidateVerifyCmds(%q) = nil, want an error (must be rejected)", cmd)
		}
	}
	// One bad command in an otherwise-valid list rejects the whole list.
	if err := ValidateVerifyCmds([]string{"go test ./...", "rm -rf /"}); err == nil {
		t.Fatalf("ValidateVerifyCmds with one bad command = nil, want an error")
	}
}

// TestRunVerifyPassAndFail exercises the executor: a passing allowlisted command
// yields Passed=true with no FailedCmd, and a failing one yields Passed=false with
// the failing command, a non-zero exit code, and captured output recorded. It also
// asserts the stop-at-first-failure ordering.
func TestRunVerifyPassAndFail(t *testing.T) {
	// A passing command (go version exits 0 regardless of cwd).
	pass := runVerify(t.TempDir(), []string{"go version"})
	if !pass.Passed {
		t.Fatalf("runVerify(go version).Passed = false, want true (out=%q)", pass.Output)
	}
	if pass.FailedCmd != "" {
		t.Fatalf("passing verify has FailedCmd %q, want empty", pass.FailedCmd)
	}

	// A failing command: go vet on a non-existent package exits non-zero.
	const bad = "go vet ./this-does-not-exist-xyz"
	fail := runVerify(t.TempDir(), []string{bad})
	if fail.Passed {
		t.Fatalf("runVerify(%q).Passed = true, want false", bad)
	}
	if fail.FailedCmd != bad {
		t.Fatalf("failed verify FailedCmd = %q, want %q", fail.FailedCmd, bad)
	}
	if fail.ExitCode == 0 {
		t.Fatalf("failed verify ExitCode = 0, want non-zero")
	}
	if fail.Output == "" {
		t.Fatalf("failed verify captured no output, want the command's combined output")
	}

	// Stop at first failure: the failing command is first, so the later passing
	// command never runs and the FailedCmd is the first one.
	seq := runVerify(t.TempDir(), []string{bad, "go version"})
	if seq.Passed || seq.FailedCmd != bad {
		t.Fatalf("runVerify stop-at-first-failure: Passed=%v FailedCmd=%q, want false / %q", seq.Passed, seq.FailedCmd, bad)
	}
}

// TestDispatchModelVerifyGate proves the verify gate is wired into the dispatch
// pipeline: a passing verify attaches a Passed VerifyResult and leaves the
// author's status untouched, while a failing verify attaches a failed VerifyResult
// and flips the status to "verify_failed" — WITHOUT erroring the dispatch and
// without discarding the author's result.
func TestDispatchModelVerifyGate(t *testing.T) {
	t.Run("passing verify keeps status", func(t *testing.T) {
		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		var argv []string
		o := New(mem, mgr)
		o.GetAdapter = func(name string) (agent.Adapter, error) {
			return newRecordingAdapter(t, name, cannedJSON, &argv), nil
		}

		got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"go version"}, "")
		if err != nil {
			t.Fatalf("DispatchModel error: %v", err)
		}
		if got.Verify == nil {
			t.Fatalf("Result.Verify is nil, want a populated VerifyResult")
		}
		if !got.Verify.Passed {
			t.Fatalf("Result.Verify.Passed = false, want true (out=%q)", got.Verify.Output)
		}
		if got.Status != "ok" {
			t.Fatalf("status = %q, want ok (author status unchanged on passing verify)", got.Status)
		}
		if got.VerifyRevisions != 0 {
			t.Fatalf("VerifyRevisions = %d, want 0 (verify passed first try, no revision)", got.VerifyRevisions)
		}
	})

	t.Run("failing verify sets verify_failed", func(t *testing.T) {
		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		var argv []string
		o := New(mem, mgr)
		o.GetAdapter = func(name string) (agent.Adapter, error) {
			return newRecordingAdapter(t, name, cannedJSON, &argv), nil
		}

		got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"go vet ./this-does-not-exist-xyz"}, "")
		if err != nil {
			t.Fatalf("DispatchModel error: %v (a failing verify must NOT error the dispatch)", err)
		}
		if got.Verify == nil || got.Verify.Passed {
			t.Fatalf("Result.Verify = %+v, want a failed VerifyResult", got.Verify)
		}
		if got.Status != "verify_failed" {
			t.Fatalf("status = %q, want verify_failed", got.Status)
		}
		// The author's result payload is preserved (gate does not discard work).
		if got.Result != "did X" {
			t.Fatalf("result = %q, want the author's result preserved", got.Result)
		}
	})
}

// verifyFlipAdapter is a fake author whose file writes deterministically flip a
// REAL allowlisted verify command (`gofmt marker.go`) between fail and pass. The
// verify command fails while marker.go is absent (gofmt on a missing file exits
// non-zero) and passes once a syntactically valid Go file has been written there.
// The adapter writes marker.go into dir starting on the writeFrom-th RunWith call
// (writeFrom == 0 => never writes, so verify keeps failing). A single shared
// instance is reused across re-dispatches so calls counts author runs across the
// whole pipeline.
type verifyFlipAdapter struct {
	name      string
	dir       string
	writeFrom int
	calls     int
}

func (a *verifyFlipAdapter) Name() string                             { return a.name }
func (a *verifyFlipAdapter) BuildCommand(task, model string) []string { return nil }
func (a *verifyFlipAdapter) BuildCommandWith(task, model string, extra []string) []string {
	return nil
}
func (a *verifyFlipAdapter) Run(task, model string) (string, error) { return "", nil }
func (a *verifyFlipAdapter) RunWith(task, model string, extra []string) (string, error) {
	a.calls++
	if a.writeFrom > 0 && a.calls >= a.writeFrom {
		if err := os.WriteFile(filepath.Join(a.dir, "marker.go"), []byte("package flip\n"), 0o644); err != nil {
			return "", err
		}
	}
	return cannedJSON, nil
}

// TestDispatchModelVerifyReviseSucceeds proves the bounded auto-revision: the
// first author run leaves the tree failing verify (marker.go absent) and the
// single re-dispatched run writes marker.go so verify passes — EXACTLY ONE
// revision runs and the final status is the author's "ok".
func TestDispatchModelVerifyReviseSucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // process cwd == dir, where runAuthor/runVerify resolve os.Getwd

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	// Write marker.go on the SECOND author run (the revision) so verify flips
	// fail -> pass after exactly one automatic round.
	ad := &verifyFlipAdapter{name: "claude", dir: dir, writeFrom: 2}
	o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

	got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"gofmt marker.go"}, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	if got.VerifyRevisions != 1 {
		t.Fatalf("VerifyRevisions = %d, want 1 (one automatic revision then pass)", got.VerifyRevisions)
	}
	if got.Verify == nil || !got.Verify.Passed {
		t.Fatalf("Result.Verify = %+v, want a passing VerifyResult after the revision", got.Verify)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok (verify passed after the revision)", got.Status)
	}
	if ad.calls != 2 {
		t.Fatalf("author ran %d times, want 2 (initial + one revision)", ad.calls)
	}
}

// TestDispatchModelVerifyReviseCapsOut proves the loop is BOUNDED: when verify
// keeps failing (marker.go never written), the automatic revisions stop at the
// clamped hard maximum (no infinite loop) and the final status is verify_failed.
func TestDispatchModelVerifyReviseCapsOut(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	o.VerifyReviseRounds = 100 // absurdly high: must clamp to verifyReviseRoundsMax
	// writeFrom == 0: never writes marker.go, so `gofmt marker.go` always fails.
	ad := &verifyFlipAdapter{name: "claude", dir: dir, writeFrom: 0}
	o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

	got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"gofmt marker.go"}, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v (a failing verify must NOT error the dispatch)", err)
	}
	if got.VerifyRevisions != verifyReviseRoundsMax {
		t.Fatalf("VerifyRevisions = %d, want the clamped max %d", got.VerifyRevisions, verifyReviseRoundsMax)
	}
	if got.Verify == nil || got.Verify.Passed {
		t.Fatalf("Result.Verify = %+v, want a failed VerifyResult", got.Verify)
	}
	if got.Status != "verify_failed" {
		t.Fatalf("status = %q, want verify_failed", got.Status)
	}
	// Deterministic termination: initial author run + exactly the clamped number
	// of revision runs, never more.
	if ad.calls != 1+verifyReviseRoundsMax {
		t.Fatalf("author ran %d times, want %d (initial + clamped revisions)", ad.calls, 1+verifyReviseRoundsMax)
	}
}

// workdirAdapter is a fake author that records the process dir it was anchored to
// (via SetDir) and the extras it was handed, and optionally writes a valid Go
// file into that dir on RunWith. It lets a workdir dispatch prove BOTH that the
// adapter was SetDir'd to the workdir AND that the verify gate ran there (a
// `gofmt marker.go` verify passes only because the file was written into — and
// verify ran in — the workdir).
type workdirAdapter struct {
	name     string
	writeGo  bool
	dir      string // captured via SetDir
	gotExtra []string
}

func (a *workdirAdapter) Name() string                             { return a.name }
func (a *workdirAdapter) BuildCommand(task, model string) []string { return nil }
func (a *workdirAdapter) BuildCommandWith(task, model string, extra []string) []string {
	return nil
}
func (a *workdirAdapter) Run(task, model string) (string, error) { return "", nil }
func (a *workdirAdapter) SetDir(dir string)                      { a.dir = dir }
func (a *workdirAdapter) RunWith(task, model string, extra []string) (string, error) {
	a.gotExtra = extra
	if a.writeGo && a.dir != "" {
		if err := os.WriteFile(filepath.Join(a.dir, "marker.go"), []byte("package flip\n"), 0o644); err != nil {
			return "", err
		}
	}
	return cannedJSON, nil
}

// TestDispatchModelWorkdir proves the per-dispatch working directory (FIX A):
// the workdir is created if missing, the author adapter is anchored to it via
// SetDir, the claude author is granted --add-dir <workdir> / the codex author
// -C <workdir>, and the verify gate runs IN the workdir. An empty workdir adds
// no grant (byte-identical extras).
func TestDispatchModelWorkdir(t *testing.T) {
	t.Run("claude: created, SetDir, --add-dir, verify runs in workdir", func(t *testing.T) {
		// Nested/absent path so MkdirAll's create-if-missing is exercised.
		workdir := filepath.Join(t.TempDir(), "nested", "target")

		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		o := New(mem, mgr)
		ad := &workdirAdapter{name: "claude", writeGo: true}
		o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

		// Verify `gofmt marker.go` can only pass if verify runs in the workdir,
		// where the author wrote marker.go (the adapter writes into its SetDir dir).
		got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"gofmt marker.go"}, workdir)
		if err != nil {
			t.Fatalf("DispatchModel error: %v", err)
		}
		if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
			t.Fatalf("workdir %q not created as a directory: err=%v", workdir, err)
		}
		if ad.dir != workdir {
			t.Fatalf("adapter SetDir = %q, want the workdir %q", ad.dir, workdir)
		}
		if !containsPair(ad.gotExtra, "--add-dir", workdir) {
			t.Fatalf("claude extras missing --add-dir %q (workdir grant): %v", workdir, ad.gotExtra)
		}
		if got.Verify == nil || !got.Verify.Passed {
			t.Fatalf("verify should PASS in the workdir (marker.go was written there): %+v", got.Verify)
		}
		if got.Status != "ok" {
			t.Fatalf("status = %q, want ok (verify passed in workdir)", got.Status)
		}
	})

	t.Run("codex: -C workdir", func(t *testing.T) {
		workdir := t.TempDir()
		mem := memory.New(t.TempDir())
		mgr, _ := newFakeManager(0)
		o := New(mem, mgr)
		ad := &workdirAdapter{name: "codex"}
		o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

		if _, err := o.DispatchModel("add a helper", "codex", "", "", "", "", false, nil, workdir); err != nil {
			t.Fatalf("DispatchModel error: %v", err)
		}
		if ad.dir != workdir {
			t.Fatalf("adapter SetDir = %q, want the workdir %q", ad.dir, workdir)
		}
		if !containsPair(ad.gotExtra, "-C", workdir) {
			t.Fatalf("codex extras missing -C %q (workdir grant): %v", workdir, ad.gotExtra)
		}
	})

	t.Run("empty workdir adds no grant (byte-identical)", func(t *testing.T) {
		const sys, memDir, cwd = "SYS", "/mem", "/cwd"

		_, claudeEmpty := buildDispatchArgs("claude", "t", sys, memDir, cwd, false, "", "")
		_, claudeWD := buildDispatchArgs("claude", "t", sys, memDir, cwd, false, "", "/wd")
		// Empty workdir must not grant either the resolved cwd or a workdir.
		if containsPair(claudeEmpty, "--add-dir", cwd) || containsPair(claudeEmpty, "--add-dir", "/wd") {
			t.Fatalf("claude empty-workdir extras must add no cwd/workdir --add-dir: %v", claudeEmpty)
		}
		if !containsPair(claudeWD, "--add-dir", "/wd") {
			t.Fatalf("claude workdir extras missing --add-dir /wd: %v", claudeWD)
		}
		if len(claudeWD) != len(claudeEmpty)+2 {
			t.Fatalf("claude workdir added %d args, want exactly 2 (--add-dir /wd): empty=%v wd=%v", len(claudeWD)-len(claudeEmpty), claudeEmpty, claudeWD)
		}

		_, codexEmpty := buildDispatchArgs("codex", "t", sys, memDir, cwd, false, "", "")
		_, codexWD := buildDispatchArgs("codex", "t", sys, memDir, cwd, false, "", "/wd")
		if hasFlag(codexEmpty, "-C") {
			t.Fatalf("codex empty-workdir extras must add no -C: %v", codexEmpty)
		}
		if !containsPair(codexWD, "-C", "/wd") {
			t.Fatalf("codex workdir extras missing -C /wd: %v", codexWD)
		}
		if len(codexWD) != len(codexEmpty)+2 {
			t.Fatalf("codex workdir added %d args, want exactly 2 (-C /wd): empty=%v wd=%v", len(codexWD)-len(codexEmpty), codexEmpty, codexWD)
		}
	})
}

// TestDispatchModelVerifyFailSyncsMemoryStatus proves FIX B: when the verify gate
// flips the returned status to "verify_failed", the authoritative memory record
// is reconciled to that status instead of keeping the author's raw status ("ok").
func TestDispatchModelVerifyFailSyncsMemoryStatus(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	var argv []string
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, cannedJSON, &argv), nil
	}

	// cannedJSON's author status is "ok"; a cwd-independent always-failing verify
	// forces verify_failed, so the record's persisted "ok" must be synced.
	got, err := o.DispatchModel("add a helper", "claude", "", "", "", "", false, []string{"go vet ./this-does-not-exist-xyz"}, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	if got.Status != "verify_failed" {
		t.Fatalf("returned status = %q, want verify_failed", got.Status)
	}
	stored, ok := mem.Read(got.Key)
	if !ok {
		t.Fatalf("Read(%s) not found", got.Key)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stored %s is %T, want map", got.Key, stored)
	}
	if entry["status"] != "verify_failed" {
		t.Fatalf("persisted memory status = %v, want verify_failed (must agree with the returned status)", entry["status"])
	}
}

// TestDispatchModelVerifyFailWithReviewCoherentSummary proves FIX C: a review=true
// dispatch whose verify fails and triggers a revision returns a Result that is
// NOT self-contradictory — reviews are populated AND the summary states the
// verify gate failed (not the reviewless revision agent's raw summary claiming
// nothing about review).
func TestDispatchModelVerifyFailWithReviewCoherentSummary(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	calls := 0
	o := New(mem, mgr)
	// Author (claude) runs twice: initial + one verify revision. Reviewers approve
	// once each (review runs before verify, on the initial result only).
	o.GetAdapter = scriptedGetAdapter(map[string][]string{
		"claude": {cannedJSON, cannedJSON},
		"agy":    {reviewApproved},
		"codex":  {reviewApproved},
	}, &calls)

	got, err := o.DispatchModel(reviewTask, "claude", "", "", "", "", true, []string{"go vet ./this-does-not-exist-xyz"}, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	if got.Status != "verify_failed" {
		t.Fatalf("status = %q, want verify_failed", got.Status)
	}
	if got.VerifyRevisions != 1 {
		t.Fatalf("VerifyRevisions = %d, want 1", got.VerifyRevisions)
	}
	if len(got.Reviews) == 0 {
		t.Fatalf("Reviews is empty; want the pre-revision reviewers preserved")
	}
	if !strings.Contains(got.Summary, "verify gate failed") {
		t.Fatalf("Summary = %q, want a jindo-composed summary containing %q (not the raw agent summary)", got.Summary, "verify gate failed")
	}
}

// cannedPlanJSON is a realistic planner stdout: prose followed by a final JSON
// block carrying the plan contract (two ordered steps + a summary).
const cannedPlanJSON = `I read the shared memory and decomposed the goal.

{"steps":[{"id":"s1","title":"add PlanStep","difficulty":"standard","suggested_model":"claude-sonnet-5","suggested_verify":["go build ./..."],"depends_on":[]},{"id":"s2","title":"wire the tool","difficulty":"hard","suggested_model":"claude-opus-4-8","suggested_verify":["go test ./..."],"depends_on":["s1"]}],"summary":"two-step plan"}`

// TestPlanDefaultsToClaudeHardModelAndPersists proves Plan with no agent/model
// (a) defaults the agent to claude and derives its hard-tier model, (b) runs the
// planner READ-ONLY (claude reviewMode extras: --permission-mode plan), (c)
// returns the parsed steps/summary, and (d) persists a readable plan entry under
// the returned key.
func TestPlanDefaultsToClaudeHardModelAndPersists(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)

	cap := &captureAdapter{name: "claude", canned: cannedPlanJSON}
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		if name != "claude" {
			t.Fatalf("GetAdapter got %q, want claude (default planning agent)", name)
		}
		return cap, nil
	}

	res, err := o.Plan("add a plan MCP tool", "", "", "")
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}

	// (a) agent+model derivation.
	if res.Agent != "claude" {
		t.Errorf("res.Agent = %q, want claude", res.Agent)
	}
	if res.Model != "claude-opus-4-8" {
		t.Errorf("res.Model = %q, want claude-opus-4-8 (claude hard tier)", res.Model)
	}
	if cap.gotModel != "claude-opus-4-8" {
		t.Errorf("adapter got model %q, want claude-opus-4-8", cap.gotModel)
	}

	// (b) read-only planner run: claude's reviewMode grant blocks edits.
	if !containsSeq(cap.gotExtra, "--permission-mode", "plan") {
		t.Errorf("adapter extras %v do not carry read-only --permission-mode plan", cap.gotExtra)
	}

	// (c) parsed steps + summary.
	if len(res.Steps) != 2 {
		t.Fatalf("len(res.Steps) = %d, want 2", len(res.Steps))
	}
	if res.Steps[0].ID != "s1" || res.Steps[1].DependsOn[0] != "s1" {
		t.Errorf("steps not parsed as expected: %+v", res.Steps)
	}
	if res.Summary != "two-step plan" {
		t.Errorf("res.Summary = %q, want %q", res.Summary, "two-step plan")
	}

	// (d) persisted under the returned key and readable.
	if res.Key == "" {
		t.Fatalf("res.Key empty; plan was not persisted")
	}
	stored, ok := mem.Read(res.Key)
	if !ok {
		t.Fatalf("mem.Read(%q) not found; plan not persisted", res.Key)
	}
	b, _ := json.Marshal(stored)
	if !strings.Contains(string(b), "add PlanStep") || !strings.Contains(string(b), "two-step plan") {
		t.Errorf("persisted plan does not carry the steps/summary: %s", b)
	}
}

// TestPlanPinsExplicitModel proves a non-empty model is pinned verbatim and
// handed to the adapter, bypassing the hard-tier derivation.
func TestPlanPinsExplicitModel(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)

	const pinned = "claude-haiku-4-5"
	cap := &captureAdapter{name: "claude", canned: cannedPlanJSON}
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	res, err := o.Plan("small goal", "", "claude", pinned)
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Model != pinned {
		t.Errorf("res.Model = %q, want pinned %q", res.Model, pinned)
	}
	if cap.gotModel != pinned {
		t.Errorf("adapter got model %q, want pinned %q", cap.gotModel, pinned)
	}
}

// TestPlanUnparseableIsError proves a planner run that emits no parseable plan
// is a hard error (nothing useful to return).
func TestPlanUnparseableIsError(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)

	cap := &captureAdapter{name: "claude", canned: "I could not produce a plan."}
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	if _, err := o.Plan("goal", "", "", ""); err == nil {
		t.Fatalf("Plan with unparseable planner output returned nil error, want error")
	}
}

// containsSeq reports whether s contains a and the immediately following element
// b (an adjacent flag/value pair).
func containsSeq(s []string, a, b string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == a && s[i+1] == b {
			return true
		}
	}
	return false
}

// TestBuildDispatchArgsEffortPerAgent pins the per-CLI reasoning-effort wiring:
// with effort="high" claude gets --effort high, codex gets
// -c model_reasoning_effort=high, and agy (model-encoded effort) gets NEITHER.
// With effort="" no branch adds any effort flag (the backward-compat regression
// pin), and codex's extras stay exactly the pre-effort {-s workspace-write}.
func TestBuildDispatchArgsEffortPerAgent(t *testing.T) {
	const sys, mem, cwd = "SYS", "/mem", "/cwd"

	// effort="high": each agent emits (or omits) its own flag.
	_, claudeExtra := buildDispatchArgs("claude", "t", sys, mem, cwd, false, "high", "")
	if !containsPair(claudeExtra, "--effort", "high") {
		t.Fatalf("claude extras missing --effort high: %v", claudeExtra)
	}

	_, codexExtra := buildDispatchArgs("codex", "t", sys, mem, cwd, false, "high", "")
	if !containsPair(codexExtra, "-c", "model_reasoning_effort=high") {
		t.Fatalf("codex extras missing -c model_reasoning_effort=high: %v", codexExtra)
	}

	_, agyExtra := buildDispatchArgs("agy", "t", sys, mem, cwd, false, "high", "")
	if hasFlag(agyExtra, "--effort") {
		t.Fatalf("agy extras must NOT contain --effort (effort is model-encoded): %v", agyExtra)
	}
	for _, a := range agyExtra {
		if strings.HasPrefix(a, "model_reasoning_effort=") {
			t.Fatalf("agy extras must NOT contain model_reasoning_effort: %v", agyExtra)
		}
	}

	// effort="" regression pin: no effort flag on any agent, and codex's extras
	// are byte-identical to the pre-effort shape.
	_, claudeNone := buildDispatchArgs("claude", "t", sys, mem, cwd, false, "", "")
	if hasFlag(claudeNone, "--effort") {
		t.Fatalf("claude with effort=\"\" must add no --effort: %v", claudeNone)
	}
	_, codexNone := buildDispatchArgs("codex", "t", sys, mem, cwd, false, "", "")
	if hasFlag(codexNone, "-c") {
		t.Fatalf("codex with effort=\"\" must add no -c: %v", codexNone)
	}
	if len(codexNone) != 3 || codexNone[0] != "-s" || codexNone[1] != "workspace-write" ||
		codexNone[2] != "--ignore-user-config" {
		t.Fatalf("codex with effort=\"\" extras = %v, want [-s workspace-write --ignore-user-config]", codexNone)
	}
}

// TestBuildDispatchArgsSubAgentMCPIsolation pins the per-CLI flag that stops a
// dispatched sub-agent from reloading the HOST's MCP config (which registers
// jindo itself, and serena for codex) — without it, the sub-agent recurses into
// its own jindo MCP server and redundantly spawns every other configured MCP
// server. claude: --strict-mcp-config (no --mcp-config alongside it means zero
// MCP servers). codex: --ignore-user-config (skips ~/.codex/config.toml's
// mcp_servers table; auth still works via CODEX_HOME). agy exposes no MCP
// loading and has no such flag, so it must get neither.
func TestBuildDispatchArgsSubAgentMCPIsolation(t *testing.T) {
	const sys, mem, cwd = "SYS", "/mem", "/cwd"

	for _, reviewMode := range []bool{false, true} {
		_, claudeExtra := buildDispatchArgs("claude", "t", sys, mem, cwd, reviewMode, "", "")
		if !hasFlag(claudeExtra, "--strict-mcp-config") {
			t.Fatalf("claude extras (reviewMode=%v) missing --strict-mcp-config: %v", reviewMode, claudeExtra)
		}

		_, codexExtra := buildDispatchArgs("codex", "t", sys, mem, cwd, reviewMode, "", "")
		if !hasFlag(codexExtra, "--ignore-user-config") {
			t.Fatalf("codex extras (reviewMode=%v) missing --ignore-user-config: %v", reviewMode, codexExtra)
		}

		_, agyExtra := buildDispatchArgs("agy", "t", sys, mem, cwd, reviewMode, "", "")
		if hasFlag(agyExtra, "--strict-mcp-config") || hasFlag(agyExtra, "--ignore-user-config") {
			t.Fatalf("agy extras (reviewMode=%v) must contain no MCP isolation flag (agy has none): %v", reviewMode, agyExtra)
		}
	}
}

// TestEffortForCodexClampsMax pins the one effort-vocabulary incompatibility:
// claude's "max" has no codex equivalent, so it clamps to "xhigh"; every other
// level passes through unchanged.
func TestEffortForCodexClampsMax(t *testing.T) {
	if got := effortForCodex("max"); got != "xhigh" {
		t.Fatalf("effortForCodex(\"max\") = %q, want xhigh", got)
	}
	for _, e := range []string{"low", "medium", "high", "xhigh", ""} {
		if got := effortForCodex(e); got != e {
			t.Fatalf("effortForCodex(%q) = %q, want passthrough", e, got)
		}
	}
}

// TestDispatchModelTierDefaultEffort proves the author picks up the per-tier
// default effort when the caller passes no override: the adapter argv carries
// --effort <EffortForDifficulty(tier)> for the routed difficulty.
func TestDispatchModelTierDefaultEffort(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "claude", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	got, err := o.DispatchModel(task, "claude", "", "", "", "", false, nil, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	want := routing.EffortForDifficulty(got.Difficulty)
	if want == "" {
		t.Fatalf("test precondition: tier %q has no default effort", got.Difficulty)
	}
	if !containsPair(cap.gotExtra, "--effort", want) {
		t.Fatalf("author extras missing tier-default --effort %q for tier %q: %v", want, got.Difficulty, cap.gotExtra)
	}
}

// TestDispatchModelEffortOverrideWins proves a non-empty host effort override
// beats the per-tier default: the adapter argv carries the override regardless
// of the routed tier's configured effort.
func TestDispatchModelEffortOverrideWins(t *testing.T) {
	const task = "refactor the concurrent scheduler to fix the race condition and deadlock"

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "claude", canned: cannedJSON}

	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }

	got, err := o.DispatchModel(task, "claude", "", "", "", "xhigh", false, nil, "")
	if err != nil {
		t.Fatalf("DispatchModel error: %v", err)
	}
	if !containsPair(cap.gotExtra, "--effort", "xhigh") {
		t.Fatalf("author extras missing override --effort xhigh: %v", cap.gotExtra)
	}
	// And the tier default must NOT also appear (override replaced it).
	if def := routing.EffortForDifficulty(got.Difficulty); def != "xhigh" && containsPair(cap.gotExtra, "--effort", def) {
		t.Fatalf("author extras carry tier-default --effort %q as well as override: %v", def, cap.gotExtra)
	}
}

// modelScriptAdapter is a fake Adapter that scripts RunWith output PER MODEL, so
// a DispatchMulti fan-out (which routes each pinned model to its agent and calls
// RunWith with that model) can be driven deterministically from one instance
// shared across agents. A model listed in errModels makes RunWith return an
// error (the best-effort per-candidate failure path). Its fields are read-only
// during a run, so concurrent RunWith calls are race-safe.
type modelScriptAdapter struct {
	byModel   map[string]string
	errModels map[string]bool
}

func (m *modelScriptAdapter) Name() string                             { return "scripted" }
func (m *modelScriptAdapter) BuildCommand(task, model string) []string { return nil }
func (m *modelScriptAdapter) BuildCommandWith(task, model string, extra []string) []string {
	return nil
}
func (m *modelScriptAdapter) Run(task, model string) (string, error) { return m.byModel[model], nil }
func (m *modelScriptAdapter) RunWith(task, model string, extra []string) (string, error) {
	if m.errModels[model] {
		return "", fmt.Errorf("scripted adapter: forced error for model %q", model)
	}
	return m.byModel[model], nil
}

// proposeJSON builds a propose-mode agent stdout carrying result in the shared
// response contract ParseResponse reads.
func proposeJSON(result string) string {
	return `thought about it.

{"status":"ok","result":"` + result + `","summary":"proposed","memory_updates":[]}`
}

// TestDispatchMultiFansOutCandidates verifies DispatchMulti runs every pinned
// model concurrently in read-only propose mode and returns one candidate per
// model — with the right agent (inferred from the model id), model, and the
// model's own scripted result — and, without synthesis, no judge.
func TestDispatchMultiFansOutCandidates(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	ad := &modelScriptAdapter{byModel: map[string]string{
		"claude-opus-4-8": proposeJSON("opus answer"),
		"gpt-5.5":         proposeJSON("codex answer"),
	}}
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

	res, err := o.DispatchMulti("solve this", []string{"claude-opus-4-8", "gpt-5.5"}, "", "", "")
	if err != nil {
		t.Fatalf("DispatchMulti error: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(res.Candidates))
	}
	// Order matches the models argument.
	if res.Candidates[0].Model != "claude-opus-4-8" || res.Candidates[0].Agent != "claude" {
		t.Fatalf("candidate[0] = %+v, want model claude-opus-4-8 agent claude", res.Candidates[0])
	}
	if res.Candidates[0].Result != "opus answer" || res.Candidates[0].Status != "ok" {
		t.Fatalf("candidate[0] result/status = %q/%q, want %q/ok", res.Candidates[0].Result, res.Candidates[0].Status, "opus answer")
	}
	if res.Candidates[1].Model != "gpt-5.5" || res.Candidates[1].Agent != "codex" {
		t.Fatalf("candidate[1] = %+v, want model gpt-5.5 agent codex", res.Candidates[1])
	}
	if res.Candidates[1].Result != "codex answer" {
		t.Fatalf("candidate[1] result = %q, want %q", res.Candidates[1].Result, "codex answer")
	}
	if res.Synthesis != nil {
		t.Fatalf("synthesis = %+v, want nil (no judge requested)", res.Synthesis)
	}
	// A propose is not a recorded dispatch: no dispatch key was allocated.
	all, err := mem.All()
	if err != nil {
		t.Fatalf("All error: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("DispatchMulti wrote %d memory entries, want 0 (propose is not a recorded dispatch): %v", len(all), all)
	}
}

// TestDispatchMultiJudgeSynthesizes verifies synthesis=="judge" runs the judge
// model over the candidates and populates MultiResult.Synthesis with the judge's
// result.
func TestDispatchMultiJudgeSynthesizes(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	ad := &modelScriptAdapter{byModel: map[string]string{
		"claude-opus-4-8": proposeJSON("opus answer"),
		"gpt-5.5":         proposeJSON("codex answer"),
		"claude-sonnet-5": proposeJSON("synthesized best answer"),
	}}
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

	res, err := o.DispatchMulti("solve this", []string{"claude-opus-4-8", "gpt-5.5"}, "", "judge", "claude-sonnet-5")
	if err != nil {
		t.Fatalf("DispatchMulti error: %v", err)
	}
	if res.Synthesis == nil {
		t.Fatalf("synthesis = nil, want populated judge candidate")
	}
	if res.Synthesis.Result != "synthesized best answer" {
		t.Fatalf("synthesis result = %q, want %q", res.Synthesis.Result, "synthesized best answer")
	}
	if res.Synthesis.Agent != "claude" || res.Synthesis.Model != "claude-sonnet-5" {
		t.Fatalf("synthesis agent/model = %q/%q, want claude/claude-sonnet-5", res.Synthesis.Agent, res.Synthesis.Model)
	}
}

// TestDispatchMultiEmptyModels verifies an empty models list is a hard error.
func TestDispatchMultiEmptyModels(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		t.Fatalf("GetAdapter should not be called for empty models")
		return nil, nil
	}
	if _, err := o.DispatchMulti("solve this", nil, "", "", ""); err == nil {
		t.Fatalf("DispatchMulti(empty models) error = nil, want error")
	}
}

// TestDispatchMultiCandidateErrorIsBestEffort verifies a single candidate whose
// adapter run errors becomes a Status "error" candidate WITHOUT failing the whole
// call — the other candidate still returns its solution.
func TestDispatchMultiCandidateErrorIsBestEffort(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)

	ad := &modelScriptAdapter{
		byModel:   map[string]string{"claude-opus-4-8": proposeJSON("opus answer")},
		errModels: map[string]bool{"gpt-5.5": true},
	}
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) { return ad, nil }

	res, err := o.DispatchMulti("solve this", []string{"claude-opus-4-8", "gpt-5.5"}, "", "", "")
	if err != nil {
		t.Fatalf("DispatchMulti error = %v, want nil (candidate failure is best-effort)", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(res.Candidates))
	}
	if res.Candidates[0].Status != "ok" || res.Candidates[0].Result != "opus answer" {
		t.Fatalf("candidate[0] = %+v, want ok/opus answer", res.Candidates[0])
	}
	if res.Candidates[1].Status != "error" {
		t.Fatalf("candidate[1] status = %q, want error", res.Candidates[1].Status)
	}
	// The failed candidate still carries the model (and resolved agent) it was asked for.
	if res.Candidates[1].Model != "gpt-5.5" || res.Candidates[1].Agent != "codex" {
		t.Fatalf("candidate[1] model/agent = %q/%q, want gpt-5.5/codex", res.Candidates[1].Model, res.Candidates[1].Agent)
	}
}

// TestDispatchInsightRoundTrip verifies the cross-agent insight loop end to end:
// a first dispatch contributes its memory_updates NOTE (the durable-fact channel)
// to the insight layer, and a later dispatch whose task shares terminology gets
// that learning injected into the sub-agent's system prompt (curated recall),
// while an unrelated task does not. The dispatch SUMMARY is deliberately NOT
// contributed — only notes are.
func TestDispatchInsightRoundTrip(t *testing.T) {
	// A realistic response: a low-signal action summary plus a durable-fact note.
	// Only the note should become an insight.
	const noteCanned = `Done.

{"status":"ok","result":"did X","summary":"made some changes","memory_updates":[{"note":"the scheduler is guarded by a single global mutex"}]}`

	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(1)

	var argv []string
	o := New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return newRecordingAdapter(t, name, noteCanned, &argv), nil
	}

	// First dispatch (routed to claude) -> the NOTE is contributed as an insight;
	// the summary ("made some changes") is not.
	if _, err := o.Dispatch("refactor the concurrent scheduler race condition", "claude", ""); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	ins, err := mem.Insights()
	if err != nil {
		t.Fatalf("Insights: %v", err)
	}
	if len(ins) != 1 || !strings.Contains(ins[0].Text, "scheduler") {
		t.Fatalf("expected one note-derived 'scheduler' insight, got %v", ins)
	}
	if strings.Contains(ins[0].Text, "made some changes") {
		t.Fatalf("summary was contributed as an insight; only notes should be: %v", ins)
	}

	// Second dispatch on a task that shares the term "scheduler": the insight
	// brief must be injected into the system prompt handed to the sub-agent.
	argv = nil
	if _, err := o.Dispatch("add unit tests for the scheduler", "claude", ""); err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "CROSS-AGENT INSIGHTS") {
		t.Fatalf("second dispatch prompt missing insight brief header; argv=%v", argv)
	}
	if !strings.Contains(joined, "single global mutex") {
		t.Fatalf("second dispatch prompt missing the contributed note-insight; argv=%v", argv)
	}

	// Third dispatch on an unrelated task: no brief injected (no noise).
	argv = nil
	if _, err := o.Dispatch("update the copyright year in the license header", "claude", ""); err != nil {
		t.Fatalf("third Dispatch: %v", err)
	}
	if strings.Contains(strings.Join(argv, "\x00"), "CROSS-AGENT INSIGHTS") {
		t.Fatalf("unrelated dispatch wrongly injected an insight brief; argv=%v", argv)
	}
}

// newGateOrch builds an Orchestrator whose goal-met judge is a captureAdapter
// returning canned goal-check JSON, with a deterministic model-pinned route so
// the gate test does not depend on the real routing model table.
func newGateOrch(t *testing.T, canned string) (*Orchestrator, *captureAdapter) {
	t.Helper()
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	cap := &captureAdapter{name: "claude", canned: canned}
	o := New(mem, mgr)
	o.Route = func(task, agentName, priority, model string) (routing.Selection, error) {
		return routing.Selection{Agent: "claude", Model: model, Difficulty: "hard"}, nil
	}
	o.GetAdapter = func(name string) (agent.Adapter, error) { return cap, nil }
	return o, cap
}

// TestPlanGateCanStop covers the affirmative path: no steps remain, an empty
// verify list is trivially passed, and the judge confirms goal_met -> can_stop.
func TestPlanGateCanStop(t *testing.T) {
	o, cap := newGateOrch(t, `{"goal_met": true, "reason": "all done"}`)

	res, err := o.PlanGate("the goal", "the spec", nil, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("PlanGate error: %v", err)
	}
	if !res.Verify.Passed {
		t.Errorf("Verify.Passed = false, want true (empty verify list is trivially passed)")
	}
	if !res.GoalMet || res.GoalMetReason != "all done" {
		t.Errorf("goal-met = (%v, %q), want (true, \"all done\")", res.GoalMet, res.GoalMetReason)
	}
	if !res.CanStop {
		t.Errorf("CanStop = false, want true (no steps remain, verify passed, goal met)")
	}
	// The judge ran read-only: plan mode + write tools disallowed for claude.
	if !strings.Contains(strings.Join(cap.gotExtra, "\x00"), "plan") {
		t.Errorf("judge extras = %v, want read-only plan mode", cap.gotExtra)
	}
}

// TestPlanGateStepsRemainingBlocks proves CanStop is conservative on the step
// axis: even with verify passed and the goal judged met, remaining steps block a
// stop.
func TestPlanGateStepsRemainingBlocks(t *testing.T) {
	o, _ := newGateOrch(t, `{"goal_met": true, "reason": "done"}`)

	res, err := o.PlanGate("g", "", nil, 1, t.TempDir(), "")
	if err != nil {
		t.Fatalf("PlanGate error: %v", err)
	}
	if !res.GoalMet || !res.Verify.Passed {
		t.Fatalf("precondition: want goal met and verify passed, got goalMet=%v verify=%v", res.GoalMet, res.Verify.Passed)
	}
	if res.CanStop {
		t.Errorf("CanStop = true with 1 step remaining, want false")
	}
}

// TestPlanGateJudgeNotMet proves a not-met verdict blocks a stop and its reason
// is surfaced.
func TestPlanGateJudgeNotMet(t *testing.T) {
	o, _ := newGateOrch(t, `{"goal_met": false, "reason": "missing tests"}`)

	res, err := o.PlanGate("g", "", nil, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("PlanGate error: %v", err)
	}
	if res.GoalMet || res.CanStop {
		t.Errorf("goalMet=%v canStop=%v, want both false", res.GoalMet, res.CanStop)
	}
	if res.GoalMetReason != "missing tests" {
		t.Errorf("GoalMetReason = %q, want %q", res.GoalMetReason, "missing tests")
	}
}

// TestPlanGateJudgeUnavailableIsConservative proves the gate never claims the
// goal is met when the judge could not run (here GetAdapter fails): GoalMet is
// false with an explanatory reason, and CanStop is false.
func TestPlanGateJudgeUnavailableIsConservative(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr, _ := newFakeManager(0)
	o := New(mem, mgr)
	o.Route = func(task, agentName, priority, model string) (routing.Selection, error) {
		return routing.Selection{Agent: "claude", Model: model, Difficulty: "hard"}, nil
	}
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		return nil, fmt.Errorf("no adapter for %q", name)
	}

	res, err := o.PlanGate("g", "", nil, 0, t.TempDir(), "")
	if err != nil {
		t.Fatalf("PlanGate error: %v", err)
	}
	if res.GoalMet || res.CanStop {
		t.Errorf("goalMet=%v canStop=%v, want both false when the judge is unavailable", res.GoalMet, res.CanStop)
	}
	if !strings.Contains(res.GoalMetReason, "goal-met judge unavailable") {
		t.Errorf("GoalMetReason = %q, want it to explain the judge was unavailable", res.GoalMetReason)
	}
}

// TestPlanGateInvalidVerifyIsConfigError proves an unsafe/non-allowlisted verify
// command refuses the gate as a config error before running the judge.
func TestPlanGateInvalidVerifyIsConfigError(t *testing.T) {
	o, _ := newGateOrch(t, `{"goal_met": true, "reason": "done"}`)

	_, err := o.PlanGate("g", "", []string{"rm -rf /"}, 0, t.TempDir(), "")
	if err == nil {
		t.Fatalf("PlanGate error = nil, want a gate-config error for a non-allowlisted verify command")
	}
}
