package tmux

import (
	"reflect"
	"testing"
)

// fakeTmux is a deterministic tmux seam. It records every invocation as an
// argument slice and answers has-session with a controllable return code.
type fakeTmux struct {
	calls        [][]string
	hasSessionRC int    // rc returned for has-session probes
	listOut      string // output returned for list-windows probes
}

func (f *fakeTmux) run(args ...string) (string, int, error) {
	// Record a copy so later reuse of the caller's slice cannot mutate history.
	rec := make([]string, len(args))
	copy(rec, args)
	f.calls = append(f.calls, rec)

	switch args[0] {
	case "has-session":
		return "", f.hasSessionRC, nil
	case "list-windows":
		return f.listOut, 0, nil
	default:
		return "", 0, nil
	}
}

func newWithFake(f *fakeTmux, session string, agents []string) *TmuxManager {
	m := New(session, agents)
	m.Tmux = f.run
	return m
}

func TestEnsureSessionCreatesWhenAbsent(t *testing.T) {
	f := &fakeTmux{hasSessionRC: 1} // not existing
	agents := []string{"claude", "codex", "agy"}
	m := newWithFake(f, "jindo", agents)

	if err := m.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession returned error: %v", err)
	}

	// Expect: has-session probe, one new-session, then one new-window per agent.
	want := [][]string{
		{"has-session", "-t", "jindo"},
		{"new-session", "-d", "-s", "jindo"},
		{"new-window", "-t", "jindo", "-n", "claude"},
		{"new-window", "-t", "jindo", "-n", "codex"},
		{"new-window", "-t", "jindo", "-n", "agy"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls mismatch:\n got %v\nwant %v", f.calls, want)
	}

	newSessions, newWindows := 0, 0
	for _, c := range f.calls {
		switch c[0] {
		case "new-session":
			newSessions++
		case "new-window":
			newWindows++
		}
	}
	if newSessions != 1 {
		t.Errorf("new-session count = %d, want 1", newSessions)
	}
	if newWindows != len(agents) {
		t.Errorf("new-window count = %d, want %d", newWindows, len(agents))
	}
}

func TestEnsureSessionIdempotentWhenExisting(t *testing.T) {
	f := &fakeTmux{hasSessionRC: 0} // existing
	m := newWithFake(f, "jindo", nil)

	if err := m.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession returned error: %v", err)
	}

	// Only the has-session probe; no session/window creation.
	want := [][]string{{"has-session", "-t", "jindo"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls mismatch:\n got %v\nwant %v", f.calls, want)
	}
	for _, c := range f.calls {
		if c[0] == "new-session" || c[0] == "new-window" {
			t.Fatalf("unexpected creation call: %v", c)
		}
	}
}

func TestDispatchKnownAgent(t *testing.T) {
	f := &fakeTmux{}
	m := newWithFake(f, "jindo", []string{"claude", "codex", "agy"})

	if err := m.Dispatch("codex", "echo hi"); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	want := [][]string{
		{"send-keys", "-t", "jindo:codex", "echo hi", "Enter"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls mismatch:\n got %v\nwant %v", f.calls, want)
	}
}

func TestDispatchUnknownAgent(t *testing.T) {
	f := &fakeTmux{}
	m := newWithFake(f, "jindo", []string{"claude", "codex", "agy"})

	if err := m.Dispatch("ghost", "echo hi"); err == nil {
		t.Fatal("Dispatch(unknown) returned nil error, want error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("expected no tmux calls for unknown agent, got %v", f.calls)
	}
}

func TestExistsReflectsRC(t *testing.T) {
	for _, tc := range []struct {
		rc   int
		want bool
	}{
		{0, true},
		{1, false},
	} {
		f := &fakeTmux{hasSessionRC: tc.rc}
		m := newWithFake(f, "jindo", nil)
		if got := m.Exists(); got != tc.want {
			t.Errorf("Exists with rc=%d = %v, want %v", tc.rc, got, tc.want)
		}
	}
}

func TestListWindowsParsesOutput(t *testing.T) {
	f := &fakeTmux{listOut: "claude\n\ncodex\n  agy  \n\n"}
	m := newWithFake(f, "jindo", nil)

	got, err := m.ListWindows()
	if err != nil {
		t.Fatalf("ListWindows returned error: %v", err)
	}
	want := []string{"claude", "codex", "agy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("windows mismatch:\n got %v\nwant %v", got, want)
	}

	// Confirm the probe used the expected tmux invocation.
	wantCall := []string{"list-windows", "-t", "jindo", "-F", "#{window_name}"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], wantCall) {
		t.Fatalf("list-windows call mismatch:\n got %v\nwant %v", f.calls, wantCall)
	}
}
