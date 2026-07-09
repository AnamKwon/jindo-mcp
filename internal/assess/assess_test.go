package assess

import (
	"context"
	"errors"
	"testing"
	"time"

	"jindo/internal/routing"
)

// mockExec builds an exec seam that ignores argv and returns the given
// output/error, recording that it was invoked.
func mockExec(out string, err error) (func(ctx context.Context, argv []string) (string, error), *bool) {
	called := false
	fn := func(ctx context.Context, argv []string) (string, error) {
		called = true
		return out, err
	}
	return fn, &called
}

// TestAssess_cleanHard: an unambiguous "hard" answer yields ok=true, tier=hard.
func TestAssess_cleanHard(t *testing.T) {
	exec, called := mockExec("hard\n", nil)
	a := New(withExec(exec))

	tier, ok := a("rewrite the lock-free queue", routing.Score{})
	if !ok {
		t.Fatalf("clean answer: ok=false, want true")
	}
	if tier != "hard" {
		t.Errorf("tier = %q, want %q", tier, "hard")
	}
	if !*called {
		t.Errorf("exec seam was not invoked")
	}
}

// TestAssess_lastLine: a tier word on the last line (with a preamble line and
// trailing punctuation) is accepted tolerantly.
func TestAssess_lastLine(t *testing.T) {
	exec, _ := mockExec("Let me think about this.\nStandard.", nil)
	a := New(withExec(exec))

	tier, ok := a("add a config flag", routing.Score{})
	if !ok || tier != "standard" {
		t.Errorf("last-line parse: got tier=%q ok=%v, want tier=standard ok=true", tier, ok)
	}
}

// TestAssess_adapterError: an adapter/subprocess error (e.g. agy not found)
// yields ok=false regardless of any output.
func TestAssess_adapterError(t *testing.T) {
	exec, _ := mockExec("hard", errors.New("exec: \"agy\": executable file not found"))
	a := New(withExec(exec))

	if tier, ok := a("anything", routing.Score{}); ok {
		t.Errorf("adapter error: got tier=%q ok=true, want ok=false", tier)
	}
}

// TestAssess_timeout: a real ctx deadline (via a slow exec seam that honors
// ctx) surfaces as an error → ok=false, and no goroutine is leaked because the
// seam returns once ctx is done.
func TestAssess_timeout(t *testing.T) {
	slow := func(ctx context.Context, argv []string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	a := New(withExec(slow), WithTimeout(10*time.Millisecond))

	start := time.Now()
	if tier, ok := a("anything", routing.Score{}); ok {
		t.Errorf("timeout: got tier=%q ok=true, want ok=false", tier)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("timeout took %v, expected it to fire near 10ms", elapsed)
	}
}

// TestAssess_unparseable: prose with no clean tier word on the last line is
// rejected as ambiguous.
func TestAssess_unparseable(t *testing.T) {
	exec, _ := mockExec("This could be either standard or hard, hard to say.", nil)
	a := New(withExec(exec))

	if tier, ok := a("anything", routing.Score{}); ok {
		t.Errorf("unparseable: got tier=%q ok=true, want ok=false", tier)
	}
}

// TestAssess_invalidTier: a valid-looking single word that is NOT one of the
// three tiers is rejected.
func TestAssess_invalidTier(t *testing.T) {
	exec, _ := mockExec("medium\n", nil)
	a := New(withExec(exec))

	if tier, ok := a("anything", routing.Score{}); ok {
		t.Errorf("invalid tier: got tier=%q ok=true, want ok=false", tier)
	}
}

// TestParseTier_table exercises the tolerant parser directly across the
// alone/last-line/ambiguous/invalid cases.
func TestParseTier_table(t *testing.T) {
	cases := []struct {
		in       string
		wantTier string
		wantOK   bool
	}{
		{"hard", "hard", true},
		{"  HARD  ", "hard", true},
		{"trivial\n", "trivial", true},
		{"preamble\nstandard", "standard", true},
		{"reasoning...\n**hard**", "hard", true}, // last line trims markdown/punct to "hard"
		{"the answer is hard", "", false},        // tier word embedded in prose
		{"medium", "", false},                     // not a tier
		{"", "", false},                           // empty
		{"   \n  ", "", false},                    // whitespace only
	}
	for _, c := range cases {
		tier, ok := parseTier(c.in)
		if tier != c.wantTier || ok != c.wantOK {
			t.Errorf("parseTier(%q) = (%q,%v), want (%q,%v)", c.in, tier, ok, c.wantTier, c.wantOK)
		}
	}
}
