package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Adapter mirrors the Python AgentAdapter ABC.
//
// BuildCommandWith is the extras-aware builder; BuildCommand delegates to it
// with nil extra so existing callers keep working.
type Adapter interface {
	Name() string
	BuildCommand(task, model string) []string
	BuildCommandWith(task, model string, extra []string) []string
	Run(task, model string) (string, error)
	// RunWith runs the task threading per-dispatch extra flags (e.g.
	// --append-system-prompt, --add-dir, --permission-mode) through to the
	// built argv. For claude-like CLIs the extras land after --model and
	// before -p/<task>; for codex they land after --skip-git-repo-check and
	// before <task> (e.g. -s workspace-write).
	RunWith(task, model string, extra []string) (string, error)
}

// adapterKind distinguishes the headless command shape a CLI expects.
type adapterKind int

const (
	// kindClaudeLike: `<cli> --model <id> [extra...] -p <task>`.
	// Uses --model (not -m), -p for headless, and honors injected extra flags
	// (e.g. --append-system-prompt, --add-dir) placed before -p/task.
	kindClaudeLike adapterKind = iota
	// kindCodex: `codex exec -m <id> --skip-git-repo-check [extra...] <task>`.
	// Needs the `exec` subcommand, -m for the model, and --skip-git-repo-check.
	// codex has no --append-system-prompt equivalent, so the memory-read
	// instruction is prefixed into <task> by the orchestrator (see
	// buildDispatchArgs); but codex DOES support other flags via `-c`/`-s`/
	// `--add-dir` (confirmed via `codex exec --help`), so extra IS honored
	// here, placed after --skip-git-repo-check and before <task> (e.g.
	// `-s workspace-write` to allow real file writes headlessly).
	kindCodex
)

// cliAdapter is the concrete implementation. Exec is a mockable seam.
type cliAdapter struct {
	name string
	cli  string
	kind adapterKind
	// ExtraArgs is a per-dispatch extras slot the orchestrator may set to
	// inject flags without threading them through every call site. It is
	// merged ahead of the per-call extra passed to BuildCommandWith.
	ExtraArgs []string
	Exec      func(argv []string) (string, error)
}

func (a *cliAdapter) Name() string { return a.name }

// BuildCommand builds the headless argv with no injected extras.
func (a *cliAdapter) BuildCommand(task, model string) []string {
	return a.BuildCommandWith(task, model, nil)
}

// BuildCommandWith builds the correct per-CLI headless argv, inserting extra
// flags (for claude/agy) after the model flag and before -p/<task>. The
// adapter's own ExtraArgs are applied first, then the per-call extra.
//
// The final positional argument is always <task>.
func (a *cliAdapter) BuildCommandWith(task, model string, extra []string) []string {
	switch a.kind {
	case kindCodex:
		// codex exec -m <id> --skip-git-repo-check [extra...] <task>
		// extra (e.g. "-s","workspace-write") is honored, placed after
		// --skip-git-repo-check and before the trailing <task> positional.
		argv := []string{a.cli, "exec"}
		if model != "" {
			argv = append(argv, "-m", model)
		}
		argv = append(argv, "--skip-git-repo-check")
		argv = append(argv, a.ExtraArgs...)
		argv = append(argv, extra...)
		argv = append(argv, task)
		return argv
	default: // kindClaudeLike
		// <cli> --model <id> [extra...] -p <task>
		argv := []string{a.cli}
		if model != "" {
			argv = append(argv, "--model", model)
		}
		argv = append(argv, a.ExtraArgs...)
		argv = append(argv, extra...)
		argv = append(argv, "-p", task)
		return argv
	}
}

// Run builds the argv (with any adapter ExtraArgs) and delegates to Exec.
func (a *cliAdapter) Run(task, model string) (string, error) {
	argv := a.BuildCommand(task, model)
	return a.Exec(argv)
}

// RunWith builds the argv with per-dispatch extra flags and delegates to Exec.
func (a *cliAdapter) RunWith(task, model string, extra []string) (string, error) {
	argv := a.BuildCommandWith(task, model, extra)
	return a.Exec(argv)
}

// wrapExecErr enriches a failed subprocess error with the captured stderr, so
// callers see the CLI's actual complaint (e.g. an unsupported flag) instead
// of an opaque "exit status N". On success (err == nil) it passes the output
// through unchanged.
func wrapExecErr(out []byte, err error) (string, error) {
	if err == nil {
		return string(out), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

// defaultExec runs the real subprocess via os/exec.
func defaultExec(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	return wrapExecErr(out, err)
}

// NewClaude returns an Adapter for the 'claude' CLI.
func NewClaude() *cliAdapter {
	return &cliAdapter{name: "claude", cli: "claude", kind: kindClaudeLike, Exec: defaultExec}
}

// NewCodex returns an Adapter for the 'codex' CLI.
func NewCodex() *cliAdapter {
	return &cliAdapter{name: "codex", cli: "codex", kind: kindCodex, Exec: defaultExec}
}

// NewAgy returns an Adapter for the 'agy' CLI.
func NewAgy() *cliAdapter {
	return &cliAdapter{name: "agy", cli: "agy", kind: kindClaudeLike, Exec: defaultExec}
}

// GetAdapter returns the named adapter or an error for unknown names.
func GetAdapter(name string) (*cliAdapter, error) {
	switch name {
	case "claude":
		return NewClaude(), nil
	case "codex":
		return NewCodex(), nil
	case "agy":
		return NewAgy(), nil
	default:
		return nil, fmt.Errorf("unknown adapter: %q", name)
	}
}
