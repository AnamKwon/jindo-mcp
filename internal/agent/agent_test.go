package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// adaptersUnderTest enumerates the three concrete adapters together with their
// expected per-CLI headless argv (with a model, without a model, and with
// injected extra flags). Each expectation ends in the task positional arg.
//
// extraIn is the per-adapter extras the ORCHESTRATOR would hand this CLI, which
// differ by the flags the CLI actually defines: claude accepts
// --append-system-prompt + --add-dir; agy accepts ONLY --add-dir (it has no
// system-prompt flag, so sending --append-system-prompt would fail); codex has
// no system-prompt/--add-dir equivalent but DOES honor other flags (e.g.
// -s/--sandbox), which BuildCommandWith inserts before the trailing <task>
// positional just like the claude-like CLIs. wantExtras is the argv
// BuildCommandWith(extraIn) must produce.
var adaptersUnderTest = []struct {
	constructor func() *cliAdapter
	wantName    string
	wantCLI     string
	wantWithMdl []string // model set, no extras
	wantNoMdl   []string // model empty, no extras
	extraIn     []string // extras passed to BuildCommandWith
	wantExtras  []string // model set, given extraIn
}{
	{
		NewClaude, "claude", "claude",
		[]string{"claude", "--model", "m", "-p", "do x"},
		[]string{"claude", "-p", "do x"},
		[]string{"--append-system-prompt", "SP", "--add-dir", "/x"},
		[]string{"claude", "--model", "m", "--append-system-prompt", "SP", "--add-dir", "/x", "-p", "do x"},
	},
	{
		NewCodex, "codex", "codex",
		[]string{"codex", "exec", "-m", "m", "--skip-git-repo-check", "do x"},
		[]string{"codex", "exec", "--skip-git-repo-check", "do x"},
		// codex has no system-prompt/--add-dir flag, but the orchestrator DOES
		// pass a real flag it supports: -s workspace-write (elevates the
		// sandbox so headless file writes succeed instead of failing with
		// "read-only sandbox and approvals are disabled").
		[]string{"-s", "workspace-write"},
		[]string{"codex", "exec", "-m", "m", "--skip-git-repo-check", "-s", "workspace-write", "do x"},
	},
	{
		// agy has NO system-prompt flag: the orchestrator passes ONLY --add-dir,
		// so --append-system-prompt must never appear in agy's argv.
		NewAgy, "agy", "agy",
		[]string{"agy", "--model", "m", "-p", "do x"},
		[]string{"agy", "-p", "do x"},
		[]string{"--add-dir", "/x"},
		[]string{"agy", "--model", "m", "--add-dir", "/x", "-p", "do x"},
	},
}

func TestBuildCommand_withModel(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a := tc.constructor()
		got := a.BuildCommand("do x", "m")
		if !reflect.DeepEqual(got, tc.wantWithMdl) {
			t.Errorf("%s.BuildCommand(task,model): got %v, want %v", tc.wantName, got, tc.wantWithMdl)
		}
	}
}

func TestBuildCommand_withoutModel(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a := tc.constructor()
		got := a.BuildCommand("do x", "")
		if !reflect.DeepEqual(got, tc.wantNoMdl) {
			t.Errorf("%s.BuildCommand(task,\"\"): got %v, want %v", tc.wantName, got, tc.wantNoMdl)
		}
		// The task must always be the final positional arg.
		if got[len(got)-1] != "do x" {
			t.Errorf("%s.BuildCommand(task,\"\"): last arg = %q, want %q", tc.wantName, got[len(got)-1], "do x")
		}
	}
}

func TestBuildCommandWith_extras(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a := tc.constructor()
		got := a.BuildCommandWith("do x", "m", tc.extraIn)
		if !reflect.DeepEqual(got, tc.wantExtras) {
			t.Errorf("%s.BuildCommandWith(extras): got %v, want %v", tc.wantName, got, tc.wantExtras)
		}
		// extras (for claude/agy) must sit before -p/task; task stays last.
		if got[len(got)-1] != "do x" {
			t.Errorf("%s.BuildCommandWith(extras): last arg = %q, want %q", tc.wantName, got[len(got)-1], "do x")
		}
	}
}

// TestAgyNeverGetsAppendSystemPrompt is the regression guard for the live bug:
// agy defines no system-prompt flag, so with the orchestrator's agy extras
// (only --add-dir) its argv must contain --add-dir and its value but NEVER
// --append-system-prompt anywhere. In parallel, claude WITH the full extras must
// still carry --append-system-prompt.
func TestAgyNeverGetsAppendSystemPrompt(t *testing.T) {
	agy := NewAgy()
	got := agy.BuildCommandWith("do x", "m", []string{"--add-dir", "/x"})
	if !containsArg(got, "--add-dir") || !containsArg(got, "/x") {
		t.Errorf("agy argv missing --add-dir/value: %v", got)
	}
	if containsArg(got, "--append-system-prompt") {
		t.Errorf("agy argv must NEVER contain --append-system-prompt: %v", got)
	}

	claude := NewClaude()
	cgot := claude.BuildCommandWith("do x", "m", []string{"--append-system-prompt", "SP", "--add-dir", "/x"})
	if !containsArg(cgot, "--append-system-prompt") {
		t.Errorf("claude argv must still contain --append-system-prompt: %v", cgot)
	}
	if !containsArg(cgot, "--add-dir") {
		t.Errorf("claude argv must still contain --add-dir: %v", cgot)
	}
}

// containsArg reports whether argv contains the exact token arg.
func containsArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

func TestBuildCommand_delegatesToBuildCommandWithNil(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a := tc.constructor()
		if !reflect.DeepEqual(a.BuildCommand("do x", "m"), a.BuildCommandWith("do x", "m", nil)) {
			t.Errorf("%s: BuildCommand != BuildCommandWith(nil)", tc.wantName)
		}
	}
}

func TestRun_injectsExecAndReturnsOutput(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a := tc.constructor()
		sentinel := "sentinel-output-" + tc.wantName
		var capturedArgv []string

		a.Exec = func(argv []string) (string, error) {
			capturedArgv = argv
			return sentinel, nil
		}

		got, err := a.Run("do x", "m")
		if err != nil {
			t.Errorf("%s.Run: unexpected error: %v", tc.wantName, err)
		}
		if got != sentinel {
			t.Errorf("%s.Run: got %q, want %q", tc.wantName, got, sentinel)
		}

		wantArgv := a.BuildCommand("do x", "m")
		if !reflect.DeepEqual(capturedArgv, wantArgv) {
			t.Errorf("%s.Run argv: got %v, want %v", tc.wantName, capturedArgv, wantArgv)
		}
	}
}

func TestGetAdapter_knownNames(t *testing.T) {
	for _, tc := range adaptersUnderTest {
		a, err := GetAdapter(tc.wantName)
		if err != nil {
			t.Errorf("GetAdapter(%q): unexpected error: %v", tc.wantName, err)
			continue
		}
		if a.Name() != tc.wantName {
			t.Errorf("GetAdapter(%q).Name() = %q, want %q", tc.wantName, a.Name(), tc.wantName)
		}
		if a.cli != tc.wantCLI {
			t.Errorf("GetAdapter(%q).cli = %q, want %q", tc.wantName, a.cli, tc.wantCLI)
		}
	}
}

func TestGetAdapter_unknownName(t *testing.T) {
	_, err := GetAdapter("bogus")
	if err == nil {
		t.Error("GetAdapter(\"bogus\"): expected error, got nil")
	}
}

// TestWrapExecErr_capturesStderr runs a real failing subprocess to produce a
// genuine *exec.ExitError with populated Stderr, and asserts wrapExecErr
// surfaces that stderr instead of the opaque "exit status N".
func TestWrapExecErr_capturesStderr(t *testing.T) {
	out, err := wrapExecErr(exec.Command("/bin/sh", "-c", "echo boom 1>&2; exit 2").Output())
	if err == nil {
		t.Fatal("wrapExecErr: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("wrapExecErr error = %q, want it to contain %q", err.Error(), "boom")
	}
	_ = out
}

// TestWrapExecErr_success asserts the nil-error path passes output through
// unchanged.
func TestWrapExecErr_success(t *testing.T) {
	out, err := wrapExecErr([]byte("ok"), nil)
	if err != nil {
		t.Errorf("wrapExecErr: unexpected error: %v", err)
	}
	if out != "ok" {
		t.Errorf("wrapExecErr output = %q, want %q", out, "ok")
	}
}

// TestWrapExecErr_nonExitErrorPassthrough asserts non-*exec.ExitError errors
// (e.g. a "command not found" error from exec.Command itself) are returned
// unchanged, since there is no Stderr to enrich them with.
func TestWrapExecErr_nonExitErrorPassthrough(t *testing.T) {
	wantErr := errors.New("boom: not an ExitError")
	out, err := wrapExecErr([]byte("out"), wantErr)
	if err != wantErr {
		t.Errorf("wrapExecErr error = %v, want %v (unchanged)", err, wantErr)
	}
	if out != "out" {
		t.Errorf("wrapExecErr output = %q, want %q", out, "out")
	}
}

// TestAvailable exercises the PATH-probe availability check. It cannot assert a
// specific true/false for the real CLIs (they may or may not be installed on
// the machine running the test), so it only asserts the known agents run
// without panicking and return a bool, and that an unknown name is always
// false (no binary to resolve). A synthetic PATH with a stub binary pins the
// "installed" branch deterministically for one adapter.
func TestAvailable(t *testing.T) {
	for _, name := range []string{"claude", "codex", "agy"} {
		_ = Available(name) // must not panic; value is environment-dependent.
	}
	if Available("nope") {
		t.Errorf("Available(%q) = true, want false for an unknown agent", "nope")
	}
	if Available("") {
		t.Errorf("Available(\"\") = true, want false for an unknown agent")
	}

	// Deterministic installed branch: put an executable named "claude" on a
	// PATH we control and confirm Available finds it.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	t.Setenv("PATH", dir)
	if !Available("claude") {
		t.Errorf("Available(\"claude\") = false with stub on PATH, want true")
	}
}
