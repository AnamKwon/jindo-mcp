// Package modelscan discovers, at runtime, which models each installed
// coding-agent CLI actually exposes, and identifies newly seen models that need
// assessment. It is an isolated, read-only observability layer: it never
// touches the routing engine. The static routing table (internal/routing) is
// the source of truth for legacy no-capability dispatch; modelscan only reports
// the gap between that table and what the CLIs currently expose. A newly seen
// model is explicitly unmeasured: its name is never converted into a tier,
// effort, or capability claim. Nothing here is ever auto-applied to routing.
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

// Proposal is an assessment request for a discovered model that is not yet in
// the static routing table. It deliberately carries no proposed tier or effort:
// model names and advertised size are not evidence of task capability.
type Proposal struct {
	Model              string   `json:"model"`
	Agent              string   `json:"agent"`
	EvidenceStatus     string   `json:"evidence_status"`
	RequiredAssessment []string `json:"required_assessment"`
	Reason             string   `json:"reason"`
	New                bool     `json:"new"`
}

// Inventory is the full Refresh result: what each agent exposes plus assessment
// requests for models missing from both routing and capability catalogs.
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

// Classify reports every discovered model that is NOT already listed for its
// agent in both the legacy reference table and capability catalog. Despite the
// historical name, it does not classify model capability: each gap is marked
// unmeasured and tells the host which evidence must be gathered before routing it.
func Classify(agents []AgentModels, reference map[string]map[string]string, capabilityCatalog ...routing.CapabilityCandidate) []Proposal {
	var proposals []Proposal
	for _, am := range agents {
		known := knownModels(reference[am.Agent])
		for _, candidate := range capabilityCatalog {
			if candidate.Agent == am.Agent {
				known[candidate.Model] = true
			}
		}
		for _, model := range am.Available {
			if known[model] {
				continue // already routed; nothing to propose
			}
			proposals = append(proposals, Proposal{
				Model: model, Agent: am.Agent, EvidenceStatus: "unmeasured_new_model", New: true,
				RequiredAssessment: []string{
					"confirm the model can execute through its installed CLI",
					"compare it on representative capability cells with objective oracles",
					"measure repeatability, latency, operational failures, and independent review quality",
					"let the host relate those observations to each concrete task",
				},
				Reason: "newly discovered model has no local task evidence; its name and size do not imply a routing tier",
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

// Refresh is the convenience entry point used by the MCP tool: probe every
// agent, then report unmeasured models against the live legacy routing table.
// It is read-only end to end.
func Refresh() Inventory {
	agents := Probe()
	return Inventory{
		Agents:    agents,
		Proposals: Classify(agents, routing.AgentsModels(), routing.CapabilityModelCatalog()...),
	}
}
