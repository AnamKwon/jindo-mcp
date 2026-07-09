package calibrate

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jindo/internal/routing"
)

// fixtureLog is a small dispatch.log with: two "standard" tier dispatches (one
// "ok", one "error", so the tier fails the 20% non-ok threshold), one "hard"
// tier "ok" dispatch whose total (6.0) is inside the hard near-threshold band
// [5.0,7.0], one "standard" dispatch whose total (1.0) is inside the standard
// near-threshold band [0.7,1.3], a blank line, and one garbled (non-JSON)
// line. Only the "security" and "constraints" signals ever appear in
// rationale.matched, so "scope" and "ambiguity" should be flagged as never
// matched.
const fixtureLog = `{"timestamp":"t1","key":"k1","task":"add auth","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}
{"timestamp":"t2","key":"k2","task":"add auth 2","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"error"}
{"timestamp":"t3","key":"k3","task":"refactor everything","agent":"codex","model":"m2","difficulty":"hard","rationale":{"matched":{"constraints":6},"total":6.0,"threshold":"hard","threshold_value":6.0,"tier":"hard"},"status":"ok"}
{"timestamp":"t4","key":"k4","task":"borderline task","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":1.0},"total":1.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}

not valid json at all
`

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(path, []byte(fixtureLog), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadDistributionsAndSignals(t *testing.T) {
	path := writeFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if r.ParsedLines != 4 {
		t.Fatalf("ParsedLines = %d, want 4", r.ParsedLines)
	}
	// One blank line + one garbled line = 2 skipped.
	if r.SkippedLines != 2 {
		t.Fatalf("SkippedLines = %d, want 2", r.SkippedLines)
	}

	standard := r.StatusByTier["standard"]
	if standard["ok"] != 2 || standard["error"] != 1 {
		t.Fatalf("StatusByTier[standard] = %v, want ok=2 error=1", standard)
	}
	hard := r.StatusByTier["hard"]
	if hard["ok"] != 1 {
		t.Fatalf("StatusByTier[hard] = %v, want ok=1", hard)
	}

	m1 := r.StatusByModel["m1"]
	if m1["ok"] != 2 || m1["error"] != 1 {
		t.Fatalf("StatusByModel[m1] = %v, want ok=2 error=1", m1)
	}

	if r.SignalFreq["security"] != 3 {
		t.Fatalf("SignalFreq[security] = %d, want 3", r.SignalFreq["security"])
	}
	if r.SignalFreq["constraints"] != 1 {
		t.Fatalf("SignalFreq[constraints] = %d, want 1", r.SignalFreq["constraints"])
	}
	if r.SignalFreq["scope"] != 0 || r.SignalFreq["ambiguity"] != 0 {
		t.Fatalf("expected scope/ambiguity to never match, got %v", r.SignalFreq)
	}

	// t4 (total=1.0) is inside the standard band [0.7,1.3]; t3 (total=6.0) is
	// inside the hard band [5.0,7.0].
	if r.NearThreshold != 2 {
		t.Fatalf("NearThreshold = %d, want 2", r.NearThreshold)
	}
}

func TestSuggestions(t *testing.T) {
	path := writeFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	joined := strings.Join(r.Suggestions, "\n")

	if !strings.Contains(joined, "tier standard has") {
		t.Fatalf("expected a standard-tier failure-rate suggestion, got: %v", r.Suggestions)
	}
	if !strings.Contains(joined, "signal scope never matched in 4 dispatches") {
		t.Fatalf("expected a never-matched suggestion for scope, got: %v", r.Suggestions)
	}
	if !strings.Contains(joined, "signal ambiguity never matched in 4 dispatches") {
		t.Fatalf("expected a never-matched suggestion for ambiguity, got: %v", r.Suggestions)
	}
	if !strings.Contains(joined, "near-threshold band") {
		t.Fatalf("expected a near-threshold suggestion, got: %v", r.Suggestions)
	}
	// hard tier is all-ok with a single sample: no failure suggestion for it.
	if strings.Contains(joined, "tier hard has") {
		t.Fatalf("did not expect a hard-tier failure suggestion, got: %v", r.Suggestions)
	}
}

func TestStringRenders(t *testing.T) {
	path := writeFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := r.String()
	for _, want := range []string{"parsed=4", "skipped=2", "near_threshold=2", "standard:", "hard:", "security:", "latency by model:", "suggestions:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() missing %q, got:\n%s", want, s)
		}
	}
}

// latencyFixtureLog carries model m1 with three duration_ms samples (100, 300,
// and a missing/zero one) and model m2 with a single 50ms sample, so the
// aggregation must: average/min/max only the two real m1 samples (100,300 ->
// avg=200,min=100,max=300), tally the missing line separately without letting
// it pull m1's min down to 0, and report m2 with a single real sample.
const latencyFixtureLog = `{"timestamp":"t1","key":"k1","task":"a","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","duration_ms":100}
{"timestamp":"t2","key":"k2","task":"b","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","duration_ms":300}
{"timestamp":"t3","key":"k3","task":"c","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok"}
{"timestamp":"t4","key":"k4","task":"d","agent":"codex","model":"m2","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","duration_ms":50}
`

func writeLatencyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(path, []byte(latencyFixtureLog), 0o644); err != nil {
		t.Fatalf("write latency fixture: %v", err)
	}
	return path
}

func TestLatencyByModelAggregation(t *testing.T) {
	path := writeLatencyFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	m1 := r.LatencyByModel["m1"]
	if m1 == nil {
		t.Fatalf("LatencyByModel[m1] missing, got %v", r.LatencyByModel)
	}
	// The missing-duration line (t3) must not skew avg/min/max: only 100 and
	// 300 count, so avg=200, min=100 (not 0), max=300.
	if m1.Count != 2 {
		t.Fatalf("m1.Count = %d, want 2", m1.Count)
	}
	if m1.Missing != 1 {
		t.Fatalf("m1.Missing = %d, want 1", m1.Missing)
	}
	if m1.AvgMs != 200 {
		t.Fatalf("m1.AvgMs = %v, want 200", m1.AvgMs)
	}
	if m1.MinMs != 100 {
		t.Fatalf("m1.MinMs = %d, want 100 (must not be dragged to 0 by the missing sample)", m1.MinMs)
	}
	if m1.MaxMs != 300 {
		t.Fatalf("m1.MaxMs = %d, want 300", m1.MaxMs)
	}

	m2 := r.LatencyByModel["m2"]
	if m2 == nil {
		t.Fatalf("LatencyByModel[m2] missing, got %v", r.LatencyByModel)
	}
	if m2.Count != 1 || m2.Missing != 0 || m2.AvgMs != 50 || m2.MinMs != 50 || m2.MaxMs != 50 {
		t.Fatalf("m2 = %+v, want count=1 missing=0 avg=min=max=50", m2)
	}

	s := r.String()
	for _, want := range []string{"latency by model:", "m1: count=2 avg=200.0ms min=100ms max=300ms missing=1", "m2: count=1 avg=50.0ms min=50ms max=50ms missing=0"} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() missing %q, got:\n%s", want, s)
		}
	}
}

// TestLatencyByModelAllMissing covers a model whose every sample lacks
// duration_ms (e.g. dispatch.log lines written before this field existed):
// Count stays 0 (no misleading avg/min/max of 0) and String() renders "no
// samples" instead of a fabricated distribution.
func TestLatencyByModelAllMissing(t *testing.T) {
	path := writeFixture(t) // fixtureLog has no duration_ms on any line.
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m1 := r.LatencyByModel["m1"]
	if m1 == nil {
		t.Fatalf("LatencyByModel[m1] missing")
	}
	if m1.Count != 0 || m1.Missing != 3 {
		t.Fatalf("m1 = %+v, want count=0 missing=3", m1)
	}
	if !strings.Contains(r.String(), "m1: no samples (missing=3)") {
		t.Fatalf("String() missing the no-samples line for m1, got:\n%s", r.String())
	}
}

// reviewFixtureLog carries four dispatches authored by model m1: two reviewed
// with verdict "changes_requested" (one of which also ended "review_failed"
// because the revision re-dispatch failed), one reviewed "approved", and one
// with no Review at all (review pipeline off for that dispatch). m1's
// changes_requested rate is 2/3 reviewed, well above failureRateThreshold, so
// it should surface a review-profile suggestion.
const reviewFixtureLog = `{"timestamp":"t1","key":"k1","task":"a","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"approved","findings":{"total":0},"revision_rounds":0,"final_status":"ok"}}
{"timestamp":"t2","key":"k2","task":"b","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"changes_requested","findings":{"total":1,"critical":1},"revision_rounds":1,"final_status":"ok"}}
{"timestamp":"t3","key":"k3","task":"c","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"review_failed","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"changes_requested","findings":{"total":1,"critical":1},"revision_rounds":1,"final_status":"review_failed"}}
{"timestamp":"t4","key":"k4","task":"d","agent":"claude","model":"m1","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok"}
`

func writeReviewFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(path, []byte(reviewFixtureLog), 0o644); err != nil {
		t.Fatalf("write review fixture: %v", err)
	}
	return path
}

func TestReviewAggregation(t *testing.T) {
	path := writeReviewFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if r.ReviewedCount != 3 {
		t.Fatalf("ReviewedCount = %d, want 3 (t4 has no review)", r.ReviewedCount)
	}
	if r.ReviewVerdicts["approved"] != 1 || r.ReviewVerdicts["changes_requested"] != 2 {
		t.Fatalf("ReviewVerdicts = %v, want approved=1 changes_requested=2", r.ReviewVerdicts)
	}
	if r.ReviewErrored != 0 {
		t.Fatalf("ReviewErrored = %d, want 0", r.ReviewErrored)
	}

	m1 := r.ReviewByAuthorModel["m1"]
	if m1 == nil {
		t.Fatalf("ReviewByAuthorModel[m1] missing")
	}
	if m1.Reviewed != 3 || m1.ChangesRequested != 2 || m1.ReviewFailed != 1 {
		t.Fatalf("ReviewByAuthorModel[m1] = %+v, want reviewed=3 changes_requested=2 review_failed=1", m1)
	}

	s := r.String()
	for _, want := range []string{"review: reviewed=3 errored=0", "approved=1 changes_requested=2", "m1: reviewed=3 review_failed=1 changes_requested=2"} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() missing %q, got:\n%s", want, s)
		}
	}

	joined := strings.Join(r.Suggestions, "\n")
	if !strings.Contains(joined, "author model m1 has") || !strings.Contains(joined, "review profile weights") {
		t.Fatalf("expected an author-model review suggestion for m1, got: %v", r.Suggestions)
	}
}

// TestReviewAggregationNoReviews guards the no-review path: fixtureLog has no
// "review" key on any line, so aggregation must report zero reviews and
// String() must render "no reviews" rather than an empty/misleading section.
func TestReviewAggregationNoReviews(t *testing.T) {
	path := writeFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.ReviewedCount != 0 {
		t.Fatalf("ReviewedCount = %d, want 0", r.ReviewedCount)
	}
	if !strings.Contains(r.String(), "review: no reviews") {
		t.Fatalf("String() missing 'review: no reviews', got:\n%s", r.String())
	}
}

// findingsFixtureLog exercises severity-level aggregation across two author
// models: m3 recurs critical/major findings across enough reviewed dispatches
// to clear minSample and failureRateThreshold, while m4 has a single
// high-severity review that must NOT trigger a suggestion (below minSample).
const findingsFixtureLog = `{"timestamp":"t1","key":"k1","task":"a","agent":"claude","model":"m3","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"approved","findings":{"total":2,"critical":1,"minor":1},"revision_rounds":0,"final_status":"ok"}}
{"timestamp":"t2","key":"k2","task":"b","agent":"claude","model":"m3","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"approved","findings":{"total":1,"major":1},"revision_rounds":0,"final_status":"ok"}}
{"timestamp":"t3","key":"k3","task":"c","agent":"claude","model":"m3","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"approved","findings":{"total":2,"critical":1,"info":1},"revision_rounds":0,"final_status":"ok"}}
{"timestamp":"t4","key":"k4","task":"d","agent":"claude","model":"m4","difficulty":"standard","rationale":{"total":1.0,"tier":"standard"},"status":"ok","review":{"reviewer_agent":"codex","reviewer_model":"m2","verdict":"changes_requested","findings":{"total":5,"critical":5},"revision_rounds":1,"final_status":"ok"}}
`

func writeFindingsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(path, []byte(findingsFixtureLog), 0o644); err != nil {
		t.Fatalf("write findings fixture: %v", err)
	}
	return path
}

func TestReviewFindingsBySeverity(t *testing.T) {
	path := writeFindingsFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := ReviewFindingCounts{Total: 10, Critical: 7, Major: 1, Minor: 1, Info: 1}
	if r.ReviewFindingTotals != want {
		t.Fatalf("ReviewFindingTotals = %+v, want %+v", r.ReviewFindingTotals, want)
	}

	m3 := r.ReviewByAuthorModel["m3"]
	if m3 == nil || m3.Reviewed != 3 || m3.CriticalMajor != 3 {
		t.Fatalf("ReviewByAuthorModel[m3] = %+v, want reviewed=3 critical_major=3", m3)
	}
	m4 := r.ReviewByAuthorModel["m4"]
	if m4 == nil || m4.Reviewed != 1 || m4.CriticalMajor != 5 {
		t.Fatalf("ReviewByAuthorModel[m4] = %+v, want reviewed=1 critical_major=5", m4)
	}

	s := r.String()
	for _, want := range []string{
		"findings by severity: critical=7 major=1 minor=1 info=1 (total=10)",
		"m3: reviewed=3 review_failed=0 changes_requested=0 critical_major=3",
		"m4: reviewed=1 review_failed=0 changes_requested=1 critical_major=5",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("String() missing %q, got:\n%s", want, s)
		}
	}

	joined := strings.Join(r.Suggestions, "\n")
	if !strings.Contains(joined, "author model m3 has recurring critical/major review findings") {
		t.Fatalf("expected a recurring-findings suggestion for m3 (above minSample), got: %v", r.Suggestions)
	}
	if strings.Contains(joined, "author model m4 has recurring critical/major review findings") {
		t.Fatalf("did not expect a recurring-findings suggestion for m4 (below minSample), got: %v", r.Suggestions)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.log")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestIsNearThresholdUsesRoutingBands guards against calibrate drifting from
// routing's llm_assess.bands (the former nearThresholdBands duplicate): every
// band boundary routing.AssessBands() reports must classify as near-threshold
// here too, since isNearThreshold now reads that accessor directly.
func TestIsNearThresholdUsesRoutingBands(t *testing.T) {
	bands := routing.AssessBands()
	if len(bands) == 0 {
		t.Fatal("routing.AssessBands() returned no bands")
	}
	for name, b := range bands {
		if !isNearThreshold(b[0]) {
			t.Errorf("band %s: isNearThreshold(%v) = false, want true (inclusive low bound)", name, b[0])
		}
		if !isNearThreshold(b[1]) {
			t.Errorf("band %s: isNearThreshold(%v) = false, want true (inclusive high bound)", name, b[1])
		}
	}
}

// TestSuggestionsUsesRoutingSignals guards against calibrate drifting from
// routing's signal table (the former knownSignals duplicate): buildSuggestions
// must flag a never-matched note for every signal routing.KnownSignals()
// reports, keyed off the live policy rather than a hand-copied list.
func TestSuggestionsUsesRoutingSignals(t *testing.T) {
	path := writeFixture(t)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	joined := strings.Join(r.Suggestions, "\n")
	for _, signal := range routing.KnownSignals() {
		if r.SignalFreq[signal] > 0 {
			continue
		}
		want := "signal " + signal + " never matched in " + strconv.Itoa(r.ParsedLines) + " dispatches"
		if !strings.Contains(joined, want) {
			t.Errorf("suggestions missing %q (signal from routing.KnownSignals()), got: %v", want, r.Suggestions)
		}
	}
}

// restoreThresholds captures the live routing thresholds and registers a
// cleanup that writes them back via ApplyOverrides, so a test that applies a
// derived override does not leak mutated global routing state into later tests.
func restoreThresholds(t *testing.T) {
	t.Helper()
	orig := routing.Thresholds()
	t.Cleanup(func() {
		ov := Overrides{Thresholds: orig}
		data, err := ov.Marshal()
		if err != nil {
			t.Fatalf("restore marshal: %v", err)
		}
		p := filepath.Join(t.TempDir(), "restore.json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("restore write: %v", err)
		}
		if err := routing.ApplyOverrides(p); err != nil {
			t.Fatalf("restore ApplyOverrides: %v", err)
		}
	})
}

// TestDeriveOverridesRoundTrip verifies the gated apply loop end to end: the
// fixture's "standard" tier has a 1/3 non-ok rate (above failureRateThreshold,
// >= minSample), so DeriveOverrides nudges its threshold up by thresholdNudge;
// its "hard" tier has a single sample (< minSample), so it is left untouched.
// The marshalled overrides, written and loaded via routing.ApplyOverrides,
// actually change the live routing threshold.
func TestDeriveOverridesRoundTrip(t *testing.T) {
	restoreThresholds(t)

	r, err := Load(writeFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	baseStandard, ok := routing.Thresholds()["standard"]
	if !ok {
		t.Fatalf("routing has no standard threshold baseline")
	}

	ov := r.DeriveOverrides()
	if ov.Empty() {
		t.Fatalf("DeriveOverrides returned empty, want a standard threshold nudge")
	}
	wantStandard := baseStandard + thresholdNudge
	if got := ov.Thresholds["standard"]; got != wantStandard {
		t.Errorf("derived standard threshold = %v, want %v", got, wantStandard)
	}
	if _, hardNudged := ov.Thresholds["hard"]; hardNudged {
		t.Errorf("hard threshold nudged but its sample count is below minSample: %v", ov.Thresholds)
	}
	if len(ov.SignalWeights) != 0 || len(ov.AssessBands) != 0 {
		t.Errorf("DeriveOverrides emitted non-threshold deltas: weights=%v bands=%v", ov.SignalWeights, ov.AssessBands)
	}

	data, err := ov.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	p := filepath.Join(t.TempDir(), "routing_overrides.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	if err := routing.ApplyOverrides(p); err != nil {
		t.Fatalf("ApplyOverrides on derived overrides: %v", err)
	}
	if got := routing.Thresholds()["standard"]; got != wantStandard {
		t.Errorf("after ApplyOverrides, standard threshold = %v, want %v", got, wantStandard)
	}
}

// TestDeriveOverridesCleanLog verifies a report with no flagged tier (every
// tier at or below the non-ok rate threshold) yields a no-op Overrides.
func TestDeriveOverridesCleanLog(t *testing.T) {
	const cleanLog = `{"timestamp":"t1","key":"k1","task":"a","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}
{"timestamp":"t2","key":"k2","task":"b","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}
{"timestamp":"t3","key":"k3","task":"c","agent":"codex","model":"m2","difficulty":"hard","rationale":{"matched":{"constraints":6},"total":6.0,"threshold":"hard","threshold_value":6.0,"tier":"hard"},"status":"ok"}
{"timestamp":"t4","key":"k4","task":"d","agent":"codex","model":"m2","difficulty":"hard","rationale":{"matched":{"constraints":6},"total":6.0,"threshold":"hard","threshold_value":6.0,"tier":"hard"},"status":"ok"}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(path, []byte(cleanLog), 0o644); err != nil {
		t.Fatalf("write clean log: %v", err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ov := r.DeriveOverrides()
	if !ov.Empty() {
		t.Errorf("clean log DeriveOverrides not empty: %+v", ov)
	}
}
