// Package tmux manages a persistent tmux session with one window per coding
// agent.
//
// Each agent (claude/codex/agy) gets its own window in a single long-lived
// session so the agent's interactive context survives across multiple
// dispatched tasks. The session is created once and kept alive; dispatching a
// task never recreates or kills the session.
//
// All tmux interaction is funneled through the Tmux seam, so tests can inject a
// fake and assert exact tmux invocations without a running tmux server.
package tmux

import (
	"fmt"
	"os/exec"
	"strings"
)

// DefaultAgents are the agents recognized when none are supplied.
var DefaultAgents = []string{"claude", "codex", "agy"}

// TmuxManager owns a persistent tmux session and its per-agent windows.
type TmuxManager struct {
	Session string
	Agents  []string
	// Tmux is the single execution seam. It runs a tmux command and returns
	// the combined output, the process exit code, and any launch error.
	Tmux func(args ...string) (string, int, error)
}

// realTmux runs `tmux <args...>`, capturing combined output and deriving the
// exit code (0 on success; on *exec.ExitError the process ExitCode()).
func realTmux(args ...string) (string, int, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(out), exitErr.ExitCode(), err
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// New builds a TmuxManager. An empty session defaults to "jindo"; nil agents
// default to DefaultAgents. The agents slice is copied so external mutation of
// the caller's list cannot silently change which agents this manager
// recognizes.
func New(session string, agents []string) *TmuxManager {
	if session == "" {
		session = "jindo"
	}
	if agents == nil {
		agents = DefaultAgents
	}
	copied := make([]string, len(agents))
	copy(copied, agents)
	return &TmuxManager{
		Session: session,
		Agents:  copied,
		Tmux:    realTmux,
	}
}

// Exists reports whether the persistent session is currently alive.
func (m *TmuxManager) Exists() bool {
	_, rc, _ := m.Tmux("has-session", "-t", m.Session)
	return rc == 0
}

// EnsureSession creates the session and per-agent windows once; idempotent.
//
// If the session already exists it is left untouched (kept alive); no windows
// are recreated, so repeated calls cannot duplicate windows. Returns the first
// error encountered, if any.
func (m *TmuxManager) EnsureSession() error {
	if m.Exists() {
		return nil
	}
	if _, _, err := m.Tmux("new-session", "-d", "-s", m.Session); err != nil {
		return err
	}
	for _, agent := range m.Agents {
		if _, _, err := m.Tmux("new-window", "-t", m.Session, "-n", agent); err != nil {
			return err
		}
	}
	return nil
}

// Dispatch sends command (followed by Enter) to agent's window.
func (m *TmuxManager) Dispatch(agent, command string) error {
	if !m.knows(agent) {
		return fmt.Errorf("unknown agent: %q", agent)
	}
	target := m.Session + ":" + agent
	_, _, err := m.Tmux("send-keys", "-t", target, command, "Enter")
	return err
}

// ListWindows returns the names of the session's windows.
func (m *TmuxManager) ListWindows() ([]string, error) {
	out, _, err := m.Tmux("list-windows", "-t", m.Session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	var windows []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			windows = append(windows, name)
		}
	}
	return windows, nil
}

func (m *TmuxManager) knows(agent string) bool {
	for _, a := range m.Agents {
		if a == agent {
			return true
		}
	}
	return false
}
