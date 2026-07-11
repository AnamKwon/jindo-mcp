package modelscan

import (
	"fmt"
	"strings"
	"testing"

	"jindo/internal/routing"
)

// sampleAgyModels mixes two NEW models (not in the routing table) with one that
// IS already routed for agy ("Gemini 3.1 Pro (High)"), so a test can assert the
// existing one is not proposed while the new ones are.
const sampleAgyModels = `Gemini 3.1 Pro (High)
GPT-OSS 120B (Medium)
Claude Opus 4.6 (Thinking)
`

// sampleCodexDoctor reproduces the `codex doctor` line shape the parser targets:
// an indented "model <id> · <provider>" row among other diagnostic lines.
const sampleCodexDoctor = `codex doctor
  auth                     ok
      model                    gpt-5.5 · openai
  network                  ok
`

// withStubbedRun swaps runCmd for a canned dispatcher and restores it after the
// test. claude is never probed via runCmd (it has no CLI command), so a call for
// it fails the test loudly.
func withStubbedRun(t *testing.T, agyErr bool) func() {
	t.Helper()
	orig := runCmd
	runCmd = func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "agy" && len(args) == 1 && args[0] == "models":
			if agyErr {
				return nil, fmt.Errorf("agy: command failed")
			}
			return []byte(sampleAgyModels), nil
		case name == "codex" && len(args) == 1 && args[0] == "doctor":
			return []byte(sampleCodexDoctor), nil
		case name == "claude":
			t.Errorf("claude CLI must not be probed via runCmd, got args %v", args)
			return nil, fmt.Errorf("unexpected claude probe")
		default:
			t.Errorf("unexpected runCmd call: %s %v", name, args)
			return nil, fmt.Errorf("unexpected call")
		}
	}
	return func() { runCmd = orig }
}

func findAgent(agents []AgentModels, name string) (AgentModels, bool) {
	for _, a := range agents {
		if a.Agent == name {
			return a, true
		}
	}
	return AgentModels{}, false
}

func findProposal(props []Proposal, model string) (Proposal, bool) {
	for _, p := range props {
		if p.Model == model {
			return p, true
		}
	}
	return Proposal{}, false
}

func TestProbe(t *testing.T) {
	defer withStubbedRun(t, false)()

	agents := Probe()

	// Stable, sorted agent order.
	got := []string{agents[0].Agent, agents[1].Agent, agents[2].Agent}
	want := []string{"agy", "claude", "codex"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent order = %v, want %v", got, want)
		}
	}

	agy, _ := findAgent(agents, "agy")
	if agy.Source != "enumerated" {
		t.Errorf("agy source = %q, want enumerated", agy.Source)
	}
	wantModels := []string{"Gemini 3.1 Pro (High)", "GPT-OSS 120B (Medium)", "Claude Opus 4.6 (Thinking)"}
	if strings.Join(agy.Available, "|") != strings.Join(wantModels, "|") {
		t.Errorf("agy available = %v, want %v", agy.Available, wantModels)
	}

	codex, _ := findAgent(agents, "codex")
	if codex.Source != "active-only" {
		t.Errorf("codex source = %q, want active-only", codex.Source)
	}
	if codex.Active != "gpt-5.5" {
		t.Errorf("codex active = %q, want gpt-5.5", codex.Active)
	}
	if len(codex.Available) != 1 || codex.Available[0] != "gpt-5.5" {
		t.Errorf("codex available = %v, want [gpt-5.5]", codex.Available)
	}

	claude, _ := findAgent(agents, "claude")
	if claude.Source != "none" {
		t.Errorf("claude source = %q, want none", claude.Source)
	}
	if claude.Available != nil {
		t.Errorf("claude available = %v, want nil", claude.Available)
	}
}

func TestProbeAgyError(t *testing.T) {
	defer withStubbedRun(t, true)()

	agents := Probe()
	agy, _ := findAgent(agents, "agy")
	if agy.Source != "error" {
		t.Errorf("agy source = %q, want error", agy.Source)
	}
	if len(agy.Available) != 0 {
		t.Errorf("agy available = %v, want empty on error", agy.Available)
	}
}

func TestClassify(t *testing.T) {
	defer withStubbedRun(t, false)()

	agents := Probe()
	proposals := Classify(agents, routing.AgentsModels())

	// The already-routed agy model must NOT be proposed.
	if _, ok := findProposal(proposals, "Gemini 3.1 Pro (High)"); ok {
		t.Errorf("Gemini 3.1 Pro (High) is in the routing table and must not be proposed")
	}

	// GPT-OSS 120B (Medium): "oss" (trivial) is checked before hard keywords,
	// so it lands trivial with effort from routing.
	oss, ok := findProposal(proposals, "GPT-OSS 120B (Medium)")
	if !ok {
		t.Fatalf("GPT-OSS 120B (Medium) not proposed")
	}
	if !oss.New {
		t.Errorf("GPT-OSS proposal New = false, want true")
	}
	if oss.ProposedTier != "trivial" {
		t.Errorf("GPT-OSS tier = %q, want trivial", oss.ProposedTier)
	}
	if oss.ProposedEffort != routing.EffortForDifficulty("trivial") {
		t.Errorf("GPT-OSS effort = %q, want %q", oss.ProposedEffort, routing.EffortForDifficulty("trivial"))
	}
	if oss.Agent != "agy" {
		t.Errorf("GPT-OSS agent = %q, want agy", oss.Agent)
	}

	// Claude Opus 4.6 (Thinking): no trivial keyword, "opus" -> hard.
	opus, ok := findProposal(proposals, "Claude Opus 4.6 (Thinking)")
	if !ok {
		t.Fatalf("Claude Opus 4.6 (Thinking) not proposed")
	}
	if opus.ProposedTier != "hard" {
		t.Errorf("Opus tier = %q, want hard", opus.ProposedTier)
	}
	if opus.ProposedEffort != routing.EffortForDifficulty("hard") {
		t.Errorf("Opus effort = %q, want %q", opus.ProposedEffort, routing.EffortForDifficulty("hard"))
	}
	if !strings.Contains(opus.Reason, "opus") {
		t.Errorf("Opus reason %q should record the matched keyword", opus.Reason)
	}

	// codex's active model gpt-5.5 is already the codex "hard" model in the
	// routing table, so it must NOT be proposed (already routed).
	if _, ok := findProposal(proposals, "gpt-5.5"); ok {
		t.Errorf("gpt-5.5 is codex's routed hard model and must not be proposed")
	}
}

func TestRefresh(t *testing.T) {
	defer withStubbedRun(t, false)()

	inv := Refresh()
	if len(inv.Agents) != 3 {
		t.Fatalf("Refresh agents = %d, want 3", len(inv.Agents))
	}
	if len(inv.Proposals) == 0 {
		t.Errorf("Refresh produced no proposals, expected the new models to be proposed")
	}
}
