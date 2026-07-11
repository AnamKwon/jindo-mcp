// Package modelscan discovers, at runtime, which models each installed
// coding-agent CLI actually exposes, and proposes how a newly-seen model
// SHOULD be routed. It is an isolated, read-only observability layer: it never
// touches the routing engine. The static routing table (internal/routing) is
// the source of truth for how work is dispatched; modelscan only reports the
// gap between that table and what the CLIs currently offer, plus HEURISTIC tier
// proposals for a human (or the routing owner) to review. Nothing here is ever
// auto-applied to routing.
//
// Each CLI is probed with the strategy that CLI actually supports:
//   - agy:    `agy models` enumerates every model (one per line) -> full list.
//   - codex:  no list command; `codex doctor` reports the ACTIVE model on a
//     "model  <id> · <provider>" line -> that one model only.
//   - claude: no model-list command at all -> nothing discoverable.
//
// A probe failure is best-effort: the affected agent is reported with Source
// "error" and an empty Available list; it never fails the whole Probe.
package modelscan

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"jindo/internal/routing"
)

// AgentModels is one agent's discovered model surface. Source records HOW the
// list was obtained so a reader can judge its completeness:
//
//	enumerated  — the CLI listed all its models (agy)
//	active-only — only the currently-active model is knowable (codex)
//	none        — the CLI exposes no model discovery at all (claude)
//	error       — the probe failed; Available is empty (best-effort)
type AgentModels struct {
	Agent     string   `json:"agent"`
	Available []string `json:"available"`
	Active    string   `json:"active,omitempty"`
	Source    string   `json:"source"`
}

// Proposal is a HEURISTIC, advisory routing suggestion for a discovered model
// that is not yet in the static routing table. It is NEVER auto-applied: New is
// true for models the routing owner has not yet placed, and ProposedTier /
// ProposedEffort / Reason exist purely to speed a human review.
type Proposal struct {
	Model          string `json:"model"`
	Agent          string `json:"agent"`
	ProposedTier   string `json:"proposed_tier"`
	ProposedEffort string `json:"proposed_effort"`
	Reason         string `json:"reason"`
	New            bool   `json:"new"`
}

// Inventory is the full Refresh result: what each agent exposes plus the
// advisory proposals for models missing from the routing table.
type Inventory struct {
	Agents    []AgentModels `json:"agents"`
	Proposals []Proposal    `json:"proposals"`
}

// probeTimeout bounds each CLI probe so a hung/slow CLI cannot block the MCP
// request. It mirrors the exec-with-deadline discipline used by the verify
// gate (internal/orchestrator/verify.go).
var probeTimeout = 15 * time.Second

// runCmd is the single exec seam every CLI probe goes through, so tests can
// substitute canned outputs and never spawn a real CLI. It runs the program
// directly via exec.CommandContext (NO shell) under probeTimeout, returning its
// stdout. A non-nil error (including a timeout) is surfaced to the caller, which
// degrades that agent's probe to Source "error".
var runCmd = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// Probe interrogates every known agent CLI for its available models and returns
// the results in stable agent order [agy, claude, codex] (sorted by name) so
// the output/tests are deterministic. A failing probe yields Source "error"
// with an empty Available list and never aborts the others.
func Probe() []AgentModels {
	return []AgentModels{
		probeAgy(),
		probeClaude(),
		probeCodex(),
	}
}

// probeAgy runs `agy models`, which prints one model name per line. Every
// non-empty trimmed line is a model, so this is a FULL enumeration (Source
// "enumerated"). A probe error degrades to Source "error".
func probeAgy() AgentModels {
	out, err := runCmd("agy", "models")
	if err != nil {
		return AgentModels{Agent: "agy", Source: "error"}
	}
	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		if m := strings.TrimSpace(line); m != "" {
			models = append(models, m)
		}
	}
	return AgentModels{Agent: "agy", Available: models, Source: "enumerated"}
}

// probeClaude reports the claude agent: its CLI has no model-list command, so
// nothing is discoverable (Available nil, Source "none"). No CLI is invoked.
func probeClaude() AgentModels {
	return AgentModels{Agent: "claude", Available: nil, Source: "none"}
}

// probeCodex runs `codex doctor` and scans its output for the active-model
// line, shaped like `      model                    gpt-5.5 · openai`. The
// model id is the token after "model" and before the "·" provider separator.
// codex has no enumeration command, so only the active model is knowable
// (Source "active-only", Available = [active]). If the line is absent the probe
// is treated as failed (Source "error"); a non-zero exit likewise degrades to
// "error".
func probeCodex() AgentModels {
	out, err := runCmd("codex", "doctor")
	if err != nil {
		return AgentModels{Agent: "codex", Source: "error"}
	}
	active := parseCodexActiveModel(string(out))
	if active == "" {
		return AgentModels{Agent: "codex", Source: "error"}
	}
	return AgentModels{
		Agent:     "codex",
		Available: []string{active},
		Active:    active,
		Source:    "active-only",
	}
}

// parseCodexActiveModel extracts the active model id from `codex doctor`
// output. It finds a line whose first field is "model", then returns the next
// field, stripped of the " · <provider>" suffix. Returns "" when no such line
// exists.
func parseCodexActiveModel(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "model" {
			// fields[1] is the model id; anything after (e.g. "·", "openai")
			// is the provider annotation, which we drop.
			return fields[1]
		}
	}
	return ""
}

// trivialKeywords and hardKeywords drive the name-based tier HEURISTIC in
// Classify. They are matched case-insensitively as substrings, trivial FIRST,
// then hard, so the first list to match wins; anything else falls to
// "standard". These are deliberately advisory signals (model family / size /
// reasoning-mode hints commonly encoded in display names), NOT a routing
// decision — see Classify.
var (
	trivialKeywords = []string{"flash", "mini", "nano", "haiku", "lite", "oss", "small", "8b"}
	hardKeywords    = []string{"opus", "gpt-5.5", "pro (high", "ultra", "thinking", "120b", "-max", " high)"}
)

// Classify produces advisory routing Proposals for every discovered model that
// is NOT already listed for its agent in the reference table (agent -> tier ->
// model). A model already in the table is skipped entirely (it is already
// routed, so there is nothing to propose).
//
// The tier is a pure NAME HEURISTIC: the model name is lowercased and checked
// against trivialKeywords first, then hardKeywords (first match wins), else
// "standard". The matched keyword is recorded in Reason. ProposedEffort is the
// routing engine's own effort for that tier (routing.EffortForDifficulty), so a
// reviewer sees the effort the tier would dispatch with.
//
// IMPORTANT: this is a HEURISTIC PROPOSAL for human / routing-owner review. It
// is never auto-applied to routing; New=true simply flags "not yet in the
// table". Misclassification here changes nothing about how work is dispatched.
func Classify(agents []AgentModels, reference map[string]map[string]string) []Proposal {
	var proposals []Proposal
	for _, am := range agents {
		known := knownModels(reference[am.Agent])
		for _, model := range am.Available {
			if known[model] {
				continue // already routed; nothing to propose
			}
			tier, keyword := heuristicTier(model)
			proposals = append(proposals, Proposal{
				Model:          model,
				Agent:          am.Agent,
				ProposedTier:   tier,
				ProposedEffort: routing.EffortForDifficulty(tier),
				Reason:         proposalReason(tier, keyword),
				New:            true,
			})
		}
	}
	return proposals
}

// knownModels collapses an agent's tier->model reference row into a set of the
// model ids it already lists, for O(1) "is this model already routed?" checks.
func knownModels(tiers map[string]string) map[string]bool {
	set := make(map[string]bool, len(tiers))
	for _, model := range tiers {
		set[model] = true
	}
	return set
}

// heuristicTier maps a model NAME to a proposed tier and returns the keyword
// that decided it. Matching is case-insensitive substring, trivial keywords
// first then hard keywords (first match wins), else "standard" with no keyword.
func heuristicTier(model string) (tier, keyword string) {
	lowered := strings.ToLower(model)
	for _, kw := range trivialKeywords {
		if strings.Contains(lowered, kw) {
			return "trivial", kw
		}
	}
	for _, kw := range hardKeywords {
		if strings.Contains(lowered, kw) {
			return "hard", kw
		}
	}
	return "standard", ""
}

// proposalReason builds the human-facing why-string, naming the keyword that
// drove a trivial/hard proposal or explaining the standard fallthrough.
func proposalReason(tier, keyword string) string {
	if keyword == "" {
		return "heuristic proposal (advisory, for routing-owner review): no trivial/hard keyword matched -> standard"
	}
	return "heuristic proposal (advisory, for routing-owner review): name matched " + tier + " keyword \"" + keyword + "\""
}

// Refresh is the convenience entry point used by the MCP tool: probe every
// agent, then classify the discovered models against the LIVE routing table.
// It is read-only end to end.
func Refresh() Inventory {
	agents := Probe()
	return Inventory{
		Agents:    agents,
		Proposals: Classify(agents, routing.AgentsModels()),
	}
}
