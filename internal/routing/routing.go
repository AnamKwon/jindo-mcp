// Package routing ports jindo's deterministic multi-signal task scorer and
// model router to Go. It is a faithful, self-contained reimplementation of
// jindo/scoring.py (score_task) and jindo/router.py (route_for_difficulty): the
// scorer counts, per signal, the distinct patterns occurring at a leading
// word boundary in the lowercased task (capped at 2 per signal), weights each
// count, sums to a total, and buckets the total into a tier by the policy
// thresholds; the router resolves that tier to an agent+model via the models
// config.
//
// The policy and models JSON are embedded at build time (go:embed) so the
// package never reads the Python config path at runtime.
package routing

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

//go:embed config/models.json
var modelsJSON []byte

//go:embed config/routing_policy.json
var policyJSON []byte

// signal mirrors one entry of policy["signals"]: a weight applied to the count
// of distinct matched patterns. Extra policy blocks (llm_routing, dev_eval,
// capability_routing, ambiguous_band) are intentionally not modeled here; the
// scorer reads only "signals" and "thresholds", matching scoring.py.
type signal struct {
	Weight   float64  `json:"weight"`
	Patterns []string `json:"patterns"`
}

// policy is the parsed subset of routing_policy.json that the scorer consumes.
type policy struct {
	Signals    map[string]signal  `json:"signals"`
	Thresholds map[string]float64 `json:"thresholds"`
	LLMAssess  llmAssessConfig    `json:"llm_assess"`
}

// llmAssessConfig is the parsed policy["llm_assess"] block: whether optional
// near-threshold LLM re-judgment of difficulty is permitted at all, and the
// per-boundary total-score bands ("standard", "hard") within which a task is
// considered close enough to a threshold to be worth re-judging. Bands are
// [low, high] absolute totals, not offsets from the threshold, so they can be
// tuned independently of thresholds.standard/thresholds.hard.
type llmAssessConfig struct {
	Enabled bool                 `json:"enabled"`
	Bands   map[string][]float64 `json:"bands"`
}

// profile is one agent's capability profile: a per-signal strength (keyed by
// the signal NAMES from routing_policy.json — security/constraints/scope/
// ambiguity) applied as a weight over the matched signal contributions, plus a
// numeric cost_rank (lower = cheaper) used to break coverage ties toward the
// cheaper agent.
type profile struct {
	Strength map[string]float64 `json:"strength"`
	CostRank int                `json:"cost_rank"`
}

// legacyModel keeps an explicitly pinnable model resolvable after a routing
// slot advances to a newer default. It does not participate in automatic
// selection; it only preserves the explicit-model compatibility contract.
type legacyModel struct {
	Agent string `json:"agent"`
	Tier  string `json:"tier"`
}

// models is the parsed models.json: per-agent tier->model tables, the default
// agent per difficulty, and optional per-agent capability profiles that drive
// intra-tier agent selection.
type models struct {
	Agents                   map[string]map[string]string `json:"agents"`
	DefaultAgentByDifficulty map[string]string            `json:"default_agent_by_difficulty"`
	LegacyModels             map[string]legacyModel       `json:"legacy_models"`
	// EffortByDifficulty maps a difficulty tier (trivial/standard/hard) to the
	// reasoning-effort level dispatched with that tier's work by default (see
	// EffortForDifficulty). It is a separate dispatch dimension from the model —
	// the model is chosen by agent+tier, the effort by tier alone — so a caller
	// can tune how hard the SAME model thinks per difficulty. An absent/empty
	// entry yields "" (no effort flag), preserving the pre-effort behavior.
	EffortByDifficulty map[string]string  `json:"effort_by_difficulty"`
	Profiles           map[string]profile `json:"profiles"`
}

// Package-level parsed config, decoded once at init from the embedded bytes.
// A parse failure here is a build/asset bug, not a runtime input error, so it
// panics rather than being deferred to callers.
var (
	loadedPolicy policy
	loadedModels models

	// patternRegexps holds one compiled leading-word-boundary matcher per
	// distinct pattern string across all signals, keyed by the lowercased
	// pattern. Compiled once here (not per ScoreTask call) since the pattern
	// set is fixed for the process lifetime.
	patternRegexps map[string]*regexp.Regexp
)

func init() {
	if err := json.Unmarshal(policyJSON, &loadedPolicy); err != nil {
		panic(fmt.Sprintf("routing: cannot parse embedded routing_policy.json: %v", err))
	}
	if err := json.Unmarshal(modelsJSON, &loadedModels); err != nil {
		panic(fmt.Sprintf("routing: cannot parse embedded models.json: %v", err))
	}

	patternRegexps = make(map[string]*regexp.Regexp)
	for _, s := range loadedPolicy.Signals {
		for _, p := range s.Patterns {
			key := strings.ToLower(p)
			if _, ok := patternRegexps[key]; ok {
				continue
			}
			// \b anchors the match to the start of a word (preceded by
			// start-of-string or a non-word character); patterns are
			// intentional stems/prefixes ("concurren", "vulnerab",
			// "validat"), so no trailing boundary is required.
			patternRegexps[key] = regexp.MustCompile(`\b` + regexp.QuoteMeta(key))
		}
	}
}

// overrides is the writable subset of routing tunables that ApplyOverrides can
// overlay onto the embedded defaults at runtime. It is deliberately small: only
// the numeric knobs an operator tunes without a rebuild — the tier thresholds,
// per-signal weights, and the optional llm_assess bands. Each field is a map
// that stays nil when its key is absent from the file, so an override file
// overlays ONLY the keys it actually contains; any key it omits keeps its
// embedded default. Unknown top-level keys are ignored by encoding/json.
type overrides struct {
	Thresholds    map[string]float64   `json:"thresholds"`
	SignalWeights map[string]float64   `json:"signal_weights"`
	AssessBands   map[string][]float64 `json:"assess_bands"`
}

// clonePolicy returns a copy of p deep enough that overlaying overrides onto the
// result never mutates the argument: the Signals, Thresholds and llm_assess
// Bands maps are freshly allocated. Pattern slices inside each signal are shared
// (never mutated — only a signal's Weight is reassigned by replacing the whole
// signal value), matching what overrides can touch.
func clonePolicy(p policy) policy {
	out := p
	out.Signals = make(map[string]signal, len(p.Signals))
	for k, v := range p.Signals {
		out.Signals[k] = v
	}
	out.Thresholds = make(map[string]float64, len(p.Thresholds))
	for k, v := range p.Thresholds {
		out.Thresholds[k] = v
	}
	if p.LLMAssess.Bands != nil {
		out.LLMAssess.Bands = make(map[string][]float64, len(p.LLMAssess.Bands))
		for k, v := range p.LLMAssess.Bands {
			out.LLMAssess.Bands[k] = v
		}
	}
	return out
}

// ApplyOverrides overlays a small set of runtime-tunable routing knobs read from
// the JSON file at path onto the embedded defaults built in init(). It is the
// runtime-override layer: init() still produces the compile-time defaults, and
// this only adjusts the keys the file names.
//
// Contract:
//   - Absent file → nil (no-op; defaults untouched). This is the backward-compat
//     guarantee: a deployment with no overrides file behaves exactly as before.
//   - Only the keys present in the file are overlaid: thresholds[tier] onto
//     loadedPolicy.Thresholds[tier]; signal_weights[name] onto the Weight of an
//     EXISTING signal (unknown signal names are ignored); assess_bands[name] onto
//     loadedPolicy.LLMAssess.Bands[name]. Unknown top-level keys are ignored.
//   - Malformed JSON → non-nil error AND defaults left fully intact. The overlay
//     is built into a fresh clone of the current policy and committed only after
//     a successful parse, so a bad file never partially mutates the live config.
//
// No precomputed state needs rebuilding: patternRegexps is keyed by pattern
// string (which overrides never change), and ScoreTask reads weights and
// thresholds from loadedPolicy at call time, so a committed overlay is reflected
// immediately.
func ApplyOverrides(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("routing: read overrides %s: %w", path, err)
	}

	var ov overrides
	if err := json.Unmarshal(data, &ov); err != nil {
		return fmt.Errorf("routing: parse overrides %s: %w", path, err)
	}

	next := clonePolicy(loadedPolicy)
	for tier, v := range ov.Thresholds {
		next.Thresholds[tier] = v
	}
	for name, w := range ov.SignalWeights {
		s, ok := next.Signals[name]
		if !ok {
			continue
		}
		s.Weight = w
		next.Signals[name] = s
	}
	for name, b := range ov.AssessBands {
		if next.LLMAssess.Bands == nil {
			next.LLMAssess.Bands = make(map[string][]float64)
		}
		next.LLMAssess.Bands[name] = b
	}

	loadedPolicy = next
	return nil
}

// Score is the result of scoring a task: the per-signal weighted contributions,
// their sum, and the resulting tier. It mirrors the shape of scoring.py's
// score_task return (minus the human-readable reason, which is not part of the
// routing contract ported here).
type Score struct {
	Scores map[string]float64
	Total  float64
	Tier   string
}

// ScoreTask scores task deterministically against the embedded policy.
//
// Contract (verbatim from jindo/scoring.py score_task):
//
//	lowered = task.lower()
//	for each signal s in policy["signals"]:
//	    raw[s]   = # of DISTINCT patterns p in s["patterns"] that occur at a
//	               leading word boundary in lowered (preceded by
//	               start-of-string or a non-word character; patterns are
//	               stems/prefixes, so no trailing boundary is required),
//	               capped at 2 per signal
//	    score[s] = s.weight * raw[s]
//	total = sum(score)
//	tier  = "hard"     if total >= thresholds["hard"]
//	        "standard" elif total >= thresholds["standard"]
//	        "trivial"  otherwise
//
// The per-signal cap of 2 distinct matched patterns bounds how much a single
// signal can contribute regardless of how many of its patterns appear, so one
// keyword-stuffed signal cannot dominate the total on its own.
//
// Patterns are lowercased before matching. The config patterns are already
// lowercase, so this is behavior-preserving relative to the Python; it only
// guards against a config that stored an upper/mixed-case pattern.
func ScoreTask(task string) Score {
	lowered := strings.ToLower(task)

	const perSignalCap = 2

	scores := make(map[string]float64, len(loadedPolicy.Signals))
	var total float64
	for name, s := range loadedPolicy.Signals {
		matched := 0
		for _, p := range s.Patterns {
			if patternRegexps[strings.ToLower(p)].MatchString(lowered) {
				matched++
			}
		}
		if matched > perSignalCap {
			matched = perSignalCap
		}
		contribution := s.Weight * float64(matched)
		scores[name] = contribution
		total += contribution
	}

	tier := "trivial"
	if total >= loadedPolicy.Thresholds["hard"] {
		tier = "hard"
	} else if total >= loadedPolicy.Thresholds["standard"] {
		tier = "standard"
	}

	return Score{Scores: scores, Total: total, Tier: tier}
}

// AgentsModels returns the per-agent tier->model mapping from the embedded
// models config (agents -> difficulty -> model). It exposes the parsed
// models.json "agents" table for callers (e.g. the MCP "agents" tool) that
// need to enumerate which models each agent runs per difficulty. A fresh deep
// copy is returned so callers cannot mutate the package-level parsed config.
func AgentsModels() map[string]map[string]string {
	out := make(map[string]map[string]string, len(loadedModels.Agents))
	for agent, tiers := range loadedModels.Agents {
		cp := make(map[string]string, len(tiers))
		for tier, model := range tiers {
			cp[tier] = model
		}
		out[agent] = cp
	}
	return out
}

// EffortForDifficulty returns the configured reasoning-effort level for a
// difficulty tier from the embedded models config (effort_by_difficulty ->
// tier), or "" when the tier has no configured effort (including an unknown
// tier or an absent effort_by_difficulty block). Returning "" on the miss path
// is the backward-compat contract: an empty effort means "add no effort flag",
// so a config without effort_by_difficulty behaves exactly as before. It
// mirrors the AgentsModels/Thresholds accessor style — a plain read of the
// package-level parsed config, safe on any input.
func EffortForDifficulty(difficulty string) string {
	return loadedModels.EffortByDifficulty[difficulty]
}

// AssessBands returns the llm_assess.bands ranges from the embedded routing
// policy ([low, high] absolute totals, keyed by boundary name — "standard",
// "hard"; see llmAssessConfig). It is the source of truth for which
// rationale.total values count as near-threshold, exposed for callers (e.g.
// internal/calibrate) that need to replicate that classification without
// duplicating the policy values by hand. A fresh copy is returned so callers
// cannot mutate the package-level parsed config. Malformed bands (not exactly
// 2 values, matching inAssessBand's own leniency) are skipped.
func AssessBands() map[string][2]float64 {
	out := make(map[string][2]float64, len(loadedPolicy.LLMAssess.Bands))
	for name, b := range loadedPolicy.LLMAssess.Bands {
		if len(b) != 2 {
			continue
		}
		out[name] = [2]float64{b[0], b[1]}
	}
	return out
}

// Thresholds returns the tier gating thresholds from the current routing
// policy (the minimum total score at which ScoreTask promotes a task into that
// tier — e.g. "standard", "hard"; "trivial" is the floor and has no entry). It
// reflects any overlay committed by ApplyOverrides, so callers read the LIVE
// baseline rather than the compile-time default. It is exposed for callers
// (e.g. internal/calibrate) that need the current threshold values to derive
// conservative adjustments without hand-duplicating the policy. A fresh copy is
// returned so callers cannot mutate the package-level parsed config.
func Thresholds() map[string]float64 {
	out := make(map[string]float64, len(loadedPolicy.Thresholds))
	for name, v := range loadedPolicy.Thresholds {
		out[name] = v
	}
	return out
}

// KnownSignals returns the names of every signal in the embedded routing
// policy's "signals" block (e.g. security, constraints, scope, ambiguity). It
// is the source of truth for which signals exist, exposed for callers (e.g.
// internal/calibrate) that need to detect signals which never fired without
// duplicating the signal table by hand. A fresh copy is returned so callers
// cannot mutate the package-level parsed config.
func KnownSignals() []string {
	out := make([]string, 0, len(loadedPolicy.Signals))
	for name := range loadedPolicy.Signals {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Rationale explains WHY a task landed in its tier. It is derived from the real
// ScoreTask result (not a reconstruction), so it always reflects the actual task
// input:
//
//   - Matched: the per-signal weighted contribution, restricted to signals that
//     actually matched (contribution > 0). Signals that scored 0 are omitted so
//     the map records only what drove the decision.
//   - Total: the summed contribution across all signals (== ScoreTask.Total).
//   - Threshold / ThresholdValue: the name and value of the threshold that
//     admitted the resulting tier. For "hard"/"standard" this is the crossed
//     policy threshold; for "trivial" no positive threshold was crossed, so the
//     name is "trivial" and the value is 0.
//   - Tier: the resulting difficulty tier (== ScoreTask.Tier).
type Rationale struct {
	Matched        map[string]float64 `json:"matched"`
	Total          float64            `json:"total"`
	Threshold      string             `json:"threshold"`
	ThresholdValue float64            `json:"threshold_value"`
	Tier           string             `json:"tier"`
	// ProfileMatch records HOW the agent was chosen within the tier (see
	// ProfileMatch). It is additive to the existing rationale fields (which are
	// unchanged for MCP/log backward compatibility) and is populated by Select
	// on every path — coverage selection, explicit override, or no-profiles
	// fallback — so the log always explains the agent choice. It is a pointer
	// with omitempty so a zero-value Rationale (e.g. a stubbed one that never
	// went through Select) serializes exactly as before.
	ProfileMatch *ProfileMatch `json:"profile_match,omitempty"`
	// Priority records the priority hint applied to profile selection
	// ("cost", "quality", or "latency"), when one was supplied to Select.
	// Omitted (omitempty) for ""/unknown so existing rationale JSON for
	// callers that never pass a priority is unchanged.
	Priority string `json:"priority,omitempty"`
	// LLMAssessed records whether Assessor was consulted and its override
	// accepted, replacing the deterministic ScoreTask tier with the one now
	// reported in Tier. omitempty: when llm_assess is disabled (the default)
	// this field never appears, so existing rationale JSON is byte-identical.
	LLMAssessed bool `json:"llm_assessed,omitempty"`
}

// ProfileCandidate is one agent considered during intra-tier profile selection:
// its weighted coverage of the matched signals and its cost_rank (lower =
// cheaper). Candidates are the agents that have a model slot for the tier.
type ProfileCandidate struct {
	Agent    string  `json:"agent"`
	Coverage float64 `json:"coverage"`
	CostRank int     `json:"cost_rank"`
}

// ProfileMatch explains the agent selection within a difficulty tier:
//
//   - Candidates: per-agent coverage/cost breakdown (only populated on the
//     coverage-selection path; empty for override/fallback where no scoring ran).
//   - Chosen: the agent Select resolved to.
//   - Reason: a short human-readable why-chosen string. For coverage selection
//     it states the tie rule; for an explicit agent it records the override; for
//     the no-profiles path it records the fallback to default_agent_by_difficulty.
type ProfileMatch struct {
	Candidates []ProfileCandidate `json:"candidates"`
	Chosen     string             `json:"chosen"`
	Reason     string             `json:"reason"`
}

// rationaleFor projects a Score into a Rationale: it keeps only the signals that
// actually matched and resolves the threshold that admitted the tier. Keeping
// this derivation next to Score guarantees the rationale equals what was
// computed from the real task input.
func rationaleFor(s Score) Rationale {
	matched := make(map[string]float64, len(s.Scores))
	for name, contribution := range s.Scores {
		if contribution > 0 {
			matched[name] = contribution
		}
	}
	// The applied threshold is the one classifying the resulting tier. "hard" and
	// "standard" are admitted by crossing their policy threshold; "trivial" is the
	// fallthrough when neither is crossed, so it has no positive threshold value.
	thresholdName := s.Tier
	var thresholdValue float64
	if v, ok := loadedPolicy.Thresholds[s.Tier]; ok {
		thresholdValue = v
	}
	return Rationale{
		Matched:        matched,
		Total:          s.Total,
		Threshold:      thresholdName,
		ThresholdValue: thresholdValue,
		Tier:           s.Tier,
	}
}

// Assessor, when non-nil, lets a caller re-judge a task's difficulty tier
// near a scoring threshold using an LLM (or any other out-of-band signal)
// instead of the deterministic ScoreTask bucketing alone. It is a
// package-level seam (nil by default) rather than a Select parameter because
// wiring an LLM call through every routing.Select call site would require
// threading a CLI/model dependency the deterministic scorer has never had;
// tests and callers that want assessment set this var directly.
//
// ok reports whether the assessment produced a usable tier; when ok is false,
// or tier is not one of "trivial"/"standard"/"hard", Select ignores the
// result and keeps the deterministic ScoreTask tier. This mirrors ScoreTask's
// own contract of always returning a tier from a closed set.
var Assessor func(task string, score Score) (tier string, ok bool)

// AgentAvailable, when non-nil, reports whether a given agent's CLI is actually
// installed (resolvable on PATH). It is a package-level seam (nil by default)
// that mirrors Assessor: routing must NOT import internal/agent (that would be
// an import cycle, since agent-side code routes), so the availability probe is
// injected here — cmd/jindo-mcp wires it to agent.Available at startup.
//
// nil ⇒ every agent is treated as usable, reproducing the pre-availability
// behavior exactly. This is the backward-compat guarantee for tests and library
// callers that never set the seam: routing still picks from the full table.
var AgentAvailable func(name string) bool

// agentUsable reports whether the named agent may be selected. With the seam
// unset (nil), every agent is usable (backward compat); otherwise it defers to
// the injected probe. It is the single gate every selection point consults, so
// "installed?" is asked in exactly one place.
func agentUsable(name string) bool {
	return AgentAvailable == nil || AgentAvailable(name)
}

// AgentAvailability returns, for every agent in the embedded models table,
// whether it is currently usable (see agentUsable). It is exposed for the tool
// layer (the MCP "agents" tool) to report install availability alongside the
// model table. With the seam unset every entry is true. A fresh map is returned
// so callers cannot mutate package state.
func AgentAvailability() map[string]bool {
	out := make(map[string]bool, len(loadedModels.Agents))
	for agent := range loadedModels.Agents {
		out[agent] = agentUsable(agent)
	}
	return out
}

// llmAssessEnvVar gates Assessor invocation in addition to the policy's
// llm_assess.enabled flag (defense-in-depth double gate): even a policy
// config that enables assessment does nothing unless the process explicitly
// opts in via this environment variable, so assessment can never silently
// activate from config alone (e.g. a shared/checked-in policy file).
const llmAssessEnvVar = "JINDO_LLM_ASSESS"

// validAssessedTier reports whether tier is one of the three tiers ScoreTask
// can produce; an assessment result outside this set is treated the same as
// !ok (fall back to the deterministic tier).
func validAssessedTier(tier string) bool {
	switch tier {
	case "trivial", "standard", "hard":
		return true
	}
	return false
}

// inAssessBand reports whether total falls inside any configured
// llm_assess.bands range ([low, high], inclusive). A malformed band (not
// exactly 2 values) is skipped rather than erroring, since llm_assess is an
// optional, disabled-by-default feature and a bad band should not break
// routing.
func inAssessBand(total float64, bands map[string][]float64) bool {
	for _, b := range bands {
		if len(b) != 2 {
			continue
		}
		if total >= b[0] && total <= b[1] {
			return true
		}
	}
	return false
}

// Selection is the routing decision for a task: the resolved agent, its model
// for the scored difficulty, that difficulty tier, and the Rationale explaining
// why the task scored into that tier.
type Selection struct {
	Agent      string
	Model      string
	Difficulty string
	Rationale  Rationale
}

// chooseByProfile picks, among the candidate agents for a difficulty (those
// that have a model slot for it), the one whose capability profile best covers
// the matched signals. Coverage is the weighted dot product of each matched
// signal's contribution with that agent's strength for the same signal:
//
//	coverage(agent) = sum over matched signals s of matched[s] * profile[agent].strength[s]
//
// The best candidate has the highest coverage; ties break toward the lower
// cost_rank (cheaper), then toward the lexicographically smaller agent name so
// the result is deterministic. When NO signals matched (trivial tasks), every
// coverage is 0, so the choice reduces to the cheapest candidate — preserving
// trivial -> cheapest agent.
//
// A candidate with no profile entry gets coverage 0 and is treated as
// maximally expensive (least preferred on ties), so a partial profiles config
// never silently promotes an unprofiled agent. It returns the chosen agent and
// the per-candidate breakdown; if the tier has no candidate agent it returns "".
//
// priority reweights which axis dominates the comparison (see betterCandidate
// for the exact ordering per value); "" behaves exactly as before.
//
// exclude, when non-empty, removes that single agent from the candidate set
// before scoring — used by SelectReviewer to guarantee a reviewer distinct
// from the author without duplicating this selection loop. "" includes every
// candidate, reproducing Select's behavior exactly.
func chooseByProfile(difficulty string, matched map[string]float64, priority string, exclude string) (string, []ProfileCandidate) {
	names := make([]string, 0, len(loadedModels.Agents))
	for agent, tiers := range loadedModels.Agents {
		if agent == exclude {
			continue
		}
		if !agentUsable(agent) {
			continue
		}
		if _, ok := tiers[difficulty]; ok {
			names = append(names, agent)
		}
	}
	sort.Strings(names)

	cands := make([]ProfileCandidate, 0, len(names))
	for _, agent := range names {
		prof, hasProfile := loadedModels.Profiles[agent]
		var coverage float64
		for sig, contribution := range matched {
			coverage += contribution * prof.Strength[sig]
		}
		costRank := prof.CostRank
		if !hasProfile {
			costRank = math.MaxInt
		}
		cands = append(cands, ProfileCandidate{Agent: agent, Coverage: coverage, CostRank: costRank})
	}

	best := -1
	for i := range cands {
		if best < 0 || betterCandidate(cands[i], cands[best], priority) {
			best = i
		}
	}
	if best < 0 {
		return "", cands
	}
	return cands[best].Agent, cands
}

// betterCandidate reports whether a should be preferred over b, per priority:
//
//   - "" (default) / "quality": higher coverage wins first (quality names this
//     explicitly — signal coverage dominates — but it is the same ordering the
//     package always used); on equal coverage the lower cost_rank wins; ties
//     break on agent name.
//   - "cost": lower cost_rank wins first — the cheapest capable candidate is
//     chosen regardless of coverage; on equal cost the higher coverage wins;
//     ties break on agent name.
//   - "latency": same ordering as "cost". The profile has no dedicated latency
//     axis, so cost_rank is reused as a proxy (cheaper models are, in this
//     config, also the faster/lighter ones) rather than inventing a new field
//     for one caller.
//
// Any other/unknown priority value falls back to the default ordering.
func betterCandidate(a, b ProfileCandidate, priority string) bool {
	switch priority {
	case "cost", "latency":
		if a.CostRank != b.CostRank {
			return a.CostRank < b.CostRank
		}
		if a.Coverage != b.Coverage {
			return a.Coverage > b.Coverage
		}
		return a.Agent < b.Agent
	default:
		if a.Coverage != b.Coverage {
			return a.Coverage > b.Coverage
		}
		if a.CostRank != b.CostRank {
			return a.CostRank < b.CostRank
		}
		return a.Agent < b.Agent
	}
}

// profileReason returns the short why-chosen string for the coverage-selection
// path, distinguishing the no-signal case (cheapest wins) from the normal
// case, and noting the priority axis when one was applied.
func profileReason(matched map[string]float64, priority string) string {
	names := make([]string, 0, len(matched))
	for n := range matched {
		names = append(names, n)
	}
	sort.Strings(names)
	matchedList := strings.Join(names, " ")

	if priority == "cost" || priority == "latency" {
		axis := "priority=cost: cheapest candidate by cost_rank dominates"
		if priority == "latency" {
			axis = "priority=latency: fastest candidate dominates, using cost_rank as a proxy"
		}
		if len(matched) == 0 {
			return axis + "; no signals matched"
		}
		return axis + "; matched signals [" + matchedList + "] used only to break cost ties"
	}

	if len(matched) == 0 {
		return "no signals matched; chose cheapest candidate by cost_rank"
	}
	suffix := ""
	if priority == "quality" {
		suffix = " (priority=quality: signal coverage dominates)"
	}
	return "highest weighted profile coverage of matched signals [" +
		matchedList + "], ties broken by lower cost_rank" + suffix
}

// Select is the backward-compatible entry point: it is exactly
// SelectModel(task, agent, priority, "") — score-based routing with no explicit
// model pin. See SelectModel for the full contract.
func Select(task string, agent string, priority string) (Selection, error) {
	return SelectModel(task, agent, priority, "")
}

// SelectModel scores task, resolves the tier to an agent+model, and returns the
// decision. It extends router.py's route_for_difficulty applied to
// ScoreTask(task).Tier with intra-tier profile selection:
//
//   - difficulty := ScoreTask(task).Tier
//   - if agent != "": honor it verbatim (explicit override wins, exactly as
//     before — no profile selection).
//   - else if profiles are configured: agent := chooseByProfile(difficulty,
//     matched signals) — the candidate whose profile best covers the matched
//     signals (cheapest on ties / when nothing matched).
//   - else (no profiles): agent := default_agent_by_difficulty[difficulty]
//     (the prior, safe behavior).
//   - model := agents[agent][difficulty]
//
// Rationale.ProfileMatch is populated on every path so the log always explains
// the agent choice.
//
// priority is an optional hint ("cost", "quality", "latency", or "") that
// reweights intra-tier profile selection (see betterCandidate for the exact
// per-value ordering). It has no effect on the explicit-agent-override path
// (agent != "" always wins unchanged) or when no profiles are configured.
// ""/unknown reproduces exactly the pre-priority weighting.
//
// model is an optional EXACT model pin. When model != "", score-based agent and
// model selection is bypassed entirely: the given model is run verbatim and the
// agent is either the one given (validated to exist, but NOT required to list
// model in its tier table — an explicit (agent, model) pair is trusted so
// callers can run unlisted models) or, when agent == "", the unique agent whose
// tier table contains model as a value. The reported Difficulty is the tier
// whose model slot equals model, or the literal "explicit" when model is not in
// the resolved agent's table. When model == "", behavior is byte-identical to
// the pre-pin routing (Select).
//
// Unlike the Python (which raises), SelectModel returns an error when the agent
// is unknown, cannot be inferred from model, or has no model slot for the
// difficulty, so callers handle the misconfiguration explicitly.
func SelectModel(task string, agent string, priority string, model string) (Selection, error) {
	score := ScoreTask(task)
	rationale := rationaleFor(score)

	difficulty := score.Tier
	if loadedPolicy.LLMAssess.Enabled && os.Getenv(llmAssessEnvVar) != "" &&
		Assessor != nil && inAssessBand(score.Total, loadedPolicy.LLMAssess.Bands) {
		if tier, ok := Assessor(task, score); ok && validAssessedTier(tier) {
			difficulty = tier
			rationale.Tier = tier
			rationale.LLMAssessed = true
		}
	}

	switch priority {
	case "cost", "quality", "latency":
		rationale.Priority = priority
	}

	if model != "" {
		// Explicit model pin: bypass scoring-based agent/model selection.
		var resolvedAgent string
		if agent != "" {
			// Trust the explicit (agent, model) pair: validate only that the
			// agent exists, NOT that model is one of its listed tier slots, so
			// callers can run models not in the table.
			if _, ok := loadedModels.Agents[agent]; !ok {
				return Selection{}, fmt.Errorf("routing: unknown agent %q", agent)
			}
			resolvedAgent = agent
		} else {
			// Reverse-lookup the unique agent whose tier table lists model as a
			// value. Model ids are distinct per agent, so at most one matches;
			// on the (config-bug) chance of several, pick the lexicographically
			// smallest agent so the result is deterministic.
			var matches []string
			for a, tiers := range loadedModels.Agents {
				for _, m := range tiers {
					if m == model {
						matches = append(matches, a)
						break
					}
				}
			}
			if len(matches) == 0 {
				legacy, ok := loadedModels.LegacyModels[model]
				if !ok {
					return Selection{}, fmt.Errorf("routing: cannot infer agent for model %q; pass agent explicitly", model)
				}
				matches = append(matches, legacy.Agent)
			}
			sort.Strings(matches)
			resolvedAgent = matches[0]
		}

		// A pinned model is worthless if its agent's CLI is not installed: the
		// dispatch would fail with "command not found". Surface it as a clear
		// routing error instead (there is no fallback for an explicit pin).
		if !agentUsable(resolvedAgent) {
			return Selection{}, fmt.Errorf("routing: agent %q (for model %q) is not installed", resolvedAgent, model)
		}

		// Difficulty LABEL for logging: the tier whose model slot equals model,
		// else the literal "explicit". Iterate tiers in sorted order so a
		// (degenerate) duplicate-model table still yields a deterministic label.
		label := "explicit"
		tiers := loadedModels.Agents[resolvedAgent]
		tierNames := make([]string, 0, len(tiers))
		for tier := range tiers {
			tierNames = append(tierNames, tier)
		}
		sort.Strings(tierNames)
		for _, tier := range tierNames {
			if tiers[tier] == model {
				label = tier
				break
			}
		}
		if label == "explicit" {
			if legacy, ok := loadedModels.LegacyModels[model]; ok && legacy.Agent == resolvedAgent {
				label = legacy.Tier
			}
		}
		rationale.Tier = label
		rationale.ProfileMatch = &ProfileMatch{
			Chosen: resolvedAgent,
			Reason: "explicit model override; scoring bypassed",
		}
		return Selection{
			Agent:      resolvedAgent,
			Model:      model,
			Difficulty: label,
			Rationale:  rationale,
		}, nil
	}

	switch {
	case agent != "":
		// Explicit override wins exactly as before; profile selection skipped.
		// But an override naming an uninstalled agent cannot be honored, and
		// (unlike the score/profile paths) there is nothing to fall back to, so
		// surface a clear error rather than dispatching to a missing CLI.
		if !agentUsable(agent) {
			return Selection{}, fmt.Errorf("routing: agent %q is not installed", agent)
		}
		rationale.ProfileMatch = &ProfileMatch{
			Chosen: agent,
			Reason: "explicit agent override; profile selection skipped",
		}
	case len(loadedModels.Profiles) > 0:
		chosen, cands := chooseByProfile(difficulty, rationale.Matched, priority, "")
		if chosen == "" {
			return Selection{}, fmt.Errorf("routing: no candidate agent for difficulty %q", difficulty)
		}
		agent = chosen
		rationale.ProfileMatch = &ProfileMatch{
			Candidates: cands,
			Chosen:     chosen,
			Reason:     profileReason(rationale.Matched, priority),
		}
	default:
		// No profiles configured: fall back to the documented default agent.
		def, ok := loadedModels.DefaultAgentByDifficulty[difficulty]
		if !ok {
			return Selection{}, fmt.Errorf("routing: no default agent for difficulty %q", difficulty)
		}
		if agentUsable(def) {
			agent = def
			rationale.ProfileMatch = &ProfileMatch{
				Chosen: agent,
				Reason: fmt.Sprintf("no profiles configured; fell back to default_agent_by_difficulty[%q]=%q", difficulty, agent),
			}
			break
		}
		// The default agent is not installed: fall back to profile-style
		// coverage selection over the USABLE candidates (chooseByProfile now
		// filters them), so a missing default degrades gracefully to an
		// available agent instead of erroring outright.
		chosen, cands := chooseByProfile(difficulty, rationale.Matched, priority, "")
		if chosen == "" {
			return Selection{}, fmt.Errorf("routing: no installed agent for difficulty %q", difficulty)
		}
		agent = chosen
		rationale.ProfileMatch = &ProfileMatch{
			Candidates: cands,
			Chosen:     chosen,
			Reason:     fmt.Sprintf("default_agent_by_difficulty[%q]=%q not installed; fell back to installed candidate %q", difficulty, def, chosen),
		}
	}

	tierModels, ok := loadedModels.Agents[agent]
	if !ok {
		return Selection{}, fmt.Errorf("routing: unknown agent %q", agent)
	}
	model, ok = tierModels[difficulty]
	if !ok {
		return Selection{}, fmt.Errorf("routing: agent %q has no model for difficulty %q", agent, difficulty)
	}

	return Selection{
		Agent:      agent,
		Model:      model,
		Difficulty: difficulty,
		Rationale:  rationale,
	}, nil
}

// SelectReviewer scores task exactly as Select does (ScoreTask + rationaleFor)
// but resolves the agent via chooseByProfile over the tier's candidates MINUS
// excludeAuthor, guaranteeing the returned agent differs from the author that
// produced the work under review — a cross-model reviewer, not a
// self-review. It shares chooseByProfile/betterCandidate with Select rather
// than duplicating the coverage-scoring loop.
//
// priority is the same optional hint ("cost", "quality", "latency", or "")
// accepted by Select, with the same effect on tie-breaking.
//
// Unlike Select, SelectReviewer has no explicit-agent-override path: the
// whole point of this API is to pick a reviewer other than a caller-known
// author, so honoring an override would defeat the guarantee.
//
// It returns an error if excludeAuthor is the only agent with a model slot
// for the scored difficulty (no cross-model reviewer available), or if the
// resolved agent/difficulty has no model slot.
func SelectReviewer(task string, priority string, excludeAuthor string) (Selection, error) {
	score := ScoreTask(task)
	rationale := rationaleFor(score)
	difficulty := score.Tier

	switch priority {
	case "cost", "quality", "latency":
		rationale.Priority = priority
	}

	chosen, cands := chooseByProfile(difficulty, rationale.Matched, priority, excludeAuthor)
	if chosen == "" {
		return Selection{}, fmt.Errorf("routing: no cross-model reviewer available excluding %q", excludeAuthor)
	}

	rationale.ProfileMatch = &ProfileMatch{
		Candidates: cands,
		Chosen:     chosen,
		Reason:     "reviewer selection (author " + excludeAuthor + " excluded): " + profileReason(rationale.Matched, priority),
	}

	tierModels, ok := loadedModels.Agents[chosen]
	if !ok {
		return Selection{}, fmt.Errorf("routing: unknown agent %q", chosen)
	}
	model, ok := tierModels[difficulty]
	if !ok {
		return Selection{}, fmt.Errorf("routing: agent %q has no model for difficulty %q", chosen, difficulty)
	}

	return Selection{
		Agent:      chosen,
		Model:      model,
		Difficulty: difficulty,
		Rationale:  rationale,
	}, nil
}

// SelectReviewers enumerates EVERY cross-model reviewer for a task: for each
// agent (other than excludeAuthor) that has a model slot for the task's scored
// difficulty, it returns a Selection pinned to that agent's model for the tier.
// Where SelectReviewer picks the single best-covering reviewer, SelectReviewers
// fans out to all of them so the caller can review an author's result
// concurrently with every other model.
//
// Difficulty and the priority rationale handling mirror SelectReviewer exactly
// (ScoreTask + rationaleFor, with the priority hint recorded on the rationale),
// so a fan-out reviewer's routing "why" matches a single-reviewer selection for
// the same task. Every returned Selection carries the same base rationale.
//
// Agents are iterated in SORTED name order so the returned slice is
// deterministic. It errors if no agent other than excludeAuthor has a model
// slot for the difficulty (no cross-model reviewers available).
func SelectReviewers(task string, priority string, excludeAuthor string) ([]Selection, error) {
	score := ScoreTask(task)
	rationale := rationaleFor(score)
	difficulty := score.Tier

	switch priority {
	case "cost", "quality", "latency":
		rationale.Priority = priority
	}

	names := make([]string, 0, len(loadedModels.Agents))
	for agent := range loadedModels.Agents {
		names = append(names, agent)
	}
	sort.Strings(names)

	sels := make([]Selection, 0, len(names))
	for _, agent := range names {
		if agent == excludeAuthor {
			continue
		}
		if !agentUsable(agent) {
			continue
		}
		model, ok := loadedModels.Agents[agent][difficulty]
		if !ok {
			continue
		}
		sels = append(sels, Selection{
			Agent:      agent,
			Model:      model,
			Difficulty: difficulty,
			Rationale:  rationale,
		})
	}
	if len(sels) == 0 {
		return nil, fmt.Errorf("routing: no cross-model reviewers available excluding %q", excludeAuthor)
	}
	return sels, nil
}
