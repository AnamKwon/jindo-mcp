package memory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// digestKey is the reserved key under which cold-tail entries are folded during
// Compact. Like notesKey it is a control key, not a real user entry: All()
// excludes it (as it does _notes), while Read("_digest") still returns it
// verbatim if a caller asks explicitly. Compact never counts it as a "real"
// entry and never folds it into itself. Its value is a wrapper
// {"value": {"body","count","oldest_ts","newest_ts"}, "author", "ts"} so it looks
// like any other entry to load()/save() and to a human reading the store.
const digestKey = "_digest"

// splitIndexedKey splits "<prefix>:<n>" into (prefix, n, true); any other
// shape returns ("", 0, false). Unlike keyN it keeps the prefix, and it is not
// limited to "task:" keys, mirroring the Python _split_indexed_key so the two
// compactors apply the same per-prefix frontier rule to reservations.
func splitIndexedKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return key[:i], n, true
}

// CompactOptions configures a single Compact pass. All limits are opt-in: a zero
// MaxEntries/MaxNotes disables that cap, and a zero TTLSeconds (or zero Now)
// disables TTL dropping, so a zero-value CompactOptions is a safe no-op.
type CompactOptions struct {
	// MaxEntries caps the number of REAL entries (non-reserved, non-digest,
	// non-notes) kept live. Excess oldest entries are folded into _digest. 0
	// disables the cap.
	MaxEntries int
	// MaxNotes keeps only the last-N notes. 0 disables note trimming.
	MaxNotes int
	// TTLSeconds drops COMPLETED entries whose ts < Now-TTLSeconds. 0 disables
	// TTL dropping. Requires Now>0 to take effect (see Now).
	TTLSeconds int64
	// Now is the reference "current time" (unix seconds) for the TTL rule,
	// injected so compaction is deterministic and testable. If 0, TTL dropping
	// is skipped entirely (there is no defensible "now" to compare against, and
	// silently calling time.Now() would reintroduce nondeterminism).
	Now int64
	// Summarize, if non-nil, produces the _digest body from the deterministic
	// cold-tail text. On error Compact falls back to the deterministic text so a
	// flaky summarizer never blocks or loses data. If nil, the deterministic
	// cold-tail text is used directly as the body.
	Summarize func(coldTail string) (string, error)
}

// CompactResult reports what a Compact pass did. Counts are of REAL entries
// (the same population MaxEntries caps); Folded is how many real entries were
// moved into _digest this pass; Digested is true if a _digest entry exists in
// the written store (either freshly created/extended this pass, or carried over).
type CompactResult struct {
	EntriesBefore int
	EntriesAfter  int
	NotesBefore   int
	NotesAfter    int
	Folded        int
	Digested      bool
}

// realEntry is a REAL entry paired with its key and parsed ts, used only inside
// Compact for ordering and folding. ts is the wrapper's numeric timestamp; a
// missing/malformed ts sorts as 0 (oldest), so a corrupt entry is folded first
// rather than surviving indefinitely.
type realEntry struct {
	key string
	// wrapper is the full {"value","author","ts",...} map as loaded.
	wrapper map[string]any
	ts      float64
}

// Compact rewrites the store under the lock, applying the deterministic rules in
// CompactOptions and (optionally) an injected summarizer to fold a cold tail of
// old entries into a single _digest entry. It reuses the same acquire/load/save
// path as Write, so a concurrent Write is fully serialized before or after this
// pass — never interleaved — and the store is replaced atomically.
//
// Rules, applied IN ORDER to REAL entries (everything except _notes, _digest,
// and _reserved placeholders):
//
//  1. supersede-by-key: keys are unique (AllocKey guarantees it), so there is
//     nothing to merge; this step keeps the newest-ts entry per key as a guard
//     that is correct even if a duplicate key ever appears. With unique keys it
//     is an identity operation.
//  2. TTL drop: if TTLSeconds>0 and Now>0, drop entries that are COMPLETED
//     (inner value is a map with a non-null "result") AND ts < Now-TTLSeconds.
//  3. cap: if MaxEntries>0 and the survivors exceed it, sort by ts ascending
//     and fold the oldest (count-MaxEntries) — the COLD TAIL — into _digest,
//     keeping the newest MaxEntries live.
//  4. notes: if MaxNotes>0 and there are more, keep only the last MaxNotes.
//
// Superseded _reserved placeholders are dropped (a kept real key with an equal
// or higher index covers them, so key allocation scans past them either way).
// FRONTIER reservations — index above every kept real key — are kept: they may
// belong to an in-flight dispatch (Go AllocKey or Python reserve_task_key) that
// has reserved but not yet written, and dropping them would let the next
// allocation hand out the same key again.
func (m *SharedMemory) Compact(opts CompactOptions) (CompactResult, error) {
	if err := m.acquire(); err != nil {
		return CompactResult{}, err
	}
	defer m.release()

	data, err := m.load()
	if err != nil {
		return CompactResult{}, err
	}

	// --- separate control keys from REAL entries ---
	var notes []any
	if n, ok := data[notesKey].([]any); ok {
		notes = n
	}
	// Pre-existing digest wrapper (may be nil).
	priorDigest, _ := data[digestKey].(map[string]any)

	// supersede-by-key (rule 1): group by key, keep newest ts. Keys are already
	// unique so this is effectively identity, but the guard is correct if a
	// duplicate ever appears.
	byKey := make(map[string]realEntry)
	reserved := make(map[string]map[string]any)
	for k, entry := range data {
		if k == notesKey || k == digestKey {
			continue
		}
		wrapper, ok := entry.(map[string]any)
		if !ok {
			continue // malformed top-level entry: drop it (matches load() tolerance)
		}
		if r, ok := wrapper[reservedField].(bool); ok && r {
			// Set aside for the frontier check after folding (see below).
			reserved[k] = wrapper
			continue
		}
		ts := tsOf(wrapper)
		if prev, seen := byKey[k]; !seen || ts >= prev.ts {
			byKey[k] = realEntry{key: k, wrapper: wrapper, ts: ts}
		}
	}

	entriesBefore := len(byKey)
	notesBefore := len(notes)

	reals := make([]realEntry, 0, len(byKey))
	for _, e := range byKey {
		reals = append(reals, e)
	}

	// --- rule 2: TTL drop of COMPLETED, expired entries ---
	if opts.TTLSeconds > 0 && opts.Now > 0 {
		cutoff := float64(opts.Now - opts.TTLSeconds)
		kept := reals[:0]
		for _, e := range reals {
			if isCompleted(e.wrapper) && e.ts < cutoff {
				continue // drop
			}
			kept = append(kept, e)
		}
		reals = kept
	}

	// --- rule 3: cap by folding the cold tail into _digest ---
	var folded []realEntry
	if opts.MaxEntries > 0 && len(reals) > opts.MaxEntries {
		// Sort oldest-first. sort.SliceStable makes equal-ts ordering
		// deterministic (insertion-order is nondeterministic from a map, so we
		// break ties by key to get a stable, reproducible cold tail).
		sort.SliceStable(reals, func(i, j int) bool {
			if reals[i].ts != reals[j].ts {
				return reals[i].ts < reals[j].ts
			}
			return reals[i].key < reals[j].key
		})
		coldCount := len(reals) - opts.MaxEntries
		folded = reals[:coldCount]
		reals = reals[coldCount:]
	}

	// --- rebuild the store ---
	out := make(map[string]any, len(reals)+2)
	for _, e := range reals {
		out[e.key] = e.wrapper
	}

	// Keep frontier reservations: a placeholder whose index is above every
	// KEPT real key OF ITS PREFIX may be held by an in-flight dispatch that
	// has reserved but not yet written; dropping it would let the next
	// allocation hand out the same key again. The comparison is per-prefix
	// (not the global max) because the Python reserve_task_key scans only its
	// own "task:<n>" form — a "task:4" reservation is not covered by a
	// higher-indexed "task:alice:5". Lower-indexed reservations are
	// superseded within their prefix and stay dropped.
	maxRealN := make(map[string]int)
	for _, e := range reals {
		if prefix, n, ok := splitIndexedKey(e.key); ok && n > maxRealN[prefix] {
			maxRealN[prefix] = n
		}
	}
	for k, wrapper := range reserved {
		if prefix, n, ok := splitIndexedKey(k); ok && n > maxRealN[prefix] {
			out[k] = wrapper
		}
	}

	result := CompactResult{
		EntriesBefore: entriesBefore,
		EntriesAfter:  len(reals),
		NotesBefore:   notesBefore,
		Folded:        len(folded),
	}

	// Extend the digest if we folded anything; carry a pre-existing digest
	// forward VERBATIM otherwise (matching the Python side) so a no-fold pass
	// never rewrites wrapper fields or drops unrecognized digest metadata.
	if len(folded) > 0 {
		out[digestKey] = buildDigest(priorDigest, folded, opts)
		result.Digested = true
	} else if priorDigest != nil {
		out[digestKey] = priorDigest
		result.Digested = true
	}

	// --- rule 4: trim notes to last-N ---
	if opts.MaxNotes > 0 && len(notes) > opts.MaxNotes {
		notes = notes[len(notes)-opts.MaxNotes:]
	}
	if notes != nil {
		out[notesKey] = notes
	}
	result.NotesAfter = len(notes)

	if err := m.save(out); err != nil {
		return CompactResult{}, err
	}
	return result, nil
}

// MaybeCompact runs a cheap threshold check under a brief lock and only performs
// a full Compact when a limit is actually exceeded, returning whether it did.
// Callers invoke this AFTER a write when they want bounded growth; it is NOT
// auto-called from Write, so the write path stays a single lock acquisition and
// callers keep control over when the (heavier) rewrite happens.
func (m *SharedMemory) MaybeCompact(opts CompactOptions) (bool, error) {
	if err := m.acquire(); err != nil {
		return false, err
	}
	data, err := m.load()
	if err != nil {
		m.release()
		return false, err
	}

	realCount := 0
	notesCount := 0
	for k, entry := range data {
		switch k {
		case notesKey:
			if n, ok := entry.([]any); ok {
				notesCount = len(n)
			}
		case digestKey:
			// not a real entry
		default:
			wrapper, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if r, ok := wrapper[reservedField].(bool); ok && r {
				continue
			}
			realCount++
		}
	}
	m.release()

	over := (opts.MaxEntries > 0 && realCount > opts.MaxEntries) ||
		(opts.MaxNotes > 0 && notesCount > opts.MaxNotes)
	if !over {
		return false, nil
	}
	if _, err := m.Compact(opts); err != nil {
		return false, err
	}
	return true, nil
}

// tsOf extracts the numeric wrapper timestamp, or 0 if missing/malformed. A
// corrupt ts thus sorts oldest and is folded first rather than pinned live.
func tsOf(wrapper map[string]any) float64 {
	if ts, ok := wrapper["ts"].(float64); ok {
		return ts
	}
	return 0
}

// isCompleted reports whether an entry's inner value is a map carrying a
// non-null "result" — the marker used across the store for a finished task.
func isCompleted(wrapper map[string]any) bool {
	inner, ok := wrapper["value"].(map[string]any)
	if !ok {
		return false
	}
	result, present := inner["result"]
	return present && result != nil
}

// buildDigest folds the cold-tail entries into a single digest wrapper, merging
// with any pre-existing digest by summing the folded count, widening the ts
// span, and appending the new body under a separator. When Summarize is set it
// produces the fresh-fold body from the deterministic cold-tail text, falling
// back to that text on error so a summarizer failure never loses information.
func buildDigest(prior map[string]any, folded []realEntry, opts CompactOptions) map[string]any {
	// Carry prior digest metadata forward.
	priorCount := 0
	priorBody := ""
	var priorOldest, priorNewest float64
	priorHasSpan := false
	if prior != nil {
		if inner, ok := prior["value"].(map[string]any); ok {
			if c, ok := inner["count"].(float64); ok {
				priorCount = int(c)
			}
			if b, ok := inner["body"].(string); ok {
				priorBody = b
			}
			if o, ok := inner["oldest_ts"].(float64); ok {
				priorOldest = o
				priorHasSpan = true
			}
			if n, ok := inner["newest_ts"].(float64); ok {
				priorNewest = n
				priorHasSpan = true
			}
		}
	}

	coldTail := coldTailText(folded)

	// Fresh body for this fold; only compute a new body if we folded entries.
	newBody := ""
	if len(folded) > 0 {
		newBody = coldTail
		if opts.Summarize != nil {
			if s, err := opts.Summarize(coldTail); err == nil {
				newBody = s
			}
			// on err: keep the deterministic coldTail as newBody (documented fallback)
		}
	}

	// Merge bodies: prior first, then the new fold, separated so both survive.
	body := priorBody
	if newBody != "" {
		if body != "" {
			body += "\n---\n"
		}
		body += newBody
	}

	// Merge ts span across prior digest and this fold.
	oldest, newest := priorOldest, priorNewest
	hasSpan := priorHasSpan
	for _, e := range folded {
		if !hasSpan {
			oldest, newest = e.ts, e.ts
			hasSpan = true
			continue
		}
		if e.ts < oldest {
			oldest = e.ts
		}
		if e.ts > newest {
			newest = e.ts
		}
	}

	inner := map[string]any{
		"body":  body,
		"count": float64(priorCount + len(folded)),
	}
	if hasSpan {
		inner["oldest_ts"] = oldest
		inner["newest_ts"] = newest
	}

	return map[string]any{
		"value":  inner,
		"author": "_compaction",
		"ts":     newest, // wrapper ts tracks the newest folded entry
	}
}

// coldTailText renders the folded entries as a deterministic textual join, one
// line per entry "key :: author :: summary". summary is the inner "result" when
// the entry is completed, else a compact rendering of the value. Entries are
// ordered oldest-first (the caller sorts before folding) so the text is stable.
func coldTailText(folded []realEntry) string {
	var b strings.Builder
	for i, e := range folded {
		if i > 0 {
			b.WriteByte('\n')
		}
		author, _ := e.wrapper["author"].(string)
		b.WriteString(e.key)
		b.WriteString(" :: ")
		b.WriteString(author)
		b.WriteString(" :: ")
		b.WriteString(entrySummary(e.wrapper))
	}
	return b.String()
}

// entrySummary renders a single entry's payload for the digest: the inner
// "result" if the entry is completed, otherwise a compact string form of the
// inner value. Deterministic for stable digest text.
func entrySummary(wrapper map[string]any) string {
	if inner, ok := wrapper["value"].(map[string]any); ok {
		if result, present := inner["result"]; present && result != nil {
			return fmt.Sprintf("%v", result)
		}
	}
	return fmt.Sprintf("%v", wrapper["value"])
}
