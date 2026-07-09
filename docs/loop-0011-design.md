# loop-0011 design: wire MaybeCompact into Dispatch

Diagnosis/design only. No code changes in this task.

## 1. Call point

Call `mem.MaybeCompact(opts)` immediately **after** the authoritative result
`Upsert` (orchestrator.go:346-356) and **before** the final `return` at line
358. Rationale:

- The authoritative Upsert is the last write of *this* dispatch's own state.
  Compacting before it would risk MaybeCompact folding/capping the store while
  the current dispatch's own record is still the placeholder written by the
  pre-run Upsert (line 283), or racing with the fan-out loop (lines 317-339)
  that may still add entries. Running after guarantees compaction always sees
  the final state of this dispatch.
- Every dispatch is a natural, low-overhead point to self-bound store growth —
  no separate cron/background job is needed, and MaybeCompact's own cheap
  threshold check (compaction.go:209-251) means dispatches that are under the
  cap pay only one extra lock/load, not a full rewrite.

## 2. Interface change

`dispatchMem` (orchestrator.go:55-60) must gain:

```go
MaybeCompact(opts memory.CompactOptions) (bool, error)
```

`*memory.SharedMemory` already implements this (compaction.go:209), so no
change needed there. `spyMem` in orchestrator_test.go (lines 44-61) must gain
a matching delegating method:

```go
func (s *spyMem) MaybeCompact(opts memory.CompactOptions) (bool, error) {
    return s.inner.MaybeCompact(opts)
}
```

Without this, `spyMem` stops satisfying `dispatchMem` and every existing test
that constructs one fails to compile. This is a mechanical, additive change —
no other test scaffolding needs to move.

## 3. Default CompactOptions

Recommend **MaxEntries: 200, MaxNotes: 200**, matching `callCompact`'s
existing manual defaults (mcp.go:340-346) exactly, with `TTLSeconds: 0` /
`Now: 0` / `Summarize: nil` (same as the manual tool — no clock dependency,
deterministic).

Justification: the manual `compact` MCP tool and the automatic per-dispatch
hook are the same policy applied at two trigger points (manual pull vs.
automatic push); using the same threshold avoids two silently-diverging
notions of "too many entries" that an operator would have to reconcile. 200
real entries is also not obviously excessive context for a headless agent
that already receives the memory *directory* (not inline content, per the
`dispatchMem` leanness invariant) and reads it itself via STEP 1 of the system
prompt — the agent chooses how much of memory.json it actually loads, so 200
is a store-size cap, not a forced context injection. I found no evidence in
this codebase that 200 is too large for that read; if a future round shows
agents skimming or truncating memory.json under a real dispatch, lowering
this constant is a one-line follow-up, not a design change.

No existing regression test hardcodes an entry-count expectation against
`Dispatch`/`dispatchMem` (grep of `internal/orchestrator` found no
`MaxEntries`/`200` usage), and `internal/memory/memory_test.go`'s Compact/
MaybeCompact tests all pass their own small explicit `CompactOptions` per
test, so a 200 default introduced only at the `Dispatch` call site cannot
collide with them.

## 4. Failure semantics

Recommend **best-effort: log and ignore**, do NOT fail Dispatch.

Justification: Upsert/AppendNote failures are hard errors because they mean
the dispatch's own record — the thing callers and later agents rely on for
correctness — was not durably written. A MaybeCompact failure, by contrast,
only means the store didn't get smaller; the dispatch's own authoritative
record (written just before, per §1) is already safely persisted. Treating
compaction as a housekeeping optimization that must never block the primary
path is consistent with `MaybeCompact` itself being described as an opt-in,
caller-controlled convenience (compaction.go:206-208), not a correctness
requirement. Concretely: wrap the call so an error is swallowed (e.g. via a
note appended with `AppendNote("orchestrator", "compact failed: "+err)`, best
effort) but never returned from `Dispatch`.

## 5. BuildSystemPrompt digest gap

Yes, this is a real, if modest, blind spot. STEP 1 (agentproto.go:41-48) tells
the agent to "read shared memory" and describes `memory.json` and the `.jindo`
store as holding "prior agents' context: facts and notes," but never mentions
that once compaction has run, older context has been folded into a single
reserved `_digest` entry (compaction.go:9-15) rather than appearing as
individual live entries. An agent that lists/iterates entries expecting one
record per prior task could silently miss everything already folded, since
`_digest` looks like an ordinary entry only if the agent knows to look for it
by name.

Proposed addition to STEP 1 (after the existing "record facts and notes..."
sentence):

> "Some older context may already be folded into a single reserved entry
> named `_digest` rather than kept as individual entries — if present, treat
> its body as a compressed summary of historical facts and notes, not as
> noise to skip. Check for `_digest` explicitly; it will not appear if the
> store has never been compacted."

This is a targeted 2-3 sentence addition, not a rewrite of STEP 1's existing
wording.
