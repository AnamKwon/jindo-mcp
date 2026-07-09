// Package calibrate reads jindo's dispatch.log (the JSONL audit trail appended
// by internal/orchestrator.appendDispatchLog) and aggregates it into a
// calibration report: how dispatches landed across tiers/models, which
// routing signals actually fired, how many dispatches scored close enough to
// a tier boundary to be worth re-judging, and a set of suggested (never
// applied) threshold/weight adjustments an operator can act on by hand.
//
// This package only reads the log; it never mutates internal/routing's
// policy config.
package calibrate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"jindo/internal/routing"
)

// Rationale mirrors the routing.Rationale fields carried on each dispatch.log
// line, restricted to what this package consumes (see routing.Rationale for
// the authoritative shape).
type Rationale struct {
	Matched        map[string]float64 `json:"matched"`
	Total          float64            `json:"total"`
	Threshold      string             `json:"threshold"`
	ThresholdValue float64            `json:"threshold_value"`
	Tier           string             `json:"tier"`
	Priority       string             `json:"priority"`
}

// Entry mirrors one dispatch.log JSONL line (see
// internal/orchestrator.dispatchLogEntry), restricted to the fields this
// package consumes.
type Entry struct {
	Timestamp  string    `json:"timestamp"`
	Key        string    `json:"key"`
	Task       string    `json:"task"`
	Agent      string    `json:"agent"`
	Model      string    `json:"model"`
	Difficulty string    `json:"difficulty"`
	Rationale  Rationale `json:"rationale"`
	Status     string    `json:"status"`
	// DurationMs is the author adapter run latency (see
	// internal/orchestrator.dispatchLogEntry.DurationMs). Log lines written
	// before this field existed decode it as the Go zero value (0), which Load
	// treats as "missing" rather than a real zero-latency sample (see
	// ModelLatency).
	DurationMs int64 `json:"duration_ms"`
	// Review mirrors internal/orchestrator.dispatchLogEntry.Review: set only
	// when a review pipeline ran for this dispatch. Its absence (nil) must not
	// be confused with a review that ran and was errored (see Review.Errored).
	Review *Review `json:"review,omitempty"`
}

// Review mirrors internal/orchestrator.reviewRecord, restricted to the fields
// this package consumes (reviewer identity, verdict, severity counts,
// revision rounds, final status, and the best-effort error flag).
type Review struct {
	ReviewerAgent  string              `json:"reviewer_agent"`
	ReviewerModel  string              `json:"reviewer_model"`
	Verdict        string              `json:"verdict"`
	Findings       ReviewFindingCounts `json:"findings"`
	RevisionRounds int                 `json:"revision_rounds"`
	FinalStatus    string              `json:"final_status"`
	Errored        bool                `json:"errored,omitempty"`
}

// ReviewFindingCounts mirrors internal/orchestrator.findingCounts.
type ReviewFindingCounts struct {
	Total    int `json:"total"`
	Critical int `json:"critical,omitempty"`
	Major    int `json:"major,omitempty"`
	Minor    int `json:"minor,omitempty"`
	Info     int `json:"info,omitempty"`
}

// Suggestion thresholds: a tier's non-"ok" rate above failureRateThreshold,
// with at least minSample dispatches to be statistically meaningful, is
// flagged for review.
const (
	failureRateThreshold = 0.2
	minSample            = 2
)

// isNearThreshold reports whether total falls inside any of routing's
// configured llm_assess.bands ranges ([low, high], inclusive). It defers to
// routing.AssessBands (the source of truth for routing_policy.json's
// llm_assess.bands) instead of a hand-duplicated copy, so this stays in sync
// with the policy automatically.
func isNearThreshold(total float64) bool {
	for _, b := range routing.AssessBands() {
		if total >= b[0] && total <= b[1] {
			return true
		}
	}
	return false
}

// Report is the aggregated calibration view over one dispatch.log.
type Report struct {
	Path string

	// ParsedLines counts JSONL lines successfully decoded into an Entry.
	ParsedLines int
	// SkippedLines counts blank lines and lines that failed to decode
	// (garbled JSON); these are excluded from every other field below.
	SkippedLines int

	// StatusByTier[tier][status] = count.
	StatusByTier map[string]map[string]int
	// StatusByModel[model][status] = count.
	StatusByModel map[string]map[string]int

	// SignalFreq[signal] = number of dispatches whose rationale.matched
	// included that signal.
	SignalFreq map[string]int

	// NearThreshold counts dispatches whose rationale.total fell inside a
	// nearThresholdBands range.
	NearThreshold int

	// LatencyByModel[model] aggregates author-run duration_ms per model (see
	// ModelLatency for how missing/zero values are handled).
	LatencyByModel map[string]*ModelLatency

	// Suggestions are human-readable, advisory-only threshold/weight
	// adjustment strings. They never mutate any config.
	Suggestions []string

	// ReviewedCount counts dispatches whose log line carried a Review (i.e.
	// the review pipeline ran), out of ParsedLines.
	ReviewedCount int
	// ReviewVerdicts[verdict] = count, over dispatches with a Review.
	ReviewVerdicts map[string]int
	// ReviewErrored counts reviews marked Errored (best-effort reviewer
	// failure that left the author's result unchanged).
	ReviewErrored int
	// ReviewByAuthorModel[model] aggregates review outcomes for dispatches
	// authored by that model (keyed by Entry.Model, not ReviewerModel).
	ReviewByAuthorModel map[string]*ReviewOutcome

	// ReviewFindingTotals sums Review.Findings across every dispatch with a
	// non-nil Review, regardless of verdict, so recurring severity patterns
	// are visible even when no single review triggers changes_requested.
	ReviewFindingTotals ReviewFindingCounts
}

// ReviewOutcome tallies one author model's review outcomes: how many of its
// dispatches were reviewed, and of those how many ended review_failed (the
// revision round's re-dispatch itself failed) or changes_requested (the
// reviewer found a critical issue, whether or not the revision recovered).
type ReviewOutcome struct {
	Model            string
	Reviewed         int
	ReviewFailed     int
	ChangesRequested int

	// CriticalMajor sums Critical+Major findings across this model's
	// reviewed dispatches, tracked separately from ChangesRequested since a
	// review can carry critical/major findings without the reviewer setting
	// verdict to changes_requested (see buildSuggestions).
	CriticalMajor int
}

// ModelLatency aggregates one model's per-dispatch author-run duration_ms
// samples (count, average, min, max).
//
// A dispatch.log line with no duration_ms (older lines predating this field)
// or a literal 0 both decode to the Go zero value, and cannot be told apart
// after JSON decoding. Rather than let an unset value masquerade as a genuine
// 0ms run and skew Min/Avg downward, Load treats every duration_ms == 0 as
// "missing": it is tallied in Missing but excluded from Count/AvgMs/MinMs/
// MaxMs. A model with samples entirely made of zeros/missing values reports
// Count == 0 (see String(), which renders "no samples" for that case) rather
// than a misleading avg/min/max of 0.
type ModelLatency struct {
	Model string

	// Count is the number of samples with duration_ms > 0, contributing to
	// AvgMs/MinMs/MaxMs below.
	Count int
	// Missing is the number of dispatches for this model whose duration_ms
	// was 0 (absent from the log line or a genuine zero), excluded from the
	// distribution below.
	Missing int

	AvgMs float64
	MinMs int64
	MaxMs int64

	// sumMs accumulates Count samples' duration_ms; used to compute AvgMs once
	// all lines are parsed.
	sumMs int64
}

// Load reads the dispatch.log JSONL file at path and returns the aggregated
// Report. Blank lines and lines that fail to unmarshal as an Entry are
// skipped (counted in SkippedLines) rather than failing the whole read, since
// a single garbled line must not hide the rest of the log's signal.
func Load(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("calibrate: open %s: %w", path, err)
	}
	defer f.Close()

	r := &Report{
		Path:                path,
		StatusByTier:        map[string]map[string]int{},
		StatusByModel:       map[string]map[string]int{},
		SignalFreq:          map[string]int{},
		LatencyByModel:      map[string]*ModelLatency{},
		ReviewVerdicts:      map[string]int{},
		ReviewByAuthorModel: map[string]*ReviewOutcome{},
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			r.SkippedLines++
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			r.SkippedLines++
			continue
		}
		r.ParsedLines++
		r.addEntry(e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("calibrate: scan %s: %w", path, err)
	}

	r.finalizeLatency()
	r.Suggestions = r.buildSuggestions()
	return r, nil
}

// finalizeLatency computes each ModelLatency's AvgMs from its accumulated sum
// now that every line has been folded in, since Avg is only meaningful once
// Count is final.
func (r *Report) finalizeLatency() {
	for _, lm := range r.LatencyByModel {
		if lm.Count > 0 {
			lm.AvgMs = float64(lm.sumMs) / float64(lm.Count)
		}
	}
}

// addEntry folds one decoded Entry into the running distributions.
func (r *Report) addEntry(e Entry) {
	tier := e.Rationale.Tier
	if tier == "" {
		tier = e.Difficulty
	}
	if _, ok := r.StatusByTier[tier]; !ok {
		r.StatusByTier[tier] = map[string]int{}
	}
	r.StatusByTier[tier][e.Status]++

	if _, ok := r.StatusByModel[e.Model]; !ok {
		r.StatusByModel[e.Model] = map[string]int{}
	}
	r.StatusByModel[e.Model][e.Status]++

	for signal := range e.Rationale.Matched {
		r.SignalFreq[signal]++
	}

	if isNearThreshold(e.Rationale.Total) {
		r.NearThreshold++
	}

	lm, ok := r.LatencyByModel[e.Model]
	if !ok {
		lm = &ModelLatency{Model: e.Model}
		r.LatencyByModel[e.Model] = lm
	}
	if e.DurationMs > 0 {
		lm.Count++
		lm.sumMs += e.DurationMs
		if lm.MinMs == 0 || e.DurationMs < lm.MinMs {
			lm.MinMs = e.DurationMs
		}
		if e.DurationMs > lm.MaxMs {
			lm.MaxMs = e.DurationMs
		}
	} else {
		lm.Missing++
	}

	if e.Review != nil {
		r.ReviewedCount++
		r.ReviewVerdicts[e.Review.Verdict]++
		if e.Review.Errored {
			r.ReviewErrored++
		}
		r.ReviewFindingTotals.Total += e.Review.Findings.Total
		r.ReviewFindingTotals.Critical += e.Review.Findings.Critical
		r.ReviewFindingTotals.Major += e.Review.Findings.Major
		r.ReviewFindingTotals.Minor += e.Review.Findings.Minor
		r.ReviewFindingTotals.Info += e.Review.Findings.Info

		ro, ok := r.ReviewByAuthorModel[e.Model]
		if !ok {
			ro = &ReviewOutcome{Model: e.Model}
			r.ReviewByAuthorModel[e.Model] = ro
		}
		ro.Reviewed++
		if e.Review.FinalStatus == "review_failed" {
			ro.ReviewFailed++
		}
		if e.Review.Verdict == "changes_requested" {
			ro.ChangesRequested++
		}
		ro.CriticalMajor += e.Review.Findings.Critical + e.Review.Findings.Major
	}
}

// buildSuggestions derives advisory-only threshold/weight adjustment strings
// from the aggregated distributions. Nothing here mutates routing config; the
// strings are meant for a human to act on.
func (r *Report) buildSuggestions() []string {
	var out []string

	for _, tier := range sortedKeys(r.StatusByTier) {
		statuses := r.StatusByTier[tier]
		total := 0
		nonOK := 0
		for status, n := range statuses {
			total += n
			if status != "ok" {
				nonOK += n
			}
		}
		if total < minSample {
			continue
		}
		rate := float64(nonOK) / float64(total)
		if rate > failureRateThreshold {
			out = append(out, fmt.Sprintf(
				"tier %s has %.0f%% non-ok outcomes (%d/%d), consider adjusting its threshold_value or reweighting signals",
				tier, rate*100, nonOK, total))
		}
	}

	for _, signal := range routing.KnownSignals() {
		if r.SignalFreq[signal] == 0 && r.ParsedLines > 0 {
			out = append(out, fmt.Sprintf(
				"signal %s never matched in %d dispatches, consider reviewing its patterns or weight",
				signal, r.ParsedLines))
		}
	}

	if r.NearThreshold > 0 && r.ParsedLines > 0 {
		rate := float64(r.NearThreshold) / float64(r.ParsedLines)
		out = append(out, fmt.Sprintf(
			"%d/%d dispatches (%.0f%%) scored within a near-threshold band, consider widening/narrowing llm_assess.bands or adjusting the crossed threshold_value",
			r.NearThreshold, r.ParsedLines, rate*100))
	}

	for _, model := range sortedReviewModels(r.ReviewByAuthorModel) {
		ro := r.ReviewByAuthorModel[model]
		if ro.Reviewed < minSample {
			continue
		}
		rate := float64(ro.ChangesRequested) / float64(ro.Reviewed)
		if rate > failureRateThreshold {
			out = append(out, fmt.Sprintf(
				"author model %s has %.0f%% changes_requested rate (%d/%d reviewed), consider adjusting its review profile weights",
				model, rate*100, ro.ChangesRequested, ro.Reviewed))
		}
	}

	for _, model := range sortedReviewModels(r.ReviewByAuthorModel) {
		ro := r.ReviewByAuthorModel[model]
		if ro.Reviewed < minSample {
			continue
		}
		rate := float64(ro.CriticalMajor) / float64(ro.Reviewed)
		if rate > failureRateThreshold {
			out = append(out, fmt.Sprintf(
				"author model %s has recurring critical/major review findings (%.1f per reviewed dispatch, %d findings over %d reviewed), consider adjusting its review profile or task assignment",
				model, rate, ro.CriticalMajor, ro.Reviewed))
		}
	}

	return out
}

// sortedReviewModels returns m's keys sorted, for deterministic suggestion
// and String() order.
func sortedReviewModels(m map[string]*ReviewOutcome) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeys returns m's keys sorted, for deterministic suggestion order.
func sortedKeys(m map[string]map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// String renders a human-readable calibration report.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Calibration report for %s\n", r.Path)
	fmt.Fprintf(&b, "  parsed=%d skipped=%d near_threshold=%d\n", r.ParsedLines, r.SkippedLines, r.NearThreshold)

	b.WriteString("  status by tier:\n")
	for _, tier := range sortedKeys(r.StatusByTier) {
		fmt.Fprintf(&b, "    %s: %s\n", tier, formatCounts(r.StatusByTier[tier]))
	}

	b.WriteString("  status by model:\n")
	for _, model := range sortedKeys(r.StatusByModel) {
		fmt.Fprintf(&b, "    %s: %s\n", model, formatCounts(r.StatusByModel[model]))
	}

	b.WriteString("  signal match frequency:\n")
	signals := make([]string, 0, len(r.SignalFreq))
	for s := range r.SignalFreq {
		signals = append(signals, s)
	}
	sort.Strings(signals)
	for _, s := range signals {
		fmt.Fprintf(&b, "    %s: %d\n", s, r.SignalFreq[s])
	}

	b.WriteString("  latency by model:\n")
	models := make([]string, 0, len(r.LatencyByModel))
	for m := range r.LatencyByModel {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		lm := r.LatencyByModel[m]
		if lm.Count == 0 {
			fmt.Fprintf(&b, "    %s: no samples (missing=%d)\n", m, lm.Missing)
			continue
		}
		fmt.Fprintf(&b, "    %s: count=%d avg=%.1fms min=%dms max=%dms missing=%d\n",
			m, lm.Count, lm.AvgMs, lm.MinMs, lm.MaxMs, lm.Missing)
	}

	if r.ReviewedCount == 0 {
		b.WriteString("  review: no reviews\n")
	} else {
		fmt.Fprintf(&b, "  review: reviewed=%d errored=%d\n", r.ReviewedCount, r.ReviewErrored)
		b.WriteString("    verdicts: ")
		verdicts := make([]string, 0, len(r.ReviewVerdicts))
		for v := range r.ReviewVerdicts {
			verdicts = append(verdicts, v)
		}
		sort.Strings(verdicts)
		parts := make([]string, 0, len(verdicts))
		for _, v := range verdicts {
			parts = append(parts, fmt.Sprintf("%s=%d", v, r.ReviewVerdicts[v]))
		}
		b.WriteString(strings.Join(parts, " "))
		b.WriteString("\n")
		fmt.Fprintf(&b, "    findings by severity: critical=%d major=%d minor=%d info=%d (total=%d)\n",
			r.ReviewFindingTotals.Critical, r.ReviewFindingTotals.Major,
			r.ReviewFindingTotals.Minor, r.ReviewFindingTotals.Info, r.ReviewFindingTotals.Total)
		b.WriteString("    by author model:\n")
		for _, model := range sortedReviewModels(r.ReviewByAuthorModel) {
			ro := r.ReviewByAuthorModel[model]
			fmt.Fprintf(&b, "      %s: reviewed=%d review_failed=%d changes_requested=%d critical_major=%d\n",
				model, ro.Reviewed, ro.ReviewFailed, ro.ChangesRequested, ro.CriticalMajor)
		}
	}

	if len(r.Suggestions) == 0 {
		b.WriteString("  suggestions: none\n")
	} else {
		b.WriteString("  suggestions:\n")
		for _, s := range r.Suggestions {
			fmt.Fprintf(&b, "    - %s\n", s)
		}
	}

	return b.String()
}

// formatCounts renders a status->count map as "status=count" pairs sorted by
// status name, for stable String() output.
func formatCounts(counts map[string]int) string {
	statuses := make([]string, 0, len(counts))
	for s := range counts {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
	}
	return strings.Join(parts, " ")
}

// thresholdNudge is the fixed, conservative absolute step DeriveOverrides adds
// to a tier's gating threshold when that tier is flagged. It is deliberately
// small so a single apply moves the boundary only slightly; repeated
// calibrate+apply passes hill-climb toward a stable configuration rather than
// overshooting in one step.
const thresholdNudge = 0.5

// gatedTiers are the tiers ScoreTask compares a total against, in the order
// DeriveOverrides evaluates them. "trivial" is the score floor and has no
// gating threshold, so it is never nudged.
var gatedTiers = []string{"standard", "hard"}

// Overrides mirrors the writable routing-tunables schema that
// routing.ApplyOverrides consumes (see routing's overrides struct): tier
// gating thresholds, per-signal weights, and llm_assess bands. DeriveOverrides
// fills ONLY the keys it wants to change; the omitempty tags keep untouched
// sections out of the serialized file, and routing overlays only the keys the
// file actually names, so an override file round-trips as an incremental patch
// over the embedded defaults.
type Overrides struct {
	Thresholds    map[string]float64   `json:"thresholds,omitempty"`
	SignalWeights map[string]float64   `json:"signal_weights,omitempty"`
	AssessBands   map[string][]float64 `json:"assess_bands,omitempty"`
}

// Empty reports whether o carries no adjustments at all, i.e. deriving from a
// clean log produced a no-op. The MCP apply path uses this to report "nothing
// to apply" and skip writing (never clobbering an operator's existing file
// with an empty patch).
func (o Overrides) Empty() bool {
	return len(o.Thresholds) == 0 && len(o.SignalWeights) == 0 && len(o.AssessBands) == 0
}

// Marshal serializes o into the exact JSON shape routing.ApplyOverrides reads,
// indented for a human-editable overrides file. It is the counterpart to
// DeriveOverrides: derive → Marshal → write → ApplyOverrides round-trips.
func (o Overrides) Marshal() ([]byte, error) {
	return json.MarshalIndent(o, "", "  ")
}

// DeriveOverrides turns the report's actionable routing signals into a small,
// deterministic set of routing tunables that routing.ApplyOverrides can apply.
//
// Rule (deliberately conservative and single-directional):
//
//   - For each gated tier ("standard", "hard"), compute its non-ok outcome
//     rate exactly as buildSuggestions does. If the tier has at least minSample
//     dispatches AND its non-ok rate exceeds failureRateThreshold, nudge that
//     tier's gating threshold UP by thresholdNudge from the LIVE baseline
//     (routing.Thresholds()). Raising a tier's entry threshold makes promotion
//     into it stricter, so borderline tasks fall to the lower tier where a
//     different model handles them — a small correction for a tier whose
//     current occupants are underperforming.
//
//   - Everything else stays advisory only (surfaced in Suggestions, never a
//     concrete delta): a never-matched signal is a pattern-authoring question,
//     not a mechanical weight the report can set safely; the global
//     near-threshold count is not attributable to a single threshold; and
//     recurring critical/major review findings are an author-model quality
//     issue that routing cannot fix. Hence DeriveOverrides emits threshold
//     nudges only, and SignalWeights/AssessBands are left empty.
//
// A clean log (no flagged tier) yields an Empty() Overrides.
func (r *Report) DeriveOverrides() Overrides {
	var ov Overrides
	baseline := routing.Thresholds()

	for _, tier := range gatedTiers {
		statuses := r.StatusByTier[tier]
		total := 0
		nonOK := 0
		for status, n := range statuses {
			total += n
			if status != "ok" {
				nonOK += n
			}
		}
		if total < minSample {
			continue
		}
		if float64(nonOK)/float64(total) <= failureRateThreshold {
			continue
		}
		base, ok := baseline[tier]
		if !ok {
			continue
		}
		if ov.Thresholds == nil {
			ov.Thresholds = map[string]float64{}
		}
		ov.Thresholds[tier] = base + thresholdNudge
	}

	return ov
}
