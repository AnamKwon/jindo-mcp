// Package assess provides a real routing.Assessor implementation backed by the
// agy (Gemini Flash) agent adapter. It exists as a separate package — importing
// both internal/routing and internal/agent — precisely to break the import
// cycle that would arise if routing (the deterministic scorer) depended on the
// agent CLI layer: routing owns the nil-by-default Assessor seam, and this
// package populates it at wiring time (see cmd/jindo-mcp/main.go).
//
// The assessor asks agy to re-judge a task's difficulty tier with a compact,
// single-word prompt, parses the answer tolerantly, and treats ANY failure
// (timeout, adapter/subprocess error, unparseable output, or a tier outside the
// closed set) as ok=false so routing.Select deterministically keeps the
// ScoreTask tier. It is therefore always safe to wire in: it can only ever
// override the tier on a clean, unambiguous, valid answer.
package assess

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"jindo/internal/agent"
	"jindo/internal/routing"
)

// Timeout bounds each assessment subprocess. It is the default deadline applied
// to the agy CLI invocation; New() captures it into the returned assessor, and
// WithTimeout overrides it per constructed assessor. A bound is essential: a
// hung agy process must degrade to ok=false (deterministic fallback) rather
// than stall routing indefinitely.
var Timeout = 20 * time.Second

// assessor holds the resolved configuration for one Assessor closure: the agy
// model to run, the per-call timeout, and the exec seam (mockable in tests).
type assessor struct {
	model   string
	timeout time.Duration
	// exec runs the built argv under ctx and returns combined stdout. It is a
	// seam so tests can substitute the real subprocess (see withExec); the
	// default (defaultExec) uses exec.CommandContext so a timeout kills the
	// process — no leaked goroutine.
	exec func(ctx context.Context, argv []string) (string, error)
}

// Option customizes a constructed assessor.
type Option func(*assessor)

// WithTimeout overrides the per-call subprocess deadline (default: Timeout).
func WithTimeout(d time.Duration) Option {
	return func(a *assessor) { a.timeout = d }
}

// withExec overrides the exec seam. Unexported: it exists for the white-box
// test to inject a mock subprocess without a live agy call, not as public API.
func withExec(fn func(ctx context.Context, argv []string) (string, error)) Option {
	return func(a *assessor) { a.exec = fn }
}

// New returns a routing.Assessor closure backed by the agy adapter. The
// returned func matches routing.Assessor's signature exactly, so callers assign
// it directly (routing.Assessor = assess.New()).
func New(opts ...Option) func(task string, score routing.Score) (tier string, ok bool) {
	a := &assessor{
		model:   defaultModel(),
		timeout: Timeout,
		exec:    defaultExec,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a.assess
}

// assess runs the agy adapter with a difficulty-classification prompt bounded by
// a.timeout and parses the answer. score is unused by the current prompt (the
// deterministic total already gated the call in Select), but is part of the
// Assessor contract so a future prompt could show agy the score.
func (a *assessor) assess(task string, _ routing.Score) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	// Run genuinely via the agy adapter: it owns the correct agy CLI argv
	// shape. We override its Exec seam with a ctx-bound closure so the timeout
	// propagates to the subprocess (RunWith itself takes no ctx).
	adapter := agent.NewAgy()
	adapter.Exec = func(argv []string) (string, error) {
		return a.exec(ctx, argv)
	}

	out, err := adapter.RunWith(buildPrompt(task), a.model, nil)
	if err != nil {
		return "", false
	}
	tier, ok := parseTier(out)
	if !ok || !validAssessedTier(tier) {
		return "", false
	}
	return tier, true
}

// buildPrompt asks agy to classify difficulty as EXACTLY one of the three
// tiers, with no surrounding prose. The instruction is intentionally terse and
// leaves no room for a preamble so parseTier's tolerant match usually hits the
// exact-single-word path.
func buildPrompt(task string) string {
	return "Classify the difficulty of the following software engineering task " +
		"as exactly one of these words: trivial, standard, hard. " +
		"Reply with ONLY that single lowercase word — no punctuation, no explanation.\n\n" +
		"Task: " + task
}

// validAssessedTier reports whether tier is one of the three tiers ScoreTask can
// produce. It mirrors routing.validAssessedTier (unexported there); Select
// re-checks the same set, so this is a fail-fast belt-and-suspenders guard.
func validAssessedTier(tier string) bool {
	switch tier {
	case "trivial", "standard", "hard":
		return true
	}
	return false
}

// parseTier tolerantly extracts a tier word from the model's raw output. It
// accepts the word standing alone (whole trimmed output) or as the last
// non-empty line (after stripping surrounding punctuation/markdown), and
// rejects anything else — including a last line that mixes a tier word with
// other prose — as ambiguous (ok=false).
func parseTier(out string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(out))
	if s == "" {
		return "", false
	}
	// Whole answer is exactly one tier word.
	if validAssessedTier(s) {
		return s, true
	}
	// Otherwise consider only the last non-empty line; a valid answer must be
	// that word alone (once trimmed of surrounding punctuation/markdown).
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		line = strings.Trim(line, " \t.!,:;\"'`*-")
		if line == "" {
			continue
		}
		if validAssessedTier(line) {
			return line, true
		}
		// The last non-empty line is not a clean tier word: ambiguous, reject.
		return "", false
	}
	return "", false
}

// defaultModel resolves the agy Flash model from the embedded routing config
// (agy's "trivial" slot is Gemini Flash), keeping the model a single source of
// truth. It falls back to a constant only if the config lacks the slot.
func defaultModel() string {
	if tiers, ok := routing.AgentsModels()["agy"]; ok {
		if m := tiers["trivial"]; m != "" {
			return m
		}
	}
	return "gemini-3.5-flash"
}

// defaultExec runs the built argv as a real subprocess bound to ctx, so a
// timeout (ctx deadline) kills the process. Errors — including the ctx
// deadline — propagate to assess, which maps any error to ok=false.
func defaultExec(ctx context.Context, argv []string) (string, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return string(out), err
}
