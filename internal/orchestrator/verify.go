package orchestrator

// verify.go implements the OBJECTIVE machine gate that complements the LLM peer
// review: the caller supplies a list of verification commands (tests/build/lint)
// that jindo runs, after the author's work, in the dispatch's working directory.
// Their exit codes — not an LLM verdict — decide whether the dispatch is "done".
//
// SECURITY is the load-bearing invariant here. These commands come from a
// caller and are executed on the host, so they are NOT run through a shell and
// must clear two gates BEFORE anything runs (ValidateVerifyCmds):
//   - an ALLOWLIST of first tokens (program names) — anything else is refused;
//   - a rejection of shell metacharacters / operators, so a caller cannot smuggle
//     a pipe/redirect/subshell/command-chain past the single-program contract.
// runVerify then splits each vetted command into program + args and runs it via
// os/exec directly (exec.Command), never a shell, stopping at the first failure.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// verifyAllowlist is the set of program names (a verify command's FIRST token)
// permitted as an objective verification gate: build/test/lint/format/security-scan
// tools for the ecosystems jindo dispatches into. A command whose first token is not here is
// refused before anything runs — this is the primary defense against executing an
// arbitrary caller-supplied program.
var verifyAllowlist = map[string]bool{
	"go":            true,
	"gofmt":         true,
	"golangci-lint": true,
	"gotestsum":     true,
	"npm":           true,
	"npx":           true,
	"pnpm":          true,
	"yarn":          true,
	"node":          true,
	"jest":          true,
	"vitest":        true,
	"tsc":           true,
	"eslint":        true,
	"prettier":      true,
	"python":        true,
	"python3":       true,
	"pytest":        true,
	"ruff":          true,
	"mypy":          true,
	"cargo":         true,
	"make":          true,
	"gosec":         true,
	"govulncheck":   true,
	"bandit":        true,
	"semgrep":       true,
}

// verifyShellMeta are the shell metacharacters that must never appear anywhere in
// a verify command. Their presence means the caller is trying to express a
// pipeline, redirect, subshell, background job, command substitution, or
// command chain — none of which the single-program contract allows, and none of
// which reach a shell anyway (commands run via exec.Command). Note that '&' and
// '|' here also make the "&&"/"||" operators unreachable; the explicit substring
// check below is kept for clarity and to match the documented rejection rule.
const verifyShellMeta = "|&;<>`$()\n"

// verifyOutputCap bounds the combined stdout+stderr captured from a failing
// verify command before it goes into the result payload, so one runaway tool
// (e.g. a test suite that prints megabytes) cannot bloat the dispatch response.
const verifyOutputCap = 4000

// VerifyResult is the outcome of running a dispatch's verify commands. Passed is
// true only when EVERY command exited zero; on the first failure FailedCmd,
// ExitCode, and Output describe that command and execution stops (later commands
// do not run). It is attached to a Result only when verify commands were given.
type VerifyResult struct {
	Passed    bool     `json:"passed"`
	Commands  []string `json:"commands"`
	FailedCmd string   `json:"failed_cmd,omitempty"`
	ExitCode  int      `json:"exit_code,omitempty"`
	Output    string   `json:"output,omitempty"`
}

// ValidateVerifyCmds enforces the security contract on a caller-supplied verify
// list BEFORE any command runs: each command must contain no shell
// metacharacter/operator and its first token must be allowlisted. It returns a
// non-nil error describing the FIRST offending command (so the dispatch can be
// refused with a clear reason); a nil/empty list is trivially valid.
func ValidateVerifyCmds(cmds []string) error {
	for _, cmd := range cmds {
		if strings.ContainsAny(cmd, verifyShellMeta) {
			return fmt.Errorf("verify command %q contains a disallowed shell metacharacter", cmd)
		}
		if strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") {
			return fmt.Errorf("verify command %q contains a disallowed shell operator", cmd)
		}
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return fmt.Errorf("verify command is empty")
		}
		if !verifyAllowlist[fields[0]] {
			return fmt.Errorf("verify command %q uses non-allowlisted program %q", cmd, fields[0])
		}
	}
	return nil
}

// verifyCmdTimeout bounds each individual verify command. Without a deadline a
// hung test/build/lint would block the dispatch — and therefore the MCP request
// — forever, and leak sibling goroutines/processes in the concurrent fan-outs.
// A timed-out command is treated as a failure just like a non-zero exit.
var verifyCmdTimeout = 10 * time.Minute

// runVerify executes each vetted verify command sequentially in cwd, WITHOUT a
// shell: the command is split into program + args and run via exec.Command with
// its working directory set to cwd. Execution stops at the first command that
// exits non-zero (like loop-engine's sequential verify), capturing that command's
// combined stdout+stderr (truncated to verifyOutputCap) and exit code. It assumes
// cmds already passed ValidateVerifyCmds — callers must validate first.
func runVerify(cwd string, cmds []string) VerifyResult {
	res := VerifyResult{Passed: true, Commands: cmds}
	for _, cmd := range cmds {
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			continue
		}
		// runOne bounds the command with a per-command deadline; the closure lets
		// cancel() fire at the end of THIS iteration rather than stacking defers
		// until runVerify returns.
		out, err := func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), verifyCmdTimeout)
			defer cancel()
			c := exec.CommandContext(ctx, fields[0], fields[1:]...)
			c.Dir = cwd
			return c.CombinedOutput()
		}()
		if err != nil {
			res.Passed = false
			res.FailedCmd = cmd
			res.ExitCode = verifyExitCode(err)
			res.Output = truncateVerifyOutput(out)
			return res
		}
	}
	return res
}

// verifyExitCode extracts the process exit code from a CombinedOutput error: a
// real non-zero exit yields that code; any other failure (e.g. the program could
// not be started) yields -1 so the caller can still tell the command did not
// pass.
func verifyExitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// truncateVerifyOutput bounds captured output to verifyOutputCap bytes, appending
// a truncation marker when it had to cut, so the payload stays sane regardless of
// how chatty a failing command is.
func truncateVerifyOutput(b []byte) string {
	if len(b) <= verifyOutputCap {
		return string(b)
	}
	return string(b[:verifyOutputCap]) + "...(truncated)"
}
