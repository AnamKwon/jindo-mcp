package memory

import (
	"testing"
)

// TestAddInsightDedupReinforces: re-deriving the same learning (modulo case,
// punctuation, spacing) reinforces the existing insight — hits grow, salience
// rises, provenance of the first contributor is kept — rather than creating a
// duplicate.
func TestAddInsightDedupReinforces(t *testing.T) {
	m := New(t.TempDir())

	added, err := m.AddInsight("Build command is `make build`", "codex", "gpt-5.5", []string{"build"})
	if err != nil || !added {
		t.Fatalf("first AddInsight: added=%v err=%v (want true, nil)", added, err)
	}
	// Same learning, different casing/punctuation/agent -> reinforce, not add.
	added, err = m.AddInsight("build command is make build", "claude", "opus-4-8", []string{"make"})
	if err != nil {
		t.Fatalf("second AddInsight: %v", err)
	}
	if added {
		t.Fatalf("second AddInsight added a duplicate; want reinforcement of the existing insight")
	}

	got, err := m.Insights()
	if err != nil {
		t.Fatalf("Insights: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("insight count = %d, want 1 (dedup)", len(got))
	}
	in := got[0]
	if in.Hits != 2 {
		t.Fatalf("hits = %d, want 2 after one reinforcement", in.Hits)
	}
	if in.Salience <= baseSalience {
		t.Fatalf("salience = %v, want > base %v after reinforcement", in.Salience, baseSalience)
	}
	if in.Agent != "codex" || in.Model != "gpt-5.5" {
		t.Fatalf("provenance = %s/%s, want first contributor codex/gpt-5.5", in.Agent, in.Model)
	}
	// Tag from the reinforcing call should be merged in.
	var haveMake bool
	for _, tg := range in.Tags {
		if tg == "make" {
			haveMake = true
		}
	}
	if !haveMake {
		t.Fatalf("tags = %v, want merged tag 'make'", in.Tags)
	}
}

// TestAddInsightBlankIsNoop: a blank/whitespace text neither errors nor adds.
func TestAddInsightBlankIsNoop(t *testing.T) {
	m := New(t.TempDir())
	added, err := m.AddInsight("   ", "codex", "gpt-5.5", nil)
	if err != nil || added {
		t.Fatalf("blank AddInsight: added=%v err=%v (want false, nil)", added, err)
	}
	if got, _ := m.Insights(); len(got) != 0 {
		t.Fatalf("blank text created %d insights, want 0", len(got))
	}
}

// TestAddInsightWithInjectedNeverReinforces: an insight text that was INJECTED
// into the contributing agent's prompt and merely parroted back is not an
// independent rediscovery, so AddInsightWith must NOT reinforce a matching
// existing insight (hits/salience frozen), while a non-injected re-derivation
// still reinforces, and a brand-new parroted text is recorded but flagged
// DerivedFromInjected.
func TestAddInsightWithInjectedNeverReinforces(t *testing.T) {
	m := New(t.TempDir())

	const text = "auth lives in internal/authz"
	added, err := m.AddInsight(text, "codex", "gpt-5.5", nil)
	if err != nil || !added {
		t.Fatalf("seed AddInsight: added=%v err=%v (want true, nil)", added, err)
	}
	before, err := m.Insights()
	if err != nil || len(before) != 1 {
		t.Fatalf("Insights after seed: got %d err=%v (want 1)", len(before), err)
	}
	baseHits, baseSal := before[0].Hits, before[0].Salience

	// The same text was injected into this agent's prompt and echoed back:
	// must NOT reinforce.
	added, err = m.AddInsightWith(text, "claude", "opus-4-8", nil, []string{text})
	if err != nil {
		t.Fatalf("parroted AddInsightWith: %v", err)
	}
	if added {
		t.Fatalf("parroted AddInsightWith added a new insight; want no-op reinforcement")
	}
	got, _ := m.Insights()
	if len(got) != 1 {
		t.Fatalf("insight count = %d after parrot, want 1", len(got))
	}
	if got[0].Hits != baseHits || got[0].Salience != baseSal {
		t.Fatalf("parrot reinforced insight: hits %d->%d salience %v->%v (want frozen)",
			baseHits, got[0].Hits, baseSal, got[0].Salience)
	}

	// A genuine (non-injected) re-derivation still reinforces.
	added, err = m.AddInsightWith(text, "agy", "gemini", nil, []string{"some unrelated injected hint"})
	if err != nil {
		t.Fatalf("genuine AddInsightWith: %v", err)
	}
	if added {
		t.Fatalf("genuine re-derivation added a duplicate; want reinforcement")
	}
	got, _ = m.Insights()
	if got[0].Hits != baseHits+1 || got[0].Salience <= baseSal {
		t.Fatalf("genuine re-derivation did not reinforce: hits=%d salience=%v", got[0].Hits, got[0].Salience)
	}

	// A brand-new text that was itself injected is recorded but flagged.
	const fresh = "config parsing is in internal/config loader"
	added, err = m.AddInsightWith(fresh, "claude", "opus-4-8", nil, []string{fresh})
	if err != nil || !added {
		t.Fatalf("fresh parroted AddInsightWith: added=%v err=%v (want true, nil)", added, err)
	}
	got, _ = m.Insights()
	var freshIn *Insight
	for i := range got {
		if got[i].Text == fresh {
			freshIn = &got[i]
		}
	}
	if freshIn == nil {
		t.Fatalf("fresh parroted insight not recorded")
	}
	if !freshIn.DerivedFromInjected {
		t.Fatalf("fresh parroted insight DerivedFromInjected=false, want true")
	}
	if freshIn.Salience != baseSalience {
		t.Fatalf("fresh parroted insight salience=%v, want base %v", freshIn.Salience, baseSalience)
	}
}

// TestRetrieveInsightsRelevanceAndTopK: retrieval returns only insights sharing
// terms with the task, ranked, capped at k. An unrelated task returns nothing.
func TestRetrieveInsightsRelevanceAndTopK(t *testing.T) {
	m := New(t.TempDir())
	seed := []struct{ text, agent string }{
		{"authentication lives in internal/authz package", "claude"},
		{"rate limiting uses a token bucket in middleware", "codex"},
		{"build command is make build", "agy"},
		{"database migrations run via goose", "claude"},
	}
	for _, s := range seed {
		if _, err := m.AddInsight(s.text, s.agent, "m", nil); err != nil {
			t.Fatalf("seed AddInsight %q: %v", s.text, err)
		}
	}

	got, err := m.RetrieveInsights("add a rate limit to the authentication endpoint", 2)
	if err != nil {
		t.Fatalf("RetrieveInsights: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("retrieved %d insights, want top-2", len(got))
	}
	// The two relevant insights (auth, rate limiting) must be the ones returned;
	// build/migrations share no meaningful term with the task.
	for _, in := range got {
		if !containsAny(in.Text, "authentication", "rate") {
			t.Fatalf("retrieved irrelevant insight: %q", in.Text)
		}
	}

	// A task unrelated to every insight retrieves nothing (no noise).
	none, err := m.RetrieveInsights("write release notes for the changelog", 5)
	if err != nil {
		t.Fatalf("RetrieveInsights(unrelated): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unrelated task retrieved %d insights, want 0: %v", len(none), none)
	}

	// k<=0 is a no-op.
	if z, _ := m.RetrieveInsights("authentication", 0); len(z) != 0 {
		t.Fatalf("k=0 returned %d insights, want 0", len(z))
	}
}

// TestRetrieveInsightsSalienceBreaksTies: with equal term overlap, the more
// reinforced (higher-salience) insight ranks first.
func TestRetrieveInsightsSalienceBreaksTies(t *testing.T) {
	m := New(t.TempDir())
	// Two insights both matching "cache"; reinforce the second so it outranks.
	if _, err := m.AddInsight("cache layer uses an LRU eviction policy", "claude", "m", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddInsight("cache invalidation is handled by the writer", "codex", "m", nil); err != nil {
		t.Fatal(err)
	}
	// Reinforce the invalidation insight twice.
	for i := 0; i < 2; i++ {
		if _, err := m.AddInsight("cache invalidation is handled by the writer", "agy", "m", nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.RetrieveInsights("fix the cache", 2)
	if err != nil {
		t.Fatalf("RetrieveInsights: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 cache insights, got %d", len(got))
	}
	if !containsAny(got[0].Text, "invalidation") {
		t.Fatalf("top insight = %q, want the reinforced 'invalidation' one first", got[0].Text)
	}
}

// TestCompactCapsInsights: MaxInsights evicts the lowest-value insights while
// keeping the layer otherwise carried forward verbatim. A zero MaxInsights
// leaves the whole layer intact.
func TestCompactCapsInsights(t *testing.T) {
	m := New(t.TempDir())
	for i, txt := range []string{
		"alpha fact about widgets",
		"beta fact about gadgets",
		"gamma fact about gizmos",
	} {
		if _, err := m.AddInsight(txt, "claude", "m", nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Reinforce "beta" so it has the highest salience and survives the cap.
	if _, err := m.AddInsight("beta fact about gadgets", "codex", "m", nil); err != nil {
		t.Fatal(err)
	}

	// Cap disabled: layer carried forward whole.
	if _, err := m.Compact(CompactOptions{MaxInsights: 0}); err != nil {
		t.Fatalf("Compact(no cap): %v", err)
	}
	if got, _ := m.Insights(); len(got) != 3 {
		t.Fatalf("no-cap Compact changed insight count to %d, want 3", len(got))
	}

	// Cap to 1: only the highest-salience ("beta") survives.
	if _, err := m.Compact(CompactOptions{MaxInsights: 1}); err != nil {
		t.Fatalf("Compact(cap=1): %v", err)
	}
	got, _ := m.Insights()
	if len(got) != 1 {
		t.Fatalf("capped insight count = %d, want 1", len(got))
	}
	if !containsAny(got[0].Text, "beta") {
		t.Fatalf("survivor = %q, want the reinforced 'beta' insight", got[0].Text)
	}
}

// TestInsightsExcludedFromEntries: the _insights control key never leaks into
// All()/Stats()/Read(), so the insight layer is invisible to the flat task-entry
// surface (backward compatibility for callers iterating entries).
func TestInsightsExcludedFromEntries(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Write("task:codex:1", map[string]any{"result": "done"}, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddInsight("some durable learning about the system", "codex", "m", nil); err != nil {
		t.Fatal(err)
	}

	all, err := m.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, leaked := all[insightsKey]; leaked {
		t.Fatalf("All() leaked the %q control key", insightsKey)
	}
	if len(all) != 1 {
		t.Fatalf("All() returned %d entries, want 1 (the task entry only): %v", len(all), all)
	}
	if v, found := m.Read(insightsKey); found || v != nil {
		t.Fatalf("Read(%q) = (%v,%v), want (nil,false)", insightsKey, v, found)
	}
	records, _, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if records != 1 {
		t.Fatalf("Stats records = %d, want 1 (insights not counted)", records)
	}
}

// TestInsightsSurviveEntryCompaction: folding the cold tail of task ENTRIES into
// _digest must not disturb the insight layer.
func TestInsightsSurviveEntryCompaction(t *testing.T) {
	m := New(t.TempDir())
	for i := 0; i < 5; i++ {
		if err := m.Write(keyFor(i), map[string]any{"result": "r"}, "codex"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.AddInsight("a learning that must survive compaction", "codex", "m", nil); err != nil {
		t.Fatal(err)
	}
	// Cap entries to 2 (folds 3 into digest); leave insights uncapped.
	if _, err := m.Compact(CompactOptions{MaxEntries: 2}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got, _ := m.Insights()
	if len(got) != 1 {
		t.Fatalf("insight count after entry compaction = %d, want 1 (untouched)", len(got))
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is a tiny strings.Index shim to avoid importing strings just for tests.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func keyFor(i int) string {
	return "task:codex:" + string(rune('1'+i))
}
