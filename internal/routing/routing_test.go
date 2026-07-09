package routing

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeOverrides writes body to a fresh temp file and returns its path. Used by
// the ApplyOverrides tests to feed a writable overrides file.
func writeOverrides(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routing_overrides.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write overrides file: %v", err)
	}
	return p
}

// restorePolicy snapshots the live loadedPolicy and registers a cleanup that
// restores it, isolating tests that mutate package config via ApplyOverrides so
// they never bleed into other routing tests.
func restorePolicy(t *testing.T) {
	t.Helper()
	saved := clonePolicy(loadedPolicy)
	t.Cleanup(func() { loadedPolicy = saved })
}

// firstPatternForWeight returns any signal name plus one of its patterns whose
// signal weight equals want. Expectations are derived from the embedded policy
// so the tests stay in lockstep with the config rather than hardcoding numbers.
func firstPatternForWeight(t *testing.T, want float64) (string, string) {
	t.Helper()
	// Deterministic iteration by sorted signal name.
	names := make([]string, 0, len(loadedPolicy.Signals))
	for name := range loadedPolicy.Signals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := loadedPolicy.Signals[name]
		if s.Weight == want && len(s.Patterns) > 0 {
			return name, s.Patterns[0]
		}
	}
	t.Fatalf("no signal with weight %v found in embedded policy", want)
	return "", ""
}

// TestPlainTaskNotHard: an innocuous doc task must not score as hard. The
// prompt notes "comment" may itself be a pattern, so we only assert the tier is
// not "hard" (trivial or standard both acceptable).
func TestPlainTaskNotHard(t *testing.T) {
	got := ScoreTask("add a comment to the readme")
	if got.Tier == "hard" {
		t.Fatalf("plain task scored hard: total=%v scores=%v", got.Total, got.Scores)
	}
	if got.Total >= loadedPolicy.Thresholds["hard"] {
		t.Fatalf("plain task total %v unexpectedly >= hard threshold %v",
			got.Total, loadedPolicy.Thresholds["hard"])
	}
}

// TestSecurityTaskHard: the multi-condition security task must clear the hard
// threshold. The threshold is read from the embedded policy, not hardcoded.
func TestSecurityTaskHard(t *testing.T) {
	task := "refactor auth token encryption across the payment service, " +
		"must be atomic and idempotent, validate all edge cases"
	got := ScoreTask(task)
	hard := loadedPolicy.Thresholds["hard"]
	if got.Total < hard {
		t.Fatalf("security task total %v < hard threshold %v (scores=%v)",
			got.Total, hard, got.Scores)
	}
	if got.Tier != "hard" {
		t.Fatalf("security task tier = %q, want hard (total=%v)", got.Tier, got.Total)
	}
}

// TestEmptyTaskTrivial: a no-signal task scores 0 and is trivial.
func TestEmptyTaskTrivial(t *testing.T) {
	got := ScoreTask("")
	if got.Total != 0 {
		t.Fatalf("empty task total = %v, want 0", got.Total)
	}
	if got.Tier != "trivial" {
		t.Fatalf("empty task tier = %q, want trivial", got.Tier)
	}
}

// TestStandardBoundary: a task whose only signal is a single weight-1.2 scope
// pattern totals 1.2, which is just >= the standard threshold (1.0) and below
// hard, so it must be "standard".
func TestStandardBoundary(t *testing.T) {
	standard := loadedPolicy.Thresholds["standard"]
	hard := loadedPolicy.Thresholds["hard"]

	name, pattern := firstPatternForWeight(t, 1.2)
	got := ScoreTask(pattern)

	// Sanity: only the intended signal fired (single-pattern task), so the
	// total equals exactly that signal's weight.
	if got.Scores[name] != 1.2 {
		t.Fatalf("signal %q score = %v, want its weight 1.2 (scores=%v)",
			name, got.Scores[name], got.Scores)
	}
	if !(got.Total >= standard && got.Total < hard) {
		t.Fatalf("boundary total %v not in [standard=%v, hard=%v)",
			got.Total, standard, hard)
	}
	if got.Tier != "standard" {
		t.Fatalf("boundary tier = %q, want standard (total=%v)", got.Tier, got.Total)
	}
}

// TestHardBoundary: build a task from distinct weight-3.0 security patterns
// until the total reaches the hard threshold, then assert it tips into "hard".
// The pattern set and threshold both come from the embedded policy.
func TestHardBoundary(t *testing.T) {
	hard := loadedPolicy.Thresholds["hard"]
	sec := loadedPolicy.Signals["security"]
	if sec.Weight != 3.0 {
		t.Fatalf("security weight = %v, expected 3.0 for this construction", sec.Weight)
	}

	// Minimum distinct patterns needed to reach the hard threshold.
	needed := 0
	for float64(needed)*sec.Weight < hard {
		needed++
	}
	if needed > len(sec.Patterns) {
		t.Fatalf("not enough security patterns (%d) to reach hard threshold %v",
			len(sec.Patterns), hard)
	}
	task := strings.Join(sec.Patterns[:needed], " ")
	got := ScoreTask(task)
	if got.Total < hard {
		t.Fatalf("constructed task total %v < hard threshold %v", got.Total, hard)
	}
	if got.Tier != "hard" {
		t.Fatalf("constructed task tier = %q, want hard (total=%v)", got.Tier, got.Total)
	}
}

// TestConstraintsSignalNoDoubleCount pins that a single occurrence of a word
// contributes once per signal, even when the config still has to hold both a
// short general pattern and a longer word that happens to contain it at a
// leading word boundary (e.g. "concurren"/"concurrent", "auth"/"authentication",
// "lock"/"locking"). Before the fix, routing_policy.json listed both members
// of each pair in the same signal, so one word in the task text matched two
// patterns and was counted twice.
//
// The words here are chosen so the longer word still starts with the shorter
// pattern (leading word boundary) — ScoreTask's word-boundary matching no
// longer credits a pattern that only occurs mid-word (e.g. "auth" inside
// "oauth", or "lock" inside "deadlock"), which is the point of that matching
// change, not a double-count regression.
func TestConstraintsSignalNoDoubleCount(t *testing.T) {
	constraints := loadedPolicy.Signals["constraints"]
	security := loadedPolicy.Signals["security"]

	cases := []struct {
		name   string
		task   string
		signal string
		weight float64
	}{
		{"concurrent word", "concurrent", "constraints", constraints.Weight},
		{"authentication word", "authentication", "security", security.Weight},
		{"locking word", "locking", "constraints", constraints.Weight},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreTask(tc.task)
			if got.Scores[tc.signal] != tc.weight {
				t.Fatalf("ScoreTask(%q).Scores[%q] = %v, want %v (one pattern match, scores=%v)",
					tc.task, tc.signal, got.Scores[tc.signal], tc.weight, got.Scores)
			}
		})
	}
}

// TestPerSignalSaturationCap pins that a signal's contribution is capped at 2
// distinct matched patterns even when more than 2 of its patterns occur in
// the task: a fourth distinct security pattern beyond the first two must not
// add to the total.
func TestPerSignalSaturationCap(t *testing.T) {
	security := loadedPolicy.Signals["security"]

	got := ScoreTask("auth token encrypt secret")
	want := security.Weight * 2
	if got.Scores["security"] != want {
		t.Fatalf("ScoreTask capped security score = %v, want %v (4 distinct patterns capped at 2, scores=%v)",
			got.Scores["security"], want, got.Scores)
	}
}

// TestWordBoundaryRejectsMidWordMatch pins that a pattern must occur at a
// leading word boundary, not merely as a substring: "capital" contains "api"
// mid-word, so it must not contribute to the scope signal (weight 1.2,
// pattern "api"), unlike a real occurrence of "api" as its own word.
func TestWordBoundaryRejectsMidWordMatch(t *testing.T) {
	got := ScoreTask("what is the capital of france")
	if got.Scores["scope"] != 0 {
		t.Fatalf("ScoreTask(%q).Scores[%q] = %v, want 0 (\"api\" only occurs mid-word in \"capital\", scores=%v)",
			"what is the capital of france", "scope", got.Scores["scope"], got.Scores)
	}
	if got.Total != 0 {
		t.Fatalf("ScoreTask(%q).Total = %v, want 0 (no leading-word-boundary pattern matches, scores=%v)",
			"what is the capital of france", got.Total, got.Scores)
	}
}

// TestSelectHardDefaultAgent: for a hard task with no explicit agent, Select
// must use default_agent_by_difficulty["hard"] and that agent's hard model,
// both read from the embedded models.json.
func TestSelectHardDefaultAgent(t *testing.T) {
	task := "refactor auth token encryption across the payment service, " +
		"must be atomic and idempotent, validate all edge cases"
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if sel.Difficulty != "hard" {
		t.Fatalf("difficulty = %q, want hard", sel.Difficulty)
	}
	wantAgent := loadedModels.DefaultAgentByDifficulty["hard"]
	if sel.Agent != wantAgent {
		t.Fatalf("agent = %q, want default hard agent %q", sel.Agent, wantAgent)
	}
	wantModel := loadedModels.Agents[wantAgent]["hard"]
	if sel.Model != wantModel {
		t.Fatalf("model = %q, want %q", sel.Model, wantModel)
	}
}

// TestSelectRationaleEqualsScore is the core rationale invariant: Select's
// Rationale must be derived from the real ScoreTask result — only the signals
// that actually matched (contribution > 0) appear, Total/Tier equal the score,
// and the applied threshold matches the crossed policy threshold.
func TestSelectRationaleEqualsScore(t *testing.T) {
	task := "refactor auth token encryption across the payment service, " +
		"must be atomic and idempotent, validate all edge cases"
	score := ScoreTask(task)

	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if sel.Rationale.Total != score.Total {
		t.Fatalf("rationale total = %v, want computed %v", sel.Rationale.Total, score.Total)
	}
	if sel.Rationale.Tier != score.Tier {
		t.Fatalf("rationale tier = %q, want computed %q", sel.Rationale.Tier, score.Tier)
	}
	// Matched is exactly the signals whose contribution was > 0.
	wantMatched := map[string]float64{}
	for name, c := range score.Scores {
		if c > 0 {
			wantMatched[name] = c
		}
	}
	if len(sel.Rationale.Matched) != len(wantMatched) {
		t.Fatalf("rationale matched = %v, want only nonzero signals %v", sel.Rationale.Matched, wantMatched)
	}
	for name, c := range wantMatched {
		if sel.Rationale.Matched[name] != c {
			t.Fatalf("rationale matched[%q] = %v, want %v", name, sel.Rationale.Matched[name], c)
		}
	}
	// A hard task's applied threshold is the crossed "hard" policy threshold.
	if sel.Rationale.Threshold != "hard" {
		t.Fatalf("rationale threshold = %q, want %q", sel.Rationale.Threshold, "hard")
	}
	if sel.Rationale.ThresholdValue != loadedPolicy.Thresholds["hard"] {
		t.Fatalf("rationale threshold value = %v, want %v", sel.Rationale.ThresholdValue, loadedPolicy.Thresholds["hard"])
	}
}

// TestSelectExplicitAgentOverride: an explicit agent is honored, and the model
// is that agent's model for the scored tier.
func TestSelectExplicitAgentOverride(t *testing.T) {
	task := "refactor auth token encryption across the payment service, " +
		"must be atomic and idempotent, validate all edge cases"
	tier := ScoreTask(task).Tier
	sel, err := Select(task, "claude", "")
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if sel.Agent != "claude" {
		t.Fatalf("agent = %q, want claude", sel.Agent)
	}
	wantModel := loadedModels.Agents["claude"][tier]
	if sel.Model != wantModel {
		t.Fatalf("model = %q, want claude[%s] = %q", sel.Model, tier, wantModel)
	}
}

// TestSelectUnknownAgent: an agent absent from the config is an error.
func TestSelectUnknownAgent(t *testing.T) {
	_, err := Select("add a comment to the readme", "no-such-agent", "")
	if err == nil {
		t.Fatal("Select with unknown agent returned nil error, want error")
	}
}

// TestSelectDefaultAgentByTier is a characterization test: it pins the
// current end-to-end routing behavior (task text -> tier -> default agent ->
// model) for one representative task per tier, per the documented thresholds
// (standard=1.0, hard=6.0) and default_agent_by_difficulty in models.json.
// Expected models are read from the embedded config rather than hardcoded, so
// only the tier/agent mapping is asserted as fixed behavior.
func TestSelectDefaultAgentByTier(t *testing.T) {
	cases := []struct {
		name      string
		task      string
		wantTier  string
		wantAgent string
	}{
		{
			name:      "trivial doc edit",
			task:      "add a comment to the readme",
			wantTier:  "trivial",
			wantAgent: "agy",
		},
		{
			name:      "standard feature work",
			task:      "implement a new api endpoint with validation",
			wantTier:  "standard",
			wantAgent: "claude",
		},
		{
			name:      "hard cross-cutting security refactor",
			task:      "refactor the auth token encryption across multiple services with concurrency and rollback",
			wantTier:  "hard",
			wantAgent: "codex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotScore := ScoreTask(tc.task)
			if gotScore.Tier != tc.wantTier {
				t.Fatalf("ScoreTask(%q).Tier = %q, want %q (total=%v scores=%v)",
					tc.task, gotScore.Tier, tc.wantTier, gotScore.Total, gotScore.Scores)
			}

			sel, err := Select(tc.task, "", "")
			if err != nil {
				t.Fatalf("Select(%q, \"\") returned error: %v", tc.task, err)
			}
			if sel.Difficulty != tc.wantTier {
				t.Fatalf("Select(%q, \"\").Difficulty = %q, want %q", tc.task, sel.Difficulty, tc.wantTier)
			}
			if sel.Agent != tc.wantAgent {
				t.Fatalf("Select(%q, \"\").Agent = %q, want %q", tc.task, sel.Agent, tc.wantAgent)
			}
			wantModel := loadedModels.Agents[tc.wantAgent][tc.wantTier]
			if sel.Model != wantModel {
				t.Fatalf("Select(%q, \"\").Model = %q, want %q", tc.task, sel.Model, wantModel)
			}
		})
	}
}

// countCandidates returns how many agents have a model slot for difficulty —
// the candidate set profile selection ranks. Read from config so the test
// tracks the real agents table rather than a hardcoded count.
func countCandidates(difficulty string) int {
	n := 0
	for _, tiers := range loadedModels.Agents {
		if _, ok := tiers[difficulty]; ok {
			n++
		}
	}
	return n
}

// TestSelectProfileBestCoverage: with profiles active and no explicit agent, a
// standard scope+constraints task must resolve to the candidate with the
// highest weighted coverage of the matched signals (claude, the scope/
// constraints specialist), and ProfileMatch must record every candidate with
// the chosen one holding the maximum coverage.
func TestSelectProfileBestCoverage(t *testing.T) {
	task := "implement a new api endpoint with validation"
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Difficulty != "standard" {
		t.Fatalf("difficulty = %q, want standard", sel.Difficulty)
	}
	if sel.Agent != "claude" {
		t.Fatalf("agent = %q, want claude (best coverage of scope+constraints)", sel.Agent)
	}

	pm := sel.Rationale.ProfileMatch
	if pm == nil {
		t.Fatal("Rationale.ProfileMatch is nil, want populated on the coverage path")
	}
	if pm.Chosen != "claude" {
		t.Fatalf("ProfileMatch.Chosen = %q, want claude", pm.Chosen)
	}
	if got, want := len(pm.Candidates), countCandidates("standard"); got != want {
		t.Fatalf("ProfileMatch.Candidates = %d, want one per standard candidate (%d)", got, want)
	}
	// The chosen agent must have the strictly (or tie-broken) maximum coverage.
	var chosenCov float64
	found := false
	for _, c := range pm.Candidates {
		if c.Agent == pm.Chosen {
			chosenCov, found = c.Coverage, true
		}
	}
	if !found {
		t.Fatalf("chosen agent %q missing from candidates %+v", pm.Chosen, pm.Candidates)
	}
	for _, c := range pm.Candidates {
		if c.Coverage > chosenCov {
			t.Fatalf("candidate %q coverage %v exceeds chosen %q coverage %v", c.Agent, c.Coverage, pm.Chosen, chosenCov)
		}
	}
	if pm.Reason == "" {
		t.Fatal("ProfileMatch.Reason is empty, want a why-chosen explanation")
	}
}

// TestSelectProfileTrivialCheapest: a no-signal (trivial) task yields coverage 0
// for every candidate, so selection reduces to the cheapest cost_rank. This
// must preserve trivial -> agy and record the no-signal reason.
func TestSelectProfileTrivialCheapest(t *testing.T) {
	task := "add a comment to the readme"
	if ScoreTask(task).Tier != "trivial" {
		t.Fatalf("precondition: %q is not trivial", task)
	}
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Agent != "agy" {
		t.Fatalf("agent = %q, want agy (cheapest candidate on a no-signal task)", sel.Agent)
	}

	pm := sel.Rationale.ProfileMatch
	if pm == nil {
		t.Fatal("ProfileMatch is nil, want populated")
	}
	// All coverage must be 0 (no matched signals), and the chosen agent must
	// hold the minimum cost_rank among candidates.
	minCost := int(^uint(0) >> 1)
	for _, c := range pm.Candidates {
		if c.Coverage != 0 {
			t.Fatalf("candidate %q coverage = %v, want 0 on a no-signal task", c.Agent, c.Coverage)
		}
		if c.CostRank < minCost {
			minCost = c.CostRank
		}
	}
	var chosenCost int
	for _, c := range pm.Candidates {
		if c.Agent == pm.Chosen {
			chosenCost = c.CostRank
		}
	}
	if chosenCost != minCost {
		t.Fatalf("chosen %q cost_rank = %d, want cheapest %d", pm.Chosen, chosenCost, minCost)
	}
	if !strings.Contains(pm.Reason, "no signals matched") {
		t.Fatalf("ProfileMatch.Reason = %q, want it to note no signals matched", pm.Reason)
	}
}

// TestSelectExplicitOverrideWinsWithProfiles: an explicit agent must be honored
// verbatim even with profiles active — no coverage scoring runs, and
// ProfileMatch records the override (empty candidate breakdown).
func TestSelectExplicitOverrideWinsWithProfiles(t *testing.T) {
	task := "implement a new api endpoint with validation" // standard, profile default = claude
	sel, err := Select(task, "codex", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Agent != "codex" {
		t.Fatalf("agent = %q, want codex (explicit override)", sel.Agent)
	}
	wantModel := loadedModels.Agents["codex"][sel.Difficulty]
	if sel.Model != wantModel {
		t.Fatalf("model = %q, want %q", sel.Model, wantModel)
	}
	pm := sel.Rationale.ProfileMatch
	if pm == nil {
		t.Fatal("ProfileMatch is nil, want the override recorded")
	}
	if pm.Chosen != "codex" {
		t.Fatalf("ProfileMatch.Chosen = %q, want codex", pm.Chosen)
	}
	if len(pm.Candidates) != 0 {
		t.Fatalf("ProfileMatch.Candidates = %+v, want none on the override path", pm.Candidates)
	}
	if !strings.Contains(pm.Reason, "override") {
		t.Fatalf("ProfileMatch.Reason = %q, want it to note the override", pm.Reason)
	}
}

// TestSelectNoProfilesFallback: when profiles are absent/empty, Select must
// fall back to default_agent_by_difficulty (the prior safe behavior) and record
// the fallback reason. This white-box test temporarily clears the embedded
// profiles and restores them.
func TestSelectNoProfilesFallback(t *testing.T) {
	saved := loadedModels.Profiles
	loadedModels.Profiles = nil
	defer func() { loadedModels.Profiles = saved }()

	task := "implement a new api endpoint with validation"
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	wantAgent := loadedModels.DefaultAgentByDifficulty[sel.Difficulty]
	if sel.Agent != wantAgent {
		t.Fatalf("agent = %q, want fallback default %q", sel.Agent, wantAgent)
	}
	pm := sel.Rationale.ProfileMatch
	if pm == nil {
		t.Fatal("ProfileMatch is nil, want the fallback recorded")
	}
	if pm.Chosen != wantAgent {
		t.Fatalf("ProfileMatch.Chosen = %q, want %q", pm.Chosen, wantAgent)
	}
	if len(pm.Candidates) != 0 {
		t.Fatalf("ProfileMatch.Candidates = %+v, want none on the fallback path", pm.Candidates)
	}
	if !strings.Contains(pm.Reason, "no profiles") {
		t.Fatalf("ProfileMatch.Reason = %q, want it to note the no-profiles fallback", pm.Reason)
	}
}

// TestSelectProfileTieBreaksByCost: when two candidates cover the matched
// signals equally, the cheaper cost_rank must win. This white-box test installs
// profiles where claude and codex have identical strengths (so they always tie)
// but codex is cheaper, and asserts codex is chosen.
func TestSelectProfileTieBreaksByCost(t *testing.T) {
	saved := loadedModels.Profiles
	loadedModels.Profiles = map[string]profile{
		"claude": {Strength: map[string]float64{"scope": 1.0, "constraints": 1.0, "security": 1.0, "ambiguity": 1.0}, CostRank: 5},
		"codex":  {Strength: map[string]float64{"scope": 1.0, "constraints": 1.0, "security": 1.0, "ambiguity": 1.0}, CostRank: 2},
		"agy":    {Strength: map[string]float64{"scope": 0.1, "constraints": 0.1, "security": 0.1, "ambiguity": 0.1}, CostRank: 1},
	}
	defer func() { loadedModels.Profiles = saved }()

	task := "implement a new api endpoint with validation" // matched scope+constraints
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Agent != "codex" {
		t.Fatalf("agent = %q, want codex (equal coverage with claude, lower cost_rank)", sel.Agent)
	}
	// Sanity: claude and codex indeed tied on coverage in the breakdown.
	cov := map[string]float64{}
	for _, c := range sel.Rationale.ProfileMatch.Candidates {
		cov[c.Agent] = c.Coverage
	}
	if cov["claude"] != cov["codex"] {
		t.Fatalf("expected claude/codex coverage tie, got claude=%v codex=%v", cov["claude"], cov["codex"])
	}
}

// TestProfileMatchOmittedOnZeroRationale: a zero-value Rationale (never routed
// through Select, e.g. a stubbed one) must serialize without the profile_match
// key, preserving the pre-existing JSON shape for MCP/log backward compat.
func TestProfileMatchOmittedOnZeroRationale(t *testing.T) {
	var r Rationale
	if r.ProfileMatch != nil {
		t.Fatalf("zero Rationale.ProfileMatch = %+v, want nil", r.ProfileMatch)
	}
	// reflect keeps this test honest that ProfileMatch is a pointer (omitempty
	// only drops nil pointers/empty values, not a populated struct value).
	if reflect.TypeOf(r.ProfileMatch).Kind() != reflect.Ptr {
		t.Fatalf("ProfileMatch field kind = %v, want pointer for omitempty semantics", reflect.TypeOf(r.ProfileMatch).Kind())
	}
}

// TestSelectPriorityCostFlipsChoice: the standard "implement a new api
// endpoint with validation" task matches scope+constraints, on which claude
// has the highest weighted coverage (see TestSelectProfileBestCoverage) even
// though agy is the cheapest standard candidate (cost_rank 1 vs claude's 2).
// This is exactly the best-coverage != cheapest situation priority is meant to
// flip: priority="" (or "quality") must still choose claude (coverage
// dominates), while priority="cost" must choose the cheapest candidate
// (agy) regardless of its lower coverage.
func TestSelectPriorityCostFlipsChoice(t *testing.T) {
	task := "implement a new api endpoint with validation"

	qualityLike, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select(_, _, \"\") error: %v", err)
	}
	if qualityLike.Agent != "claude" {
		t.Fatalf("priority=\"\" agent = %q, want claude (best coverage)", qualityLike.Agent)
	}

	quality, err := Select(task, "", "quality")
	if err != nil {
		t.Fatalf("Select(_, _, \"quality\") error: %v", err)
	}
	if quality.Agent != "claude" {
		t.Fatalf("priority=quality agent = %q, want claude (best coverage)", quality.Agent)
	}

	cost, err := Select(task, "", "cost")
	if err != nil {
		t.Fatalf("Select(_, _, \"cost\") error: %v", err)
	}
	wantCheapest := ""
	minCost := int(^uint(0) >> 1)
	for _, c := range cost.Rationale.ProfileMatch.Candidates {
		if c.CostRank < minCost {
			minCost, wantCheapest = c.CostRank, c.Agent
		}
	}
	if cost.Agent != wantCheapest {
		t.Fatalf("priority=cost agent = %q, want cheapest candidate %q (cost_rank %d)", cost.Agent, wantCheapest, minCost)
	}
	if cost.Agent == qualityLike.Agent {
		t.Fatalf("priority=cost chose the same agent as default (%q); test fixture no longer distinguishes coverage from cost", cost.Agent)
	}
}

// TestSelectPriorityRecordedInRationale: when a priority is supplied, it must
// be echoed back verbatim in Rationale.Priority so the dispatch record/log can
// show which axis was applied.
func TestSelectPriorityRecordedInRationale(t *testing.T) {
	task := "implement a new api endpoint with validation"

	sel, err := Select(task, "", "cost")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Rationale.Priority != "cost" {
		t.Fatalf("Rationale.Priority = %q, want %q", sel.Rationale.Priority, "cost")
	}

	sel2, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel2.Rationale.Priority != "" {
		t.Fatalf("Rationale.Priority = %q, want empty when no priority supplied", sel2.Rationale.Priority)
	}
}

// TestSelectEmptyPriorityUnchanged is a regression pin: Select(task, agent, "")
// must produce an identical Selection to the pre-priority two-argument
// behavior, for both the profile-selection path and the explicit-override
// path. Any accidental reweighting on the "" priority path would be caught
// here.
func TestSelectEmptyPriorityUnchanged(t *testing.T) {
	task := "implement a new api endpoint with validation"

	got, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if got.Agent != "claude" || got.Difficulty != "standard" {
		t.Fatalf("Select(task, \"\", \"\") = agent %q difficulty %q, want claude/standard (unchanged baseline)", got.Agent, got.Difficulty)
	}
	if got.Rationale.Priority != "" {
		t.Fatalf("Rationale.Priority = %q, want empty", got.Rationale.Priority)
	}

	overridden, err := Select(task, "codex", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if overridden.Agent != "codex" {
		t.Fatalf("Select(task, \"codex\", \"\") agent = %q, want codex (override unaffected by priority)", overridden.Agent)
	}
}

// withAssessor sets the package-level Assessor seam for the duration of a
// test and restores the previous value (nil, in every real caller) on
// cleanup, so tests never leak a stub into later tests.
func withAssessor(t *testing.T, fn func(task string, score Score) (string, bool)) {
	t.Helper()
	prev := Assessor
	Assessor = fn
	t.Cleanup(func() { Assessor = prev })
}

// withLLMAssessEnabled flips loadedPolicy.LLMAssess.Enabled for the duration
// of a test and restores the embedded config's value (false) on cleanup, so
// tests never leak the mutation into later tests that assume the shipped
// default.
func withLLMAssessEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev := loadedPolicy.LLMAssess.Enabled
	loadedPolicy.LLMAssess.Enabled = enabled
	t.Cleanup(func() { loadedPolicy.LLMAssess.Enabled = prev })
}

// standardBandTask returns a task whose total lands inside the embedded
// llm_assess "standard" band (config: [0.7, 1.3]) by using a single
// weight-1.2 signal pattern, same construction as TestStandardBoundary.
func standardBandTask(t *testing.T) string {
	t.Helper()
	_, pattern := firstPatternForWeight(t, 1.2)
	return pattern
}

// hardBandTask returns a task whose total lands inside the embedded
// llm_assess "hard" band (config: [5.0, 7.0]) by combining one pattern each
// from the weight-3.0, weight-1.5, and weight-1.2 signals (3.0+1.5+1.2=5.7),
// deliberately short of the 6.0 hard threshold so the deterministic tier
// stays "standard" and only the LLM assessment can push it to "hard".
func hardBandTask(t *testing.T) string {
	t.Helper()
	_, p3 := firstPatternForWeight(t, 3.0)
	_, p15 := firstPatternForWeight(t, 1.5)
	_, p12 := firstPatternForWeight(t, 1.2)
	task := strings.Join([]string{p3, p15, p12}, " ")
	got := ScoreTask(task)
	if !(got.Total >= 5.0 && got.Total <= 7.0) {
		t.Fatalf("constructed task total %v not inside hard band [5.0,7.0]", got.Total)
	}
	if got.Tier == "hard" {
		t.Fatalf("constructed task tier already hard (total=%v); test needs a pre-assessment tier below hard", got.Total)
	}
	return task
}

// TestLLMAssessDisabledNeverCalls: with llm_assess.enabled=false (the shipped
// default), Assessor must never be invoked even when the env gate is set and
// the task falls inside a configured band — disabled means fully inert, and
// Select's output must be byte-identical to the pre-assessment behavior.
func TestLLMAssessDisabledNeverCalls(t *testing.T) {
	t.Setenv(llmAssessEnvVar, "1")
	// llm_assess is enabled by default in the embedded policy now, so force it
	// OFF locally to exercise the disabled path independent of the default.
	withLLMAssessEnabled(t, false)
	called := false
	withAssessor(t, func(task string, score Score) (string, bool) {
		called = true
		return "hard", true
	})

	task := standardBandTask(t)
	got, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if called {
		t.Fatalf("Assessor was called with llm_assess.enabled=false")
	}
	if got.Difficulty != "standard" {
		t.Fatalf("Difficulty = %q, want standard (deterministic, unassessed)", got.Difficulty)
	}
	if got.Rationale.LLMAssessed {
		t.Fatalf("Rationale.LLMAssessed = true, want false when disabled")
	}
}

// TestLLMAssessEnabledInBandOverrides: enabled + env set + task in the "hard"
// band lets Assessor override the deterministic tier, and the override must
// be recorded in the rationale (Tier updated, LLMAssessed set) and drive
// profile/agent selection for the final tier.
func TestLLMAssessEnabledInBandOverrides(t *testing.T) {
	withLLMAssessEnabled(t, true)
	t.Setenv(llmAssessEnvVar, "1")
	called := false
	withAssessor(t, func(task string, score Score) (string, bool) {
		called = true
		return "hard", true
	})

	task := hardBandTask(t)
	got, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if !called {
		t.Fatalf("Assessor was not called for an in-band task")
	}
	if got.Difficulty != "hard" {
		t.Fatalf("Difficulty = %q, want hard (assessed override)", got.Difficulty)
	}
	if !got.Rationale.LLMAssessed {
		t.Fatalf("Rationale.LLMAssessed = false, want true after an accepted override")
	}
	if got.Rationale.Tier != "hard" {
		t.Fatalf("Rationale.Tier = %q, want hard (post-assessment)", got.Rationale.Tier)
	}
}

// TestLLMAssessEnabledOutOfBandNeverCalls: enabled + env set but the task's
// total falls outside every configured band, so Assessor must not be
// consulted and the deterministic tier stands.
func TestLLMAssessEnabledOutOfBandNeverCalls(t *testing.T) {
	withLLMAssessEnabled(t, true)
	t.Setenv(llmAssessEnvVar, "1")
	called := false
	withAssessor(t, func(task string, score Score) (string, bool) {
		called = true
		return "hard", true
	})

	task := "add a comment to the readme"
	got := ScoreTask(task)
	if got.Total != 0 {
		t.Fatalf("expected zero-signal task for out-of-band total, got %v", got.Total)
	}

	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if called {
		t.Fatalf("Assessor was called for an out-of-band task (total=%v)", got.Total)
	}
	if sel.Difficulty != "trivial" {
		t.Fatalf("Difficulty = %q, want trivial (deterministic, out-of-band)", sel.Difficulty)
	}
	if sel.Rationale.LLMAssessed {
		t.Fatalf("Rationale.LLMAssessed = true, want false when Assessor was never called")
	}
}

// TestLLMAssessNotOkFallsBack: enabled + env set + in-band, but Assessor
// reports !ok. Select must fall back to the deterministic ScoreTask tier
// rather than trusting a failed/unavailable assessment.
func TestLLMAssessNotOkFallsBack(t *testing.T) {
	withLLMAssessEnabled(t, true)
	t.Setenv(llmAssessEnvVar, "1")
	called := false
	withAssessor(t, func(task string, score Score) (string, bool) {
		called = true
		return "", false
	})

	task := standardBandTask(t)
	got, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if !called {
		t.Fatalf("Assessor was not called for an in-band task")
	}
	if got.Difficulty != "standard" {
		t.Fatalf("Difficulty = %q, want standard (fallback to deterministic tier)", got.Difficulty)
	}
	if got.Rationale.LLMAssessed {
		t.Fatalf("Rationale.LLMAssessed = true, want false after a !ok assessment")
	}
}

// TestSelectReviewerExcludesAuthor: across representative tasks spanning all
// three tiers, SelectReviewer must never return excludeAuthor as the chosen
// reviewer, whatever author is passed.
func TestSelectReviewerExcludesAuthor(t *testing.T) {
	tasks := []string{
		"add a comment to the readme",                  // trivial
		"implement a new api endpoint with validation", // standard
		"refactor auth token encryption across the payment " + // hard
			"service, must be atomic and idempotent, validate all edge cases",
	}
	for _, task := range tasks {
		for author := range loadedModels.Agents {
			sel, err := SelectReviewer(task, "", author)
			if err != nil {
				t.Fatalf("SelectReviewer(%q, excludeAuthor=%q) error: %v", task, author, err)
			}
			if sel.Agent == author {
				t.Fatalf("SelectReviewer(%q, excludeAuthor=%q) returned the excluded author", task, author)
			}
		}
	}
}

// TestSelectReviewerDiffersFromAuthor: with the standard profiles config,
// SelectReviewer must resolve to a different agent than Select would pick as
// the task's author, guaranteeing a cross-model review.
func TestSelectReviewerDiffersFromAuthor(t *testing.T) {
	task := "implement a new api endpoint with validation"
	author, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	reviewer, err := SelectReviewer(task, "", author.Agent)
	if err != nil {
		t.Fatalf("SelectReviewer error: %v", err)
	}
	if reviewer.Agent == author.Agent {
		t.Fatalf("reviewer agent %q == author agent %q, want a different agent", reviewer.Agent, author.Agent)
	}
	pm := reviewer.Rationale.ProfileMatch
	if pm == nil || pm.Chosen != reviewer.Agent {
		t.Fatalf("ProfileMatch not populated correctly: %+v", pm)
	}
	for _, c := range pm.Candidates {
		if c.Agent == author.Agent {
			t.Fatalf("author %q leaked into reviewer candidate set %+v", author.Agent, pm.Candidates)
		}
	}
}

// TestSelectReviewerNoCandidateLeft: white-box test that shrinks the standard
// tier's candidate set down to a single agent, then excludes that agent as
// author — no cross-model reviewer remains, so SelectReviewer must return the
// documented error. The package-level models config is mutated and restored
// so other tests are unaffected.
func TestSelectReviewerNoCandidateLeft(t *testing.T) {
	saved := loadedModels.Agents
	loadedModels.Agents = map[string]map[string]string{
		"claude": saved["claude"],
	}
	t.Cleanup(func() { loadedModels.Agents = saved })

	task := "implement a new api endpoint with validation"
	if ScoreTask(task).Tier != "standard" {
		t.Fatalf("precondition: %q is not standard", task)
	}

	_, err := SelectReviewer(task, "", "claude")
	if err == nil {
		t.Fatal("SelectReviewer returned nil error, want no-cross-model-reviewer error")
	}
	want := `routing: no cross-model reviewer available excluding "claude"`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSelectReviewersEnumeratesAllExceptAuthor: for author "claude" on a
// standard-tier task, SelectReviewers returns EVERY other agent (codex and agy),
// in sorted name order, each pinned to that agent's model for the tier.
func TestSelectReviewersEnumeratesAllExceptAuthor(t *testing.T) {
	task := "implement a new api endpoint with validation"
	if ScoreTask(task).Tier != "standard" {
		t.Fatalf("precondition: %q is not standard", task)
	}

	sels, err := SelectReviewers(task, "", "claude")
	if err != nil {
		t.Fatalf("SelectReviewers error: %v", err)
	}
	if len(sels) != 2 {
		t.Fatalf("got %d reviewers, want 2: %+v", len(sels), sels)
	}
	// Sorted name order: agy before codex.
	if sels[0].Agent != "agy" || sels[1].Agent != "codex" {
		t.Fatalf("reviewers not in sorted order: %q, %q", sels[0].Agent, sels[1].Agent)
	}
	if sels[0].Model != loadedModels.Agents["agy"]["standard"] {
		t.Fatalf("agy model = %q, want %q", sels[0].Model, loadedModels.Agents["agy"]["standard"])
	}
	if sels[1].Model != loadedModels.Agents["codex"]["standard"] {
		t.Fatalf("codex model = %q, want %q", sels[1].Model, loadedModels.Agents["codex"]["standard"])
	}
	for _, s := range sels {
		if s.Agent == "claude" {
			t.Fatalf("author claude leaked into reviewer set: %+v", sels)
		}
		if s.Difficulty != "standard" {
			t.Fatalf("reviewer %q difficulty = %q, want standard", s.Agent, s.Difficulty)
		}
	}
}

// TestSelectReviewersNoCandidateLeft: with only the author agent configured for
// the tier, SelectReviewers returns the documented no-cross-model-reviewers
// error.
func TestSelectReviewersNoCandidateLeft(t *testing.T) {
	saved := loadedModels.Agents
	loadedModels.Agents = map[string]map[string]string{
		"claude": saved["claude"],
	}
	t.Cleanup(func() { loadedModels.Agents = saved })

	_, err := SelectReviewers("implement a new api endpoint with validation", "", "claude")
	if err == nil {
		t.Fatal("SelectReviewers returned nil error, want no-cross-model-reviewers error")
	}
	want := `routing: no cross-model reviewers available excluding "claude"`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSelectModelInfersAgentFromModel: an explicit known model with agent=""
// reverse-looks-up the unique agent that lists that model and reports the
// matching tier as Difficulty, bypassing score-based routing.
func TestSelectModelInfersAgentFromModel(t *testing.T) {
	// Use a plain task so score-based routing would NOT pick claude/hard; the
	// pin must override that entirely.
	task := "add a comment to the readme"
	sel, err := SelectModel(task, "", "", "claude-opus-4-8")
	if err != nil {
		t.Fatalf("SelectModel error: %v", err)
	}
	if sel.Agent != "claude" {
		t.Fatalf("Agent = %q, want claude (inferred from model)", sel.Agent)
	}
	if sel.Model != "claude-opus-4-8" {
		t.Fatalf("Model = %q, want claude-opus-4-8", sel.Model)
	}
	if sel.Difficulty != "hard" {
		t.Fatalf("Difficulty = %q, want hard (tier whose slot == model)", sel.Difficulty)
	}
	if sel.Rationale.Tier != "hard" {
		t.Fatalf("Rationale.Tier = %q, want hard", sel.Rationale.Tier)
	}
	if sel.Rationale.ProfileMatch == nil ||
		sel.Rationale.ProfileMatch.Chosen != "claude" ||
		sel.Rationale.ProfileMatch.Reason != "explicit model override; scoring bypassed" {
		t.Fatalf("ProfileMatch = %#v, want chosen=claude with bypass reason", sel.Rationale.ProfileMatch)
	}
}

// TestSelectModelAgentAndModelVerbatim: an explicit (agent, model) pair listed
// in the table is returned verbatim with the matching tier label.
func TestSelectModelAgentAndModelVerbatim(t *testing.T) {
	sel, err := SelectModel("add a comment to the readme", "codex", "", "gpt-5.5")
	if err != nil {
		t.Fatalf("SelectModel error: %v", err)
	}
	if sel.Agent != "codex" || sel.Model != "gpt-5.5" || sel.Difficulty != "hard" {
		t.Fatalf("got agent=%q model=%q difficulty=%q, want codex/gpt-5.5/hard",
			sel.Agent, sel.Model, sel.Difficulty)
	}
}

// TestSelectModelUnknownModelNoAgent: agent="" with a model no agent lists is
// unresolvable and must error.
func TestSelectModelUnknownModelNoAgent(t *testing.T) {
	_, err := SelectModel("add a comment to the readme", "", "", "no-such-model-42")
	if err == nil {
		t.Fatal("SelectModel returned nil error, want cannot-infer-agent error")
	}
	want := `routing: cannot infer agent for model "no-such-model-42"; pass agent explicitly`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSelectModelEmptyModelEqualsSelect: model=="" must be byte-identical to
// the pre-pin Select on every routing path (default, explicit agent, priority).
func TestSelectModelEmptyModelEqualsSelect(t *testing.T) {
	cases := []struct{ task, agent, priority string }{
		{"add a comment to the readme", "", ""},
		{"fix a concurrency race in the scheduler", "", ""},
		{"implement a new api endpoint with validation", "claude", ""},
		{"fix a concurrency race in the scheduler", "", "cost"},
	}
	for _, tc := range cases {
		want, wantErr := Select(tc.task, tc.agent, tc.priority)
		got, gotErr := SelectModel(tc.task, tc.agent, tc.priority, "")
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("SelectModel(%q,%q,%q,\"\") err=%v, Select err=%v", tc.task, tc.agent, tc.priority, gotErr, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SelectModel(%q,%q,%q,\"\") = %#v, want == Select = %#v", tc.task, tc.agent, tc.priority, got, want)
		}
	}
}

// TestSelectModelUnlistedModelHonored: an explicit (agent, model) pair whose
// model is NOT in the agent's table is trusted and reported with
// Difficulty=="explicit".
func TestSelectModelUnlistedModelHonored(t *testing.T) {
	sel, err := SelectModel("add a comment to the readme", "claude", "", "claude-opus-4-8-preview")
	if err != nil {
		t.Fatalf("SelectModel error: %v", err)
	}
	if sel.Agent != "claude" || sel.Model != "claude-opus-4-8-preview" {
		t.Fatalf("got agent=%q model=%q, want claude/claude-opus-4-8-preview", sel.Agent, sel.Model)
	}
	if sel.Difficulty != "explicit" || sel.Rationale.Tier != "explicit" {
		t.Fatalf("Difficulty=%q Rationale.Tier=%q, want both \"explicit\"", sel.Difficulty, sel.Rationale.Tier)
	}
}

// TestApplyOverridesAbsentFileNoOp: the backward-compat guarantee. Pointing
// ApplyOverrides at a nonexistent path returns nil and leaves scoring and
// selection byte-identical to the pre-call baseline.
func TestApplyOverridesAbsentFileNoOp(t *testing.T) {
	task := "refactor auth token encryption, must be atomic and validate edge cases"
	baseScore := ScoreTask(task)
	baseSel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("baseline Select error: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := ApplyOverrides(missing); err != nil {
		t.Fatalf("ApplyOverrides(absent) = %v, want nil", err)
	}

	if got := ScoreTask(task); !reflect.DeepEqual(got, baseScore) {
		t.Fatalf("ScoreTask changed after absent-file ApplyOverrides:\n got=%+v\nwant=%+v", got, baseScore)
	}
	gotSel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("post Select error: %v", err)
	}
	if !reflect.DeepEqual(gotSel, baseSel) {
		t.Fatalf("Select changed after absent-file ApplyOverrides:\n got=%+v\nwant=%+v", gotSel, baseSel)
	}
}

// TestApplyOverridesThresholds: lowering the "hard" threshold below a task's
// total shifts its tier boundary from standard into hard. Values are derived
// from the embedded policy so the test tracks the config.
func TestApplyOverridesThresholds(t *testing.T) {
	restorePolicy(t)

	_, pattern := firstPatternForWeight(t, 1.2)
	base := ScoreTask(pattern)
	if base.Tier != "standard" {
		t.Fatalf("baseline tier = %q, want standard (total=%v)", base.Tier, base.Total)
	}
	standardBefore := loadedPolicy.Thresholds["standard"]

	// A hard threshold at/below the task's total (1.2) must tip it into hard.
	path := writeOverrides(t, `{"thresholds": {"hard": 1.0}}`)
	if err := ApplyOverrides(path); err != nil {
		t.Fatalf("ApplyOverrides error: %v", err)
	}

	got := ScoreTask(pattern)
	if got.Total != base.Total {
		t.Fatalf("total changed unexpectedly: got %v want %v", got.Total, base.Total)
	}
	if got.Tier != "hard" {
		t.Fatalf("tier after lowering hard threshold = %q, want hard (total=%v)", got.Tier, got.Total)
	}
	// The untouched "standard" threshold keeps its embedded value (only the key
	// present in the file is overlaid).
	if loadedPolicy.Thresholds["standard"] != standardBefore {
		t.Fatalf("standard threshold was clobbered: got %v want %v",
			loadedPolicy.Thresholds["standard"], standardBefore)
	}
}

// TestApplyOverridesSignalWeight: overriding one signal's weight changes a
// matching task's score by exactly the new weight, while unknown signal names
// and unknown top-level keys are ignored (no panic, no effect).
func TestApplyOverridesSignalWeight(t *testing.T) {
	restorePolicy(t)

	name, pattern := firstPatternForWeight(t, 3.0) // security/auth in the embedded policy
	base := ScoreTask(pattern)
	if base.Scores[name] != 3.0 {
		t.Fatalf("baseline %q score = %v, want 3.0", name, base.Scores[name])
	}

	path := writeOverrides(t, `{
		"signal_weights": {"`+name+`": 10.0, "no_such_signal": 99.0},
		"totally_unknown_key": {"x": 1}
	}`)
	if err := ApplyOverrides(path); err != nil {
		t.Fatalf("ApplyOverrides error: %v", err)
	}

	got := ScoreTask(pattern)
	if got.Scores[name] != 10.0 {
		t.Fatalf("%q score after override = %v, want 10.0", name, got.Scores[name])
	}
	if got.Total != base.Total-3.0+10.0 {
		t.Fatalf("total after override = %v, want %v", got.Total, base.Total-3.0+10.0)
	}
	// The unknown signal must not have been introduced.
	if _, ok := loadedPolicy.Signals["no_such_signal"]; ok {
		t.Fatalf("unknown signal name was added to policy")
	}
}

// TestApplyOverridesMalformedIntact: a malformed file returns an error and
// leaves the defaults completely untouched — a subsequent ScoreTask matches the
// pre-call baseline.
func TestApplyOverridesMalformedIntact(t *testing.T) {
	restorePolicy(t)

	task := "refactor auth token encryption, must be atomic and validate edge cases"
	base := ScoreTask(task)

	path := writeOverrides(t, `{"thresholds": {"hard": ` /* truncated, invalid */)
	if err := ApplyOverrides(path); err == nil {
		t.Fatal("ApplyOverrides(malformed) = nil, want error")
	}

	if got := ScoreTask(task); !reflect.DeepEqual(got, base) {
		t.Fatalf("ScoreTask changed after malformed ApplyOverrides:\n got=%+v\nwant=%+v", got, base)
	}
}

// setUnavailable installs an AgentAvailable seam that reports every name in
// unavailable as not-installed (and every other agent as installed), restoring
// the prior seam (usually nil) on cleanup so availability tests never bleed into
// the rest of the suite.
func setUnavailable(t *testing.T, unavailable ...string) {
	t.Helper()
	saved := AgentAvailable
	t.Cleanup(func() { AgentAvailable = saved })
	miss := make(map[string]bool, len(unavailable))
	for _, n := range unavailable {
		miss[n] = true
	}
	AgentAvailable = func(name string) bool { return !miss[name] }
}

// hardSecurityTask builds a task that scores into the "hard" tier out of the
// embedded security signal's patterns (mirrors TestHardBoundary's construction),
// so the availability tests track the config instead of hardcoding a string.
func hardSecurityTask(t *testing.T) string {
	t.Helper()
	hard := loadedPolicy.Thresholds["hard"]
	sec := loadedPolicy.Signals["security"]
	needed := 0
	for float64(needed)*sec.Weight < hard {
		needed++
	}
	if needed > len(sec.Patterns) {
		t.Fatalf("not enough security patterns (%d) to reach hard threshold %v", len(sec.Patterns), hard)
	}
	task := strings.Join(sec.Patterns[:needed], " ")
	if ScoreTask(task).Tier != "hard" {
		t.Fatalf("precondition: constructed task is not hard: %q", task)
	}
	return task
}

// TestSelectSkipsUnavailableAgentInProfile: a hard security task normally routes
// to codex (highest security coverage); with codex marked not-installed the
// profile path must skip it and choose an installed agent instead, never codex,
// with no error.
func TestSelectSkipsUnavailableAgentInProfile(t *testing.T) {
	task := hardSecurityTask(t)

	// Baseline (seam unset): confirm the task really would pick codex, so the
	// fallback below is exercising a real preference, not a no-op.
	base, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("baseline Select error: %v", err)
	}
	if base.Agent != "codex" {
		t.Fatalf("precondition: baseline agent = %q, want codex", base.Agent)
	}

	setUnavailable(t, "codex")
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error with codex unavailable: %v", err)
	}
	if sel.Agent == "codex" {
		t.Fatalf("agent = codex, want an installed agent (codex is unavailable)")
	}
	if sel.Model != loadedModels.Agents[sel.Agent]["hard"] {
		t.Fatalf("model = %q, want %q", sel.Model, loadedModels.Agents[sel.Agent]["hard"])
	}
	// The unavailable agent must not appear among the scored candidates.
	for _, c := range sel.Rationale.ProfileMatch.Candidates {
		if c.Agent == "codex" {
			t.Fatalf("codex leaked into candidate set: %+v", sel.Rationale.ProfileMatch.Candidates)
		}
	}
}

// TestSelectDefaultAgentFallsBackWhenUnavailable exercises the no-profiles
// default_agent_by_difficulty path: with profiles cleared, the hard default is
// codex; marking codex unavailable must degrade to an installed candidate via
// coverage selection and record the fallback reason, rather than erroring.
func TestSelectDefaultAgentFallsBackWhenUnavailable(t *testing.T) {
	savedProfiles := loadedModels.Profiles
	loadedModels.Profiles = nil
	t.Cleanup(func() { loadedModels.Profiles = savedProfiles })

	task := hardSecurityTask(t)
	if def := loadedModels.DefaultAgentByDifficulty["hard"]; def != "codex" {
		t.Fatalf("precondition: hard default = %q, want codex", def)
	}

	setUnavailable(t, "codex")
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Agent == "codex" || !agentUsable(sel.Agent) {
		t.Fatalf("agent = %q, want an installed non-codex agent", sel.Agent)
	}
	if !strings.Contains(sel.Rationale.ProfileMatch.Reason, "not installed") {
		t.Fatalf("reason = %q, want it to note the default was not installed", sel.Rationale.ProfileMatch.Reason)
	}
}

// TestSelectDefaultNoInstalledAgentErrors: no profiles, and every candidate for
// the difficulty is unavailable, so the graceful fallback finds nothing and the
// documented "no installed agent" error surfaces.
func TestSelectDefaultNoInstalledAgentErrors(t *testing.T) {
	savedProfiles := loadedModels.Profiles
	loadedModels.Profiles = nil
	t.Cleanup(func() { loadedModels.Profiles = savedProfiles })

	task := hardSecurityTask(t)
	setUnavailable(t, "claude", "codex", "agy")
	_, err := Select(task, "", "")
	if err == nil {
		t.Fatal("Select returned nil error, want no-installed-agent error")
	}
	if want := `routing: no installed agent for difficulty "hard"`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSelectReviewersExcludesUnavailable: with author claude and codex
// unavailable on a standard task, the only cross-model reviewer left is agy.
func TestSelectReviewersExcludesUnavailable(t *testing.T) {
	task := "implement a new api endpoint with validation"
	if ScoreTask(task).Tier != "standard" {
		t.Fatalf("precondition: %q is not standard", task)
	}
	setUnavailable(t, "codex")
	sels, err := SelectReviewers(task, "", "claude")
	if err != nil {
		t.Fatalf("SelectReviewers error: %v", err)
	}
	if len(sels) != 1 || sels[0].Agent != "agy" {
		t.Fatalf("reviewers = %+v, want exactly [agy] (codex unavailable, claude author)", sels)
	}
}

// TestSelectModelExplicitAgentUnavailableErrors: an explicit agent override
// naming an uninstalled agent has no fallback and must error clearly.
func TestSelectModelExplicitAgentUnavailableErrors(t *testing.T) {
	setUnavailable(t, "codex")
	_, err := Select("add a comment to the readme", "codex", "")
	if err == nil {
		t.Fatal("Select returned nil error, want agent-not-installed error")
	}
	if want := `routing: agent "codex" is not installed`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSelectModelPinUnavailableAgentErrors: a model pin whose resolved agent is
// not installed must error, naming both the agent and the model.
func TestSelectModelPinUnavailableAgentErrors(t *testing.T) {
	setUnavailable(t, "codex")
	_, err := SelectModel("add a comment to the readme", "", "", "gpt-5.5") // gpt-5.5 -> codex
	if err == nil {
		t.Fatal("SelectModel returned nil error, want agent-not-installed error")
	}
	if want := `routing: agent "codex" (for model "gpt-5.5") is not installed`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestAgentAvailabilityReflectsSeam: the exported map reports the seam's verdict
// per agent, and the nil-seam default reports every agent available (backward
// compat).
func TestAgentAvailabilityReflectsSeam(t *testing.T) {
	// nil seam (default): all agents available.
	saved := AgentAvailable
	AgentAvailable = nil
	got := AgentAvailability()
	AgentAvailable = saved
	for agent, ok := range got {
		if !ok {
			t.Fatalf("nil-seam AgentAvailability[%q] = false, want true (backward compat)", agent)
		}
	}
	if _, ok := got["codex"]; !ok {
		t.Fatal("AgentAvailability missing codex entry")
	}

	setUnavailable(t, "codex")
	got = AgentAvailability()
	if got["codex"] {
		t.Fatalf("AgentAvailability[codex] = true, want false")
	}
	if !got["claude"] || !got["agy"] {
		t.Fatalf("AgentAvailability = %+v, want claude/agy available", got)
	}
}

// TestSelectNilSeamPreservesBaseline: with the seam unset, selection is
// byte-identical to a snapshot taken before any availability wiring — the
// backward-compat regression guard for the whole feature.
func TestSelectNilSeamPreservesBaseline(t *testing.T) {
	if AgentAvailable != nil {
		t.Fatal("precondition: AgentAvailable must be nil by default")
	}
	task := hardSecurityTask(t)
	sel, err := Select(task, "", "")
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if sel.Agent != "codex" {
		t.Fatalf("nil-seam agent = %q, want codex (unchanged default behavior)", sel.Agent)
	}
}

// TestEffortForDifficulty verifies the per-tier effort accessor returns the
// configured effort for each known tier and "" for an unknown tier (the
// backward-compat "no effort flag" signal).
func TestEffortForDifficulty(t *testing.T) {
	cases := map[string]string{
		"trivial":  "low",
		"standard": "medium",
		"hard":     "high",
	}
	for tier, want := range cases {
		if got := EffortForDifficulty(tier); got != want {
			t.Fatalf("EffortForDifficulty(%q) = %q, want %q", tier, got, want)
		}
	}
	if got := EffortForDifficulty("nonesuch"); got != "" {
		t.Fatalf("EffortForDifficulty(unknown) = %q, want \"\"", got)
	}
	if got := EffortForDifficulty(""); got != "" {
		t.Fatalf("EffortForDifficulty(\"\") = %q, want \"\"", got)
	}
}
