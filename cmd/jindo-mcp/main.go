// Command jindo-mcp runs the jindo MCP server over stdio: it wires real
// collaborators (shared memory under ./.jindo, a tmux manager, an orchestrator
// with the default routing/agent seams) into an mcp.Server and serves
// JSON-RPC over stdin/stdout until EOF.
package main

import (
	"fmt"
	"os"

	"jindo/internal/agent"
	"jindo/internal/assess"
	"jindo/internal/mcp"
	"jindo/internal/memory"
	"jindo/internal/orchestrator"
	"jindo/internal/routing"
	"jindo/internal/tmux"
)

// run builds the real server and serves stdio. Kept separate from main so the
// exit code is decided in one place.
func run() error {
	// MCP hosts spawn stdio servers with an arbitrary working directory.
	// Everything here is project-relative — the .jindo shared-memory store,
	// the agent CLI subprocesses, and the tmux windows — so re-anchor the
	// whole process at the project root when the host provides it
	// (CLAUDE_PROJECT_DIR is the same variable .mcp.json uses to locate the
	// binary). Unset keeps the plain run-from-repo-root behavior.
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
		if err := os.Chdir(dir); err != nil {
			return fmt.Errorf("chdir %s: %w", dir, err)
		}
	}
	// Overlay runtime routing overrides from the project's .jindo store, now that
	// the working directory is the project root. Best-effort: an absent file is a
	// no-op (defaults stand), and a malformed file is logged but never fails
	// startup, so a bad override can't take the server down.
	if err := routing.ApplyOverrides(".jindo/routing_overrides.json"); err != nil {
		fmt.Fprintln(os.Stderr, "jindo-mcp: routing overrides:", err)
	}
	// Populate routing's LLM-assessment seam with the real agy-backed
	// assessor. This only wires the seam; assessment stays double-gated
	// (policy llm_assess.enabled + JINDO_LLM_ASSESS env) inside routing.Select,
	// so wiring it here changes no default behavior.
	routing.Assessor = assess.New()

	// Wire routing's availability seam to the real PATH probe so startup routing
	// only picks agents whose CLI is actually installed (missing agents fall back
	// or surface a clear error). Left unset in tests/library use, where every
	// agent is treated as available (see routing.agentUsable).
	routing.AgentAvailable = agent.Available

	mem := memory.New(".jindo")
	tmuxMgr := tmux.New("jindo", nil)
	o := orchestrator.New(mem, tmuxMgr)
	return mcp.NewServer(o).Serve(os.Stdin, os.Stdout)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jindo-mcp:", err)
		os.Exit(1)
	}
}
