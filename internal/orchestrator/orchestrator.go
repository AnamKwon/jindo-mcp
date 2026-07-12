// Package orchestrator ports jindo/orchestrator.py's Orchestrator to Go, adapted
// to the headless contract-driven model: it ties together routing (which
// agent+model a task runs on), the agent adapter (which actually runs the task
// as a headless subprocess), and shared memory (which records the routing
// intent and result so later dispatches can build on prior context).
//
// Headless memory-access model: the orchestrator does NOT read memory.json
// content into itself during a dispatch. Instead it hands the agent the bounded
// memory DIRECTORY — via an --append-system-prompt (built by agentproto) and an
// --add-dir flag for claude-like CLIs, or a task-text-prefixed instruction for
// agy/codex — and the agent reads prior context itself. This keeps the
// orchestrator lean: it moves paths and records outcomes, it does not
// aggregate memory. (See LEANNESS below.)
//
// Headless privilege model: each CLI also needs an explicit, narrowly-scoped
// permission/sandbox flag so it can actually write files without hanging on
// an interactive approval prompt that headless dispatch (no TTY) can never
// answer. See buildDispatchArgs for the live-confirmed bugs this addresses
// (a silent no-op on claude/codex, and a silent wrong-directory write on agy).
//
// Task-key scheme: each dispatch allocates a collision-free, agent-partitioned
// key "task:<agent>:<n>" from shared memory (Mem.AllocKey), used as the
// dispatch-id. The intent is Upsert'd first under that key with result=nil
// authored by "orchestrator"; after the adapter runs, the SAME key is Upsert'd
// with the populated result authored by the executing agent (idempotent — a
// retry on the same key overwrites in place rather than duplicating).
//
// Tmux is NOT the execution path in this headless model. The adapter subprocess
// is the sole executor; the Tmux field is retained (some deployments attach a
// persistent per-agent window for observation) but Dispatch no longer echoes a
// decorative command into it.
//
// Collaborators are injectable (Route, Tmux, GetAdapter, Mem) so tests can drive
// deterministic dispatches without a real LLM, tmux server, or subprocess while
// exercising the real SharedMemory.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jindo/internal/agent"
	"jindo/internal/agentproto"
	"jindo/internal/memory"
	"jindo/internal/policy"
	"jindo/internal/routing"
	"jindo/internal/tmux"
)

// dispatchMem is the shared-memory surface Dispatch uses. It deliberately does
// NOT expose Read/All: the leanness invariant is that Dispatch never pulls
// memory entry content into the orchestrator. Tests may inject a spy over the
// real store to assert that (see memPort in the tests). *memory.SharedMemory
// satisfies this interface.
type dispatchMem interface {
	Root() string
	AllocKey(agent string) (string, error)
	AppendNote(author, text string) error
	Upsert(key string, value any, author string) error
	// MaybeCompact self-bounds store growth after a dispatch's writes. It is a
	// cheap threshold check that only rewrites when a cap is exceeded (see
	// memory.SharedMemory.MaybeCompact). Kept in the lean surface — not Read/All
	// — because it never pulls entry content into the orchestrator; it only asks
	// the store to prune itself. *memory.SharedMemory satisfies this.
	MaybeCompact(opts memory.CompactOptions) (bool, error)
	// Stats reports the injection context Dispatch hands the agent — the number
	// of keyed records and whether a _digest is present — WITHOUT pulling any
	// entry content into the orchestrator (see memory.SharedMemory.Stats). Kept
	// in the lean surface for exactly that reason: it makes the dispatch log's
	// injected_records/injected_digest observable while preserving the no
	// Read/All invariant. *memory.SharedMemory satisfies this.
	Stats() (records int, hasDigest bool, err error)
	// RetrieveInsights and AddInsight expose the cross-agent insight layer (see
	// insights.go). These are the ONE deliberate, bounded exception to the "no
	// entry content in the orchestrator" invariant: the insight layer IS the
	// curated bounded-memory channel the headless-agent contract promises, so
	// Dispatch reads a relevance-ranked top-K of it to build the injection brief
	// (RetrieveInsights) and contributes this dispatch's own distilled summary
	// back to it (AddInsight). Unlike Read/All, neither exposes raw task-entry
	// content — only the small, purpose-built learning tier. *memory.SharedMemory
	// satisfies both.
	RetrieveInsights(task string, k int) ([]memory.Insight, error)
	AddInsight(text, agent, model string, tags []string) (bool, error)
	// AddInsightWith is AddInsight plus the injected-text guard: notes that
	// merely parrot an insight injected into THIS dispatch's prompt must not
	// reinforce it (see memory.SharedMemory.AddInsightWith). Dispatch passes the
	// texts it injected so a rediscovery that is really just an echo can be
	// recorded but never bootstrap its own confidence. *memory.SharedMemory
	// satisfies this.
	AddInsightWith(text, agent, model string, tags []string, injected []string) (bool, error)
}

// Orchestrator distributes tasks to agents, sharing context via shared memory.
// Route and GetAdapter default to the real routing/agent facilities; tests may
// override any field before calling Dispatch.
type Orchestrator struct {
	// Route decides the agent+model+difficulty for a task, optionally reweighted
	// by a priority hint (cost/quality/latency/"") and/or pinned to an exact
	// model (model != "" bypasses score-based routing). Defaults to
	// routing.SelectModel.
	Route func(task, agent, priority, model string) (routing.Selection, error)
	// Tmux is the persistent per-agent window manager (injected). Retained for
	// observation deployments; NOT the execution path — Dispatch does not drive
	// it.
	Tmux *tmux.TmuxManager
	// GetAdapter resolves an agent name to its executing adapter. Defaults to a
	// wrapper over agent.GetAdapter.
	GetAdapter func(name string) (agent.Adapter, error)
	// Mem is the shared-memory store recording intent and results (injected).
	// Exposed as the concrete store so callers (e.g. the MCP memory tool) can
	// Read/All the store directly; Dispatch itself only touches the lean
	// dispatchMem subset.
	Mem *memory.SharedMemory
	// mem is the lean surface Dispatch actually uses. It defaults to Mem; tests
	// may override it with a spy to assert no Read/All content pull occurs.
	mem dispatchMem
	// VerifyReviseRounds bounds the AUTOMATIC author-revision rounds triggered
	// when the objective verify gate fails (see dispatch). Its value is the max
	// number of automatic revision rounds attempted after an INITIAL verify
	// failure: each round re-dispatches the SAME author with the failed command
	// and its output fed back, then re-runs verify. When 0 it DEFAULTS to 1 (one
	// automatic retry); it is clamped to verifyReviseRoundsMax so it can NEVER
	// infinite-loop even if set absurdly high. The loop count depends ONLY on
	// verify outcomes, never on timing.
	VerifyReviseRounds int
}

// verifyReviseRoundsMax is the hard ceiling on automatic verify-revision rounds:
// no matter how large Orchestrator.VerifyReviseRounds is set, dispatch attempts
// at most this many automatic author revisions after an initial verify failure,
// guaranteeing termination.
const verifyReviseRoundsMax = 3

// verifyReviseRounds resolves the effective automatic-revision round cap from the
// configured field: 0 defaults to 1, negative is treated as 0->1, and any value
// above verifyReviseRoundsMax is clamped down to it.
func (o *Orchestrator) verifyReviseRounds() int {
	n := o.VerifyReviseRounds
	if n <= 0 {
		n = 1
	}
	if n > verifyReviseRoundsMax {
		n = verifyReviseRoundsMax
	}
	return n
}

// Result is the outcome of a Dispatch: the resolved routing plus the produced
// result, the shared-memory key the record lives under, and the parsed status
// and summary from the agent's response contract.
type Result struct {
	Agent      string
	Model      string
	Difficulty string
	Result     string
	Key        string
	Status     string
	Summary    string
	// Rationale is the routing decision's explanation (matched signals, total,
	// applied threshold, tier), carried through from routing.Select so the "why"
	// is recorded in memory and exposed by the MCP dispatch tool.
	Rationale routing.Rationale
	// Reviews carries EVERY cross-model reviewer's outcome (one reviewRecord per
	// reviewer, in sorted reviewer order — agent/model, verdict, per-severity
	// finding counts, errored flag). It is non-empty ONLY when a review:true
	// dispatch ran; a review-off Dispatch leaves it nil, keeping the prior Result
	// shape. Exposed so the host can see WHAT each reviewer found and gate the next
	// step on it, not just the coarse aggregate Status.
	Reviews []reviewRecord
	// Verify carries the OBJECTIVE machine-gate outcome when the dispatch was
	// given verify commands (nil otherwise, keeping the prior Result shape). It
	// records whether the caller-supplied tests/build/lint passed AFTER the final
	// author result; on failure the Result.Status is set to "verify_failed" so the
	// host can gate the next step on a real signal, not just the LLM verdict.
	Verify *VerifyResult
	// Files carries the git-derived manifest of paths created/modified/deleted
	// across the WHOLE dispatch pipeline (author + any review-revision), computed
	// by diffing a `git status` snapshot taken before runAuthor against one taken
	// just before returning. nil when cwd is not a git repo, git is unavailable,
	// or nothing changed — keeping the prior Result shape (additive, best-effort,
	// never fails the dispatch).
	Files []ChangedFile
	// VerifyRevisions counts the AUTOMATIC author-revision rounds that actually
	// ran because the verify gate failed (see dispatch). It is 0 on the no-verify
	// path and when verify passed on the first try — preserving the prior Result
	// shape — and rises to at most verifyReviseRoundsMax when verify keeps
	// failing. The count is deterministic: it depends only on verify outcomes.
	VerifyRevisions int
	// Isolation records the ephemeral-worktree outcome when the dispatch ran in
	// isolate mode (see DispatchIsolated); nil on a normal dispatch, preserving the
	// prior Result shape. When set, the caller's own working tree was NEVER written
	// by jindo — on success the changes live on Isolation.Branch for the HOST to
	// merge, and on failure/no-change the worktree and branch are discarded.
	Isolation *Isolation
	// Review makes the TRUST STATUS of a cross-model review EXPLICIT so a host
	// cannot mistake review=true (or a returned result) for a passed quality gate
	// when the review actually errored or found unresolved critical issues. It is
	// set ONLY on the review path (nil for a review-OFF dispatch, preserving the
	// prior Result shape) and answers, independently of Reviews' raw records,
	// whether the review actually completed and gated. See ReviewStatus.
	Review *ReviewStatus `json:"review_status,omitempty"`
}

// ReviewStatus is the explicit trust state of a cross-model review, surfaced so a
// host gates on whether the review actually RAN and PASSED rather than on the mere
// presence of review=true or of reviewer records. It is populated only when review
// was requested; review is ADVISORY and never fails the dispatch, so only
// GatePassed==true means the review ran to completion with no unresolved critical
// finding.
type ReviewStatus struct {
	// Requested is true whenever this status exists at all: it is set only on the
	// review path (review=true), so a non-nil ReviewStatus always means review was
	// asked for.
	Requested bool `json:"requested"`
	// Completed is true when at least one reviewer returned a parseable, non-errored
	// verdict (a real review came back). It is false when every reviewer errored or
	// produced an unparseable review, or when no cross-model reviewer was available.
	Completed bool `json:"completed"`
	// GatePassed is true only when the review both Completed AND left no unresolved
	// critical finding (Status != "review_failed"). A review that did not complete
	// never passes the gate. This is the ONLY field a host should treat as "review
	// gate passed"; review=true alone or a returned result must not be.
	GatePassed bool `json:"gate_passed"`
	// Confidence summarizes the two flags into one label: "review_failed" when a
	// critical finding survived the revision round (Status=="review_failed"),
	// "reviewed" when the gate passed, else "unverified" (review was requested but
	// did not complete).
	Confidence string `json:"confidence"`
}

// Isolation records the outcome of an isolate-mode dispatch (see
// DispatchIsolated): the dispatch ran inside an EPHEMERAL git worktree branched
// off the workdir repo's HEAD so a slow or host-aborted dispatch can NEVER leave
// partial changes in the caller's own working tree.
//
// On SUCCESS with changes the worktree's changes are committed on Branch and the
// worktree is then REMOVED (so WorktreePath stays empty) — the branch, carrying
// the commit, is all that remains and the HOST merges it (`git merge <branch>`).
// jindo NEVER auto-merges, so Merged is always false here; it is kept in the
// shape only so a future auto-merge could set it. On failure / no changes
// Committed is false and both the worktree and the throwaway branch are
// discarded, leaving the caller's tree pristine.
type Isolation struct {
	WorktreePath string `json:"worktree_path,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Committed    bool   `json:"committed"`
	Merged       bool   `json:"merged"`
	// Skipped is true only when isolation was REQUESTED (isolate default-on) but
	// could not be applied — the run happened IN PLACE instead. It stays false on
	// the DispatchIsolated success/no-change paths (which genuinely isolated). Set
	// by DispatchAuto when the workdir is not a git repository.
	Skipped bool `json:"skipped,omitempty"`
	// Reason explains why isolation was skipped (e.g. workdir is not a git repo);
	// non-empty only when Skipped is true.
	Reason string `json:"reason,omitempty"`
}

// New builds an Orchestrator over the given shared memory and tmux manager,
// defaulting Route to routing.SelectModel and GetAdapter to agent.GetAdapter. Tests
// may overwrite any field on the returned value.
//
// agent.GetAdapter returns the concrete *cliAdapter, whose type cannot be
// assigned to a func returning the agent.Adapter interface directly (Go
// function types are invariant), so the default is a thin adapting wrapper that
// widens the return to the interface.
func New(mem *memory.SharedMemory, tmuxMgr *tmux.TmuxManager) *Orchestrator {
	return &Orchestrator{
		Route: routing.SelectModel,
		Tmux:  tmuxMgr,
		GetAdapter: func(name string) (agent.Adapter, error) {
			return agent.GetAdapter(name)
		},
		Mem: mem,
	}
}

// dispatchStore returns the lean memory surface Dispatch uses: the test override
// if set, else the concrete Mem.
func (o *Orchestrator) dispatchStore() dispatchMem {
	if o.mem != nil {
		return o.mem
	}
	return o.Mem
}

// buildDispatchArgs decides, per routed agent, HOW the memory/response-contract
// instruction (sysPrompt) and the memory directory (memDir) reach the agent,
// AND how the agent is granted enough headless privilege to actually complete
// file-writing work without hanging on an unanswerable interactive approval
// prompt. It returns the actual task string to dispatch and the flag extras
// to pass.
//
// LIVE-CONFIRMED PERMISSION-GATE PROBLEM (2026-07): without an explicit
// permission/sandbox flag, all three CLIs default to a headless mode that
// still gates file writes/edits behind an approval prompt — and since
// headless dispatch has no TTY to answer it, the agent either reports "I
// need your approval" and does nothing (claude, codex) or, worse, SILENTLY
// redirects its work into an unrelated default location and still claims
// success (agy — see below). Each CLI's fix uses its OWN narrowest available
// flag, not a blanket "disable everything" bypass:
//
//   - claude: has --append-system-prompt AND --add-dir, so the instruction
//     rides the flag and the task text is sent UNCHANGED. Also gets
//     --permission-mode acceptEdits (auto-accepts file edits specifically,
//     narrower than --dangerously-skip-permissions) so headless file-writing
//     tasks actually complete instead of stalling on an approval request.
//   - agy: has --add-dir but NO system-prompt flag, so passing
//     --append-system-prompt would fail ("flags provided but not defined").
//     The instruction is therefore PREFIXED into the task text. LIVE-CONFIRMED
//     SEPARATE BUG: agy does NOT operate on the process's actual working
//     directory by default — with no workspace granted (or only a NESTED
//     subdirectory such as the memory dir granted), it silently falls back to
//     its own default scratch directory (~/.gemini/antigravity-cli/scratch)
//     and reports success there, never touching the real project files. Only
//     granting --add-dir pointed at the ACTUAL PROCESS CWD (not just the
//     nested memory subdirectory) makes it operate on the right directory.
//     So agy is granted --add-dir for BOTH cwd (the real project root it must
//     write into) and memDir (defensive: covers a memory root configured
//     outside cwd), plus --dangerously-skip-permissions (agy's only
//     available bypass — it has no scoped acceptEdits-equivalent).
//   - codex: has NO system-prompt flag; its ONLY instruction channel is the
//     [PROMPT] positional argument, so the instruction is PREFIXED into the
//     task text (same style as agy). Unlike the earlier assumption, codex DOES
//     support other flags (`codex exec --help` in full lists `-s/--sandbox`
//     and `--add-dir`) — it does not need a directory grant (jindo-mcp's cwd is
//     inherited by the subprocess and codex, unlike agy, correctly operates on
//     it by default), but it DOES need its sandbox elevated from the
//     directory-trust-dependent default to `-s workspace-write` (scoped to
//     the working directory + /tmp, NOT the "danger-full-access" mode) so file
//     writes succeed instead of failing with "read-only sandbox and approvals
//     are disabled".
//   - default (anything else): unchanged — no extras, task not prefixed.
//
// Callers must record the ORIGINAL task (not taskToSend) in memory; taskToSend
// is only the dispatch argument handed to RunWith.
//
// reviewMode narrows the same per-CLI privilege grant to READ-ONLY for a
// reviewer invocation, which must never write/edit files (see reviewWith):
//   - claude: --permission-mode plan (blocks all edit/execute tool calls
//     outright) plus --disallowedTools for the write-family tools directly,
//     as defense-in-depth on top of plan mode, replacing the author's
//     acceptEdits + sensitive-pattern-only disallow list.
//   - codex: -s read-only instead of -s workspace-write.
//   - agy: --mode plan (agy's plan execution mode, no edits) plus --sandbox
//     (terminal restrictions), and the cwd write grant is dropped so only the
//     read-only memory dir is added. This replaces the prior behavior where the
//     agy reviewer inherited the author's --dangerously-skip-permissions — i.e.
//     a full permission bypass on a reviewer that must never write. It no longer
//     relies solely on the prompt-level "do not modify files" instruction baked
//     into agentproto.BuildReviewPrompt; the CLI flags now enforce it too.
//
// effort, when non-empty, is the reasoning-effort level for THIS dispatch (see
// EffortForDifficulty / the DispatchModel host override). It is a dispatch
// dimension separate from the model and applied per-CLI with each CLI's own
// flag: claude via `--effort <level>` (low/medium/high/xhigh/max), codex via
// `-c model_reasoning_effort=<level>` (codex has no "max", so effortForCodex
// clamps "max"->"xhigh"). agy encodes effort in its model display name and has
// no flag, so it (and the default case) ignore effort. effort == "" adds NO
// flag on any branch — byte-identical to the pre-effort dispatch.
// workdir, when non-empty, is the EXPLICIT per-dispatch working directory (see
// DispatchModel). It gates the additional claude/codex write-access grants
// (claude --add-dir workdir; codex exec -C workdir) so the sub-agent is anchored
// in the caller's target directory. It is deliberately SEPARATE from cwd: cwd is
// the resolved process working directory (workdir when set, else jindo's
// os.Getwd()) and is always non-empty, so it cannot distinguish "no workdir
// given" from the getwd fallback; workdir can. workdir == "" adds NO extra grant
// on any branch, byte-identical to the pre-workdir args (agy still uses cwd for
// its pre-existing --add-dir cwd grant, unchanged).
func buildDispatchArgs(agentName, task, sysPrompt, memDir, cwd string, reviewMode bool, effort, workdir string) (taskToSend string, extra []string) {
	switch agentName {
	case "claude":
		extra := []string{
			"--append-system-prompt", sysPrompt,
			"--add-dir", memDir,
			// Without this, claude reloads the host's project .mcp.json (which
			// registers jindo itself), causing recursive/redundant MCP server
			// spawns. --strict-mcp-config with no --mcp-config yields zero MCP
			// servers for the sub-agent.
			"--strict-mcp-config",
		}
		// Grant the sub-agent write access to the EXPLICIT per-dispatch working
		// directory (in addition to the memory dir above). workdir == "" preserves
		// the prior args exactly.
		if workdir != "" {
			extra = append(extra, "--add-dir", workdir)
		}
		// The reasoning-effort flag rides the same extras on both the review and
		// the normal path; reviewers pass effort=="" so this is a no-op for them,
		// and only the author supplies a non-empty effort.
		if effort != "" {
			extra = append(extra, "--effort", effort)
		}
		if reviewMode {
			extra = append(extra, "--permission-mode", "plan",
				"--disallowedTools", "Write", "Edit", "MultiEdit", "NotebookEdit")
			return task, extra
		}
		// Defense-in-depth on top of the policy.Check gate in Dispatch: that
		// gate only inspects the task TEXT given to it, so it cannot catch
		// claude deciding mid-task to touch a sensitive path never mentioned
		// there. --disallowedTools blocks the write/edit itself regardless of
		// why claude attempted it (live-confirmed: ".env" is refused with
		// this flag present). codex/agy have no equivalent flag, so they rely
		// solely on the Dispatch-level gate.
		extra = append(extra, "--permission-mode", "acceptEdits")
		extra = append(extra, policy.ClaudeDisallowedToolArgs()...)
		return task, extra
	case "agy":
		// agy has no --append-system-prompt, so the instruction rides the task text.
		taskToSend := sysPrompt + "\n\n" + task
		if reviewMode {
			// Read-only reviewer/planner: grant ONLY the memory dir (to read the
			// shared context) and use agy's plan mode + sandbox so it cannot
			// write/edit or run unrestricted shell — mirroring claude
			// (--permission-mode plan) and codex (-s read-only). Deliberately NO
			// --dangerously-skip-permissions and NO cwd write grant: previously the
			// agy reviewer inherited the author's full permission bypass, letting a
			// reviewer modify the caller's tree. A reviewer must never write.
			return taskToSend, []string{
				"--add-dir", memDir,
				"--mode", "plan",
				"--sandbox",
			}
		}
		// Author: needs write access to the working tree. NOTE: agy's only
		// confirmed headless write-enabler today is --dangerously-skip-permissions
		// (its narrower --mode accept-edits is not yet verified). This blanket
		// bypass is a known P0 weakness — a mis-routed task reaching agy authors
		// with all permissions skipped. The safe fix is to run agy authors inside
		// an isolate worktree (see DispatchIsolated) and/or verify --mode
		// accept-edits so the bypass can be dropped; left unchanged here to avoid
		// regressing agy authoring until that is verified.
		return taskToSend, []string{
			"--add-dir", cwd,
			"--add-dir", memDir,
			"--dangerously-skip-permissions",
		}
	case "codex":
		sandbox := "workspace-write"
		if reviewMode {
			sandbox = "read-only"
		}
		// codex accepts -c as an exec config flag; add the reasoning-effort config
		// (clamped by effortForCodex) before -s so both ride the same extras. With
		// effort=="" the extras are exactly {"-s", sandbox} as before.
		var extra []string
		if effort != "" {
			extra = append(extra, "-c", "model_reasoning_effort="+effortForCodex(effort))
		}
		extra = append(extra, "-s", sandbox)
		// Anchor codex exec's working root to the EXPLICIT per-dispatch workdir via
		// -C (codex's working-directory flag). workdir == "" preserves the prior
		// args exactly (codex then inherits jindo's process cwd, as before).
		if workdir != "" {
			extra = append(extra, "-C", workdir)
		}
		// Skip ~/.codex/config.toml so the sub-agent doesn't reload the host's
		// mcp_servers (incl. jindo itself) — same recursion/overhead concern as
		// claude's --strict-mcp-config above. Auth still works via CODEX_HOME.
		extra = append(extra, "--ignore-user-config")
		return sysPrompt + "\n\n" + task, extra
	default: // anything else
		return task, nil
	}
}

// effortForCodex maps a jindo reasoning-effort level to codex's supported set.
// claude supports "max" but codex does not (its ceiling is "xhigh"), so "max"
// is clamped to "xhigh"; every other level (none/minimal/low/medium/high/xhigh)
// passes through unchanged. It exists only for this single incompatibility so
// the shared effort vocabulary stays claude's and codex is adapted at the edge.
func effortForCodex(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort
}

// Dispatch routes, dispatches, and executes task under the headless contract,
// with NO peer review (the backward-compatible entry point; its behavior and
// its dispatch.log line are byte-for-byte unchanged from before review existed).
// See dispatch for the shared core.
//
// LEANNESS: Dispatch never calls Mem.Read/All to pull entry content into the
// orchestrator; it only passes the memory directory path onward via the system
// prompt and --add-dir. The mem surface it uses (dispatchMem) does not even
// expose Read/All.
//
// Hard errors (routing failure, unknown adapter, adapter run failure) are
// propagated. Memory side-effect errors are propagated too rather than silently
// swallowed, so a corrupted store surfaces to the caller.
func (o *Orchestrator) Dispatch(task, agentName, priority string) (Result, error) {
	return o.dispatch(task, agentName, priority, "", "", "", false, nil, "")
}

// DispatchWithReview runs the same author dispatch as Dispatch, then a
// cross-model peer-review stage over the author's result (see dispatch). It is
// the opt-in entry point (review is DEFAULT OFF via Dispatch); callers that want
// review — e.g. the MCP dispatch tool exposing a review flag — call this instead.
func (o *Orchestrator) DispatchWithReview(task, agentName, priority string) (Result, error) {
	return o.dispatch(task, agentName, priority, "", "", "", true, nil, "")
}

// DispatchModel is the model-aware entry point: it runs the same pipeline as
// Dispatch/DispatchWithReview but pins the author run to the exact model (when
// model != "", score-based routing is bypassed; see routing.SelectModel), takes
// review explicitly, and threads optional per-task guidance (see runAuthor) into
// the author's system prompt. model == "" and guidance == "" reproduce
// Dispatch/DispatchWithReview exactly.
//
// verify, when non-empty, is a list of objective verification commands (see
// verify.go) run in the final author's working directory AFTER the review
// pipeline completes; on failure the returned Result.Status is "verify_failed".
// nil/empty reproduces the pre-verify behavior exactly.
//
// effort, when non-empty, is a host OVERRIDE of the per-difficulty reasoning
// effort applied to the AUTHOR run (see runAuthor): it wins over
// routing.EffortForDifficulty(tier). effort == "" falls back to the tier
// default, and when that is also "" no effort flag is added at all — exactly
// the pre-effort behavior.
//
// workdir, when non-empty, is the per-dispatch working directory the author
// sub-agent is anchored in (process cwd + granted write access) and the verify
// commands run in; it is created if missing. workdir == "" reproduces the prior
// behavior exactly (the author and verify run in jindo's own os.Getwd()).
func (o *Orchestrator) DispatchModel(task, agentName, priority, model, guidance, effort string, review bool, verify []string, workdir string) (Result, error) {
	return o.dispatch(task, agentName, priority, model, guidance, effort, review, verify, workdir)
}

// DispatchIsolated runs a write dispatch inside an EPHEMERAL git worktree so the
// caller's own working tree is NEVER written by jindo. It solves the failure mode
// where a host aborts a slow dispatch (idle timeout) while the sub-agent keeps
// writing files into the caller's checkout: because the sub-agent only ever
// writes into a throwaway worktree, an abort leaves the caller's tree untouched.
//
// It is a THIN wrapper around DispatchModel (reused VERBATIM): it creates a fresh
// worktree branched off the workdir repo's HEAD, runs the whole author+review+
// verify pipeline there, and then either commits the result onto a returned
// branch (on success WITH changes) or discards it entirely (on failure / no
// changes). jindo does NOT auto-merge — on success the HOST merges the returned
// branch (`git merge <branch>`), so the caller's tree is only ever modified by
// that host-side merge, never by a half-finished dispatch.
//
// workdir MUST be inside a git repository (it anchors the worktree's HEAD); an
// empty workdir or a non-repo workdir is refused with an error. All git plumbing
// runs via exec.Command with -C (never a shell), mirroring changedfiles.go for
// security parity.
func (o *Orchestrator) DispatchIsolated(task, agentName, priority, model, guidance, effort string, review bool, verify []string, workdir string) (Result, error) {
	if workdir == "" {
		return Result{}, fmt.Errorf("isolate requires workdir to be inside a git repository")
	}
	repoRoot, err := gitCmd(workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Result{}, fmt.Errorf("isolate requires workdir to be inside a git repository")
	}

	// Derive a unique worktree name WITHOUT persisting anything to the shared
	// store: os.MkdirTemp reserves a collision-free directory under .jindo/wt.
	// (Using the store's AllocKey here would write a reserved memory entry that
	// the real dispatch's OWN key never reuses, leaking a phantom task:iso:N
	// record on every isolate run.) MkdirTemp's suffix is filesystem- and
	// git-ref-safe (letters+digits only), so the branch name needs no further
	// sanitization.
	mem := o.dispatchStore()
	wtParent := filepath.Join(repoRoot, ".jindo", "wt")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		return Result{}, fmt.Errorf("isolate: create worktree parent: %w", err)
	}
	worktreePath, err := os.MkdirTemp(wtParent, "iso-")
	if err != nil {
		return Result{}, fmt.Errorf("isolate: reserve worktree name: %w", err)
	}
	// git worktree add requires the target path NOT to exist yet; MkdirTemp
	// created it only to reserve the unique name, so remove the empty dir first.
	if err := os.Remove(worktreePath); err != nil {
		return Result{}, fmt.Errorf("isolate: free reserved worktree name: %w", err)
	}
	token := filepath.Base(worktreePath)
	branch := "jindo/" + token

	// Create the worktree + throwaway branch off HEAD. On failure nothing has been
	// created to clean up, so return the error directly.
	if out, err := gitCmd(repoRoot, "worktree", "add", "-b", branch, worktreePath, "HEAD"); err != nil {
		return Result{}, fmt.Errorf("isolate: worktree add: %w: %s", err, out)
	}

	// Run the REAL dispatch inside the worktree (DispatchModel reused verbatim).
	res, derr := o.DispatchModel(task, agentName, priority, model, guidance, effort, review, verify, worktreePath)
	if derr != nil {
		// The dispatch itself errored (routing/adapter/policy): discard BOTH the
		// worktree and the branch so the caller's tree stays pristine, then surface
		// the error (mirroring the non-isolate propagation of a DispatchModel error).
		o.discardWorktree(repoRoot, worktreePath, branch)
		return Result{}, derr
	}

	// Success requires a non-error status and — when a verify gate ran — a passing
	// gate: the same signal the host uses to decide whether to keep the work.
	success := res.Status != "error" && (res.Verify == nil || res.Verify.Passed)

	// "Has changes" is decided from the worktree's OWN git status (reusing
	// gitStatusSnapshot's exec style); an empty snapshot means the sub-agent wrote
	// nothing worth keeping.
	snap, ok := gitStatusSnapshot(worktreePath)
	hasChanges := ok && len(snap) > 0

	if success && hasChanges {
		// Commit the worktree's changes onto the branch, then REMOVE the worktree
		// (the branch, carrying the commit, persists for the host to merge). A git
		// plumbing failure here best-effort discards the worktree and returns the
		// error so the caller sees isolate failed rather than a half state.
		if out, err := gitCmd(worktreePath, "add", "-A"); err != nil {
			o.discardWorktree(repoRoot, worktreePath, branch)
			return Result{}, fmt.Errorf("isolate: git add: %w: %s", err, out)
		}
		if out, err := gitCmd(worktreePath, "-c", "user.email=jindo@local", "-c", "user.name=jindo",
			"commit", "-m", "jindo isolate dispatch "+token); err != nil {
			o.discardWorktree(repoRoot, worktreePath, branch)
			return Result{}, fmt.Errorf("isolate: git commit: %w: %s", err, out)
		}
		if out, err := gitCmd(repoRoot, "worktree", "remove", "--force", worktreePath); err != nil {
			// Best-effort: the commit is safe on the branch; a leftover worktree is
			// diagnosable but is not a dispatch failure.
			_ = mem.AppendNote("orchestrator", fmt.Sprintf("isolate: worktree remove failed for %s: %v: %s", worktreePath, err, out))
		}
		res.Isolation = &Isolation{Branch: branch, Committed: true, Merged: false}
		return res, nil
	}

	// Failure or no changes: discard BOTH the worktree and the throwaway branch so
	// the caller's tree stays pristine, and report that nothing was committed.
	o.discardWorktree(repoRoot, worktreePath, branch)
	res.Isolation = &Isolation{Committed: false, Merged: false}
	return res, nil
}

// DispatchAuto is the isolate-aware entry point the MCP dispatch tools use. It
// only CHOOSES between the two existing execution paths (DispatchModel in place
// vs DispatchIsolated in an ephemeral worktree) plus a git guard with a safe
// fallback; it adds no dispatch behavior of its own.
//
//   - isolate == false: run in place via DispatchModel, byte-identical to today.
//   - isolate == true AND the workdir is inside a git repository: run isolated
//     via DispatchIsolated (worktree + commit to a jindo/iso-* branch for the
//     HOST to merge), so an aborted/mis-routed write never leaves partial changes
//     in the caller's tree.
//   - isolate == true but the workdir is NOT a git repository (or git errors):
//     FALL BACK to DispatchModel (in place) rather than erroring — there is no
//     repo to branch off — and mark the returned Result.Isolation Skipped with a
//     Reason so the host learns the run was not actually isolated.
//
// The effective workdir is workdir when non-empty, else os.Getwd() (matching the
// in-place path's own default). The git check reuses gitCmd (no shell), mirroring
// DispatchIsolated's own repo probe.
func (o *Orchestrator) DispatchAuto(task, agentName, priority, model, guidance, effort string, review bool, verify []string, workdir string, isolate bool) (Result, error) {
	if !isolate {
		return o.DispatchModel(task, agentName, priority, model, guidance, effort, review, verify, workdir)
	}

	// Resolve the directory the git guard inspects: the explicit workdir, else
	// jindo's own cwd (the same directory the in-place path would run in).
	dir := workdir
	if dir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dir = cwd
		}
	}

	// Isolation needs a git repo to branch off. If the workdir is not inside one
	// (or git errors), run in place instead of refusing, and record the skip.
	if _, err := gitCmd(dir, "rev-parse", "--show-toplevel"); err != nil {
		res, derr := o.DispatchModel(task, agentName, priority, model, guidance, effort, review, verify, workdir)
		if derr != nil {
			return res, derr
		}
		res.Isolation = &Isolation{Skipped: true, Reason: "workdir is not a git repository; ran in place"}
		_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("isolate: skipped for %q (not a git repository); ran in place", dir))
		return res, nil
	}

	// Pass the RESOLVED dir (workdir, else cwd): identical to the original workdir
	// when one was given, and it lets isolation run in cwd — which the guard just
	// confirmed is a repo — instead of DispatchIsolated refusing an empty workdir.
	return o.DispatchIsolated(task, agentName, priority, model, guidance, effort, review, verify, dir)
}

// gitCmd runs a git subcommand in dir via exec.Command (never a shell, mirroring
// changedfiles.go's exec style) and returns its trimmed combined output and any
// error. Used only for DispatchIsolated's worktree plumbing.
func gitCmd(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// sanitizeToken turns an allocated dispatch key (e.g. "task:claude:7") into a
// component safe for a git branch name and worktree path by replacing every ':'
// (illegal in a git ref) with '-'.

// discardWorktree best-effort removes the ephemeral worktree AND deletes the
// throwaway branch, leaving the caller's repo as if the isolate dispatch never
// ran. Failures are recorded as best-effort notes and never returned, since this
// is the cleanup path.
func (o *Orchestrator) discardWorktree(repoRoot, worktreePath, branch string) {
	if out, err := gitCmd(repoRoot, "worktree", "remove", "--force", worktreePath); err != nil {
		_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("isolate: worktree remove failed for %s: %v: %s", worktreePath, err, out))
	}
	if out, err := gitCmd(repoRoot, "branch", "-D", branch); err != nil {
		_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("isolate: branch delete failed for %s: %v: %s", branch, err, out))
	}
}

// PlanResult is the outcome of a Plan: the agent+model that produced the plan,
// the parsed ordered steps, the planner's summary, and the shared-memory key the
// persisted plan lives under (empty when persistence failed — see Plan). Steps
// carry per-step difficulty/suggested_model/suggested_verify/depends_on so the
// caller can dispatch each step at the right tier and gate it on the right check.
type PlanResult struct {
	Agent   string
	Model   string
	Steps   []agentproto.PlanStep
	Summary string
	Key     string
	// Spec is the caller-provided clarified intent anchoring the plan (carried
	// through so the caller can persist it); empty when none was supplied.
	Spec string
	// VerifyCmds is the planner-produced INTEGRATION gate for the whole goal
	// (distinct from each step's suggested_verify, which gates only that step).
	VerifyCmds []string
}

// planDirective is the short positional prompt handed to the planner adapter;
// the substantive plan instruction (schema + goal + memory-read) rides the
// BuildPlanPrompt sysPrompt through buildDispatchArgs, exactly as the author's
// BuildSystemPrompt and the reviewer's BuildReviewPrompt do. A non-empty
// positional is required because some CLIs (e.g. claude -p) demand a prompt
// argument even when the instruction travels via a flag.
const planDirective = "Produce the step plan described in your instructions and end with the plan JSON block."

// Plan decomposes goal into an ordered, structured step list using a capable
// model, persists it to shared memory, and returns it. It is purely additive:
// planning becomes a tool + state instead of only host prose, and it does NOT
// change any existing dispatch behavior.
//
// The planner runs through the SAME adapter path as an author dispatch but
// READ-ONLY (buildDispatchArgs reviewMode=true, mirroring reviewWith): a plan
// must not edit files. It reads the shared memory dir (granted via --add-dir)
// so the plan builds on prior context.
//
// Model selection (planning is the highest-leverage step, so it defaults to a
// strong model): if model != "" it is pinned; else the HARD-tier model of the
// chosen agent is used; else agent defaults to "claude" and its hard model.
//
// Persistence is best-effort: the parsed plan is always returned even if the
// memory write fails, but a persist failure is surfaced as a best-effort note
// and leaves Key empty. A planner run that produces no parseable plan is a hard
// error (nothing useful to return).
func (o *Orchestrator) Plan(goal, spec, agent, model string) (PlanResult, error) {
	// Resolve agent+model. A pinned model wins as-is; otherwise derive the
	// agent's hard-tier model (defaulting the agent to claude). If the agent has
	// no hard slot the model stays empty and the adapter routes to its default —
	// better than refusing, and it never happens for the known agents.
	if agent == "" {
		agent = "claude"
	}
	if model == "" {
		if tiers, ok := routing.AgentsModels()[agent]; ok {
			model = tiers["hard"]
		}
	}

	mem := o.dispatchStore()
	memDir := mem.Root()
	sysPrompt := agentproto.BuildPlanPrompt(goal, memDir)

	// The process working directory the adapter runs in (see buildDispatchArgs);
	// fall back to memDir if Getwd fails, matching runAuthor.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = memDir
	}

	// Read-only planner run: same per-CLI seam as reviewWith (reviewMode=true), so
	// the planner can read memory via --add-dir but cannot write/edit files.
	taskToSend, extra := buildDispatchArgs(agent, planDirective, sysPrompt, memDir, cwd, true, "", "")

	ad, err := o.GetAdapter(agent)
	if err != nil {
		return PlanResult{}, err
	}

	stdout, err := ad.RunWith(taskToSend, model, extra)
	if err != nil {
		return PlanResult{}, fmt.Errorf("orchestrator: planner %q run failed: %w", agent, err)
	}

	steps, summary, verifyCmds, ok := agentproto.ParsePlanResponse(stdout)
	if !ok {
		return PlanResult{}, fmt.Errorf("orchestrator: planner produced no parseable plan")
	}

	res := PlanResult{Agent: agent, Model: model, Steps: steps, Summary: summary, Spec: spec, VerifyCmds: verifyCmds}

	// Persist the plan under an agent-owned, collision-free key so later
	// dispatches can read it. Best-effort: a persist failure still returns the
	// parsed plan, but is surfaced as a note (never silently swallowed).
	key, err := mem.AllocKey(agent)
	if err != nil {
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("plan persist: alloc key failed: %v", err))
		return res, nil
	}
	planValue := map[string]any{
		"goal":    goal,
		"steps":   steps,
		"summary": summary,
	}
	if err := mem.Upsert(key, planValue, agent); err != nil {
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("plan persist: upsert %s failed: %v", key, err))
		return res, nil
	}
	res.Key = key
	return res, nil
}

// dispatch is the pipeline shared by Dispatch and DispatchWithReview: it runs the
// author (runAuthor) and, when review is true, follows with a best-effort
// cross-model peer-review stage.
//
// The author run's dispatch.log line is written HERE (not inside runAuthor) so a
// single consolidated line is emitted per pipeline: review-OFF writes the base
// entry (Review nil => omitempty => byte-identical to the pre-review log line);
// review-ON writes the same entry augmented with the Review summary.
//
// Review semantics (all best-effort — a reviewer failure NEVER fails the
// dispatch): after the author's authoritative record is written, EVERY
// cross-model reviewer (routing.SelectReviewers, all agents except the author)
// reviews the result CONCURRENTLY. If ANY reviewer reports a "critical" finding
// it forces EXACTLY ONE author re-dispatch (via the non-reviewing core, the
// union of all reviewers' findings appended to the task) followed by ONE
// concurrent re-review by the same reviewer set; if that re-review is still
// critical the returned Result.Status is "review_failed". There is never more
// than one revision round and no recursion. Each reviewer's outcome is exposed on
// Result.Reviews; the aggregate (worst verdict, summed findings) drives the
// dispatch.log line and status.
func (o *Orchestrator) dispatch(task, agentName, priority, model, guidance, effort string, review bool, verify []string, workdir string) (Result, error) {
	// Validate the objective verify gate BEFORE running anything (routing, the
	// author, or the review), so an invalid/unsafe command list refuses the whole
	// dispatch rather than doing work we would then have to discard. This mirrors
	// the sensitive-path gate's "refuse before any side effect" placement.
	if err := ValidateVerifyCmds(verify); err != nil {
		return Result{}, err
	}

	// Resolve the effective working directory ONCE: a non-empty workdir is the
	// per-dispatch anchor (created if missing — a normal error if that fails, so
	// an unusable workdir refuses the dispatch before any side effect), used for
	// BOTH the git changed-files snapshot base and the author run below. workdir
	// == "" preserves the prior behavior exactly: fall back to jindo's os.Getwd()
	// (empty on failure), matching what runAuthor itself resolves.
	var cwd string
	if workdir != "" {
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			return Result{}, fmt.Errorf("orchestrator: create workdir %q: %w", workdir, err)
		}
		cwd = workdir
	} else {
		wd, err := os.Getwd()
		if err != nil {
			wd = ""
		}
		cwd = wd
	}

	// Best-effort changed-files manifest: snapshot git status BEFORE the author
	// runs, in the effective working dir resolved above. ok is false outside a
	// git repo, in which case Result.Files simply stays nil below — this never
	// fails the dispatch.
	before, filesOK := gitStatusSnapshot(cwd)

	ar, err := o.runAuthor(task, agentName, priority, model, guidance, effort, workdir)
	if err != nil {
		return Result{}, err
	}

	// The FINAL author outcome whose working directory the verify gate runs in:
	// the possibly-revised author when review ran, else the sole author. cwd is
	// the process cwd captured in runAuthor (see authorOutcome.cwd).
	var res Result
	var final authorOutcome
	if !review {
		o.writeDispatchLog(ar.memDir, ar.logEntry)
		res, final = ar.result, ar
	} else {
		// Feed the reviewer the REAL artifacts, not just the self-report: the
		// author's changed-file paths (the same before/cwd snapshot G6 uses for
		// Result.Files). ExecOutput stays empty here — the verify gate runs after
		// review (below), so there is no pre-review verify output to include.
		var arts agentproto.ReviewArtifacts
		if filesOK {
			arts.ChangedFiles = changedFilePaths(changedFilesSince(cwd, before))
		}
		reviewed, agg, per := o.runReviews(task, priority, ar, arts)
		res, err = o.finishReviewed(reviewed, agg, per)
		if err != nil {
			return Result{}, err
		}
		final = reviewed
	}

	// Objective machine gate: run the caller's verify commands on the FINAL
	// author result (after review, so it gates the possibly-revised code). On an
	// initial failure, feed the failed command + output back to the SAME author
	// for up to verifyReviseRounds() BOUNDED automatic revision rounds, re-running
	// verify in each revised author's cwd. The loop is DETERMINISTIC: it turns
	// only on verify outcomes, and the round cap guarantees termination. The
	// author's result payload is always kept; the status is flipped to
	// "verify_failed" only if verify still fails after the last allowed round.
	if len(verify) > 0 {
		vr := runVerify(final.cwd, verify)
		res.Verify = &vr
		if !vr.Passed {
			maxRounds := o.verifyReviseRounds()
			for round := 1; round <= maxRounds; round++ {
				// Re-dispatch to the SAME author with the SAME model and guidance,
				// the failed verify block appended so it sees what to fix. A
				// re-dispatch failure ends the automatic revision (it is not a Go
				// error of the whole dispatch); the last verify result stands.
				revisedTask := task + "\n\n" + renderVerifyFailure(vr)
				ar2, rerr := o.runAuthor(revisedTask, final.result.Agent, priority, model, guidance, effort, workdir)
				if rerr != nil {
					_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("verify: re-dispatch failed for %s: %v", final.key, rerr))
					break
				}
				// Adopt the revised author's result as the pipeline outcome, but
				// preserve the review context already gathered (a verify revision
				// re-runs the author + verify only, not the review stage).
				reviews := res.Reviews
				res = ar2.result
				res.Reviews = reviews
				final = ar2
				vr = runVerify(final.cwd, verify)
				res.Verify = &vr
				res.VerifyRevisions = round
				if vr.Passed {
					break
				}
			}
			if !res.Verify.Passed {
				res.Status = "verify_failed"
				// FIX C — coherent Result: a verify-forced revision replaced res
				// (summary/status) with the REVIEWLESS revision round while keeping
				// res.Reviews from the PRE-revision reviewed round. Left as-is the
				// response would show populated reviews next to a summary claiming
				// review did not run. When reviews are present, replace the revision
				// agent's raw summary with a jindo-composed one that states plainly
				// that the review was of the pre-revision result. (No reviews => keep
				// the revision agent's summary.)
				if len(res.Reviews) > 0 {
					res.Summary = fmt.Sprintf("verify gate failed after %d automatic revision round(s); returning the revised result. The peer review in `reviews` was of the PRE-revision result.", res.VerifyRevisions)
				}
			}
		}
	}

	// Capture the NET file changes of the whole pipeline (author + any
	// review-revision) now, just before returning the successful Result.
	if filesOK {
		res.Files = changedFilesSince(cwd, before)
	}

	// FIX B — memory status sync: the pipeline may have flipped res.Status past
	// what the author's authoritative record captured (e.g. "error"/"ok" ->
	// "verify_failed" / "review_failed"), so the persisted record would otherwise
	// disagree with the returned status. Reconcile the authoritative record to
	// the final status. Best-effort: a persist failure records a note and never
	// fails the dispatch (the returned Result already carries the true status).
	if final.key != "" && final.authRecord != nil && final.authRecord["status"] != res.Status {
		final.authRecord["status"] = res.Status
		if err := o.dispatchStore().Upsert(final.key, final.authRecord, final.result.Agent); err != nil {
			_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("status sync: upsert %s failed: %v", final.key, err))
		}
	}
	return res, nil
}

// authorOutcome carries everything a completed (non-reviewing) author run
// produces that the pipeline needs afterward: the caller-facing Result, the
// memory root and process cwd (so a review can dispatch a reviewer the same way),
// the owned dispatch key, the authoritative record map written under that key (so
// a review entry can be added to it), the base dispatch.log entry (NOT yet
// written — the pipeline writes it once, possibly augmented with the review), and
// the guidance the author was dispatched with (so a review-forced revision
// re-dispatch reuses the same task-specific guidance rather than losing it).
type authorOutcome struct {
	result     Result
	memDir     string
	cwd        string
	key        string
	authRecord map[string]any
	logEntry   dispatchLogEntry
	guidance   string
	// effort is the raw host effort OVERRIDE the author was dispatched with (""
	// when none). A review- or verify-forced re-dispatch reuses it (like
	// guidance) so the override survives a revision rather than silently
	// dropping back to the tier default.
	effort string
	// workdir is the EXPLICIT per-dispatch working directory the author was
	// dispatched with ("" when none). A review- or verify-forced re-dispatch
	// reuses it (like guidance/effort) so the revision runs in — and grants the
	// sub-agent — the same directory rather than silently falling back to
	// jindo's cwd. Distinct from cwd (which is the resolved dir, never empty).
	workdir string
}

// runAuthor is the non-reviewing author core: the full headless-contract dispatch
// (route -> allocate an agent-partitioned dispatch key -> write intent -> run the
// adapter, handing it the memory DIRECTORY so it reads prior context itself ->
// parse the response -> fan out memory updates -> persist the authoritative
// record -> self-bound the store). It does NOT write the success-path dispatch.log
// line; it returns the assembled entry so the caller writes one consolidated line
// per pipeline. On a RunWith failure it DOES write the error-path log line (same
// as before) and returns the error.
//
// It is the entry point BOTH for a normal dispatch and for a review-forced
// re-dispatch, so the sensitive-path gate and all memory writes apply uniformly.
//
// guidance, when non-empty, is caller-supplied task-specific instruction (e.g.
// language conventions, API contract, skill content) appended as an extra
// section onto the base BuildSystemPrompt — it does not alter the base
// read-memory/do-work/JSON contract. guidance == "" reproduces the prior system
// prompt byte-for-byte.
//
// effort is the host OVERRIDE of the reasoning-effort level: when non-empty it
// is applied verbatim; when empty it falls back to
// routing.EffortForDifficulty(route.Difficulty), the per-tier default. The
// resolved effort is passed to buildDispatchArgs (which emits the per-CLI
// flag); a resolved "" adds no flag. The raw host override is carried on the
// returned authorOutcome so a review- or verify-forced re-dispatch keeps the
// same override rather than silently dropping it.
//
// workdir, when non-empty, is the per-dispatch working directory: it is used as
// the author's process cwd (bound on the adapter via SetDir before RunWith),
// stored on authorOutcome.cwd so the verify gate runs there, and granted to the
// sub-agent via buildDispatchArgs. workdir == "" reproduces the prior behavior
// exactly: cwd falls back to os.Getwd() (then memDir).
func (o *Orchestrator) runAuthor(task, agentName, priority, model, guidance, effort, workdir string) (authorOutcome, error) {
	// Sensitive-path gate: runs BEFORE routing/memory writes/adapter dispatch,
	// so a task referencing a sensitive file (.env, .mcp.json,
	// .claude/settings.local.json, ssh keys, etc.) never reaches any CLI, on
	// any of the three adapters. See internal/policy for why this must live
	// here rather than relying on a per-CLI flag: only claude has one
	// (--disallowedTools, added as defense-in-depth below), codex and agy do
	// not.
	if blocked, pattern := policy.Check(task); blocked {
		return authorOutcome{}, &policy.BlockedError{Task: task, Pattern: pattern}
	}

	route, err := o.Route(task, agentName, priority, model)
	if err != nil {
		return authorOutcome{}, err
	}

	mem := o.dispatchStore()

	// Allocate a collision-free, agent-partitioned dispatch key. This replaces
	// the old in-struct "task:<n>" counter: partitioning by the executing agent
	// means concurrent orchestrators/agents never collide on an index, and the
	// key doubles as the dispatch-id.
	key, err := mem.AllocKey(route.Agent)
	if err != nil {
		return authorOutcome{}, fmt.Errorf("orchestrator: alloc dispatch key: %w", err)
	}

	// Hand the agent the memory DIRECTORY, not its content. BuildSystemPrompt
	// instructs the agent to read the bounded store itself before working.
	memDir := mem.Root()
	sysPrompt := agentproto.BuildSystemPrompt(memDir)
	if guidance != "" {
		sysPrompt += "\n\nTASK-SPECIFIC GUIDANCE (from the caller; follow it for THIS task):\n" + guidance + "\n"
	}
	// Curated cross-agent insight injection: instead of relying only on the
	// agent scanning the whole store, retrieve the few learnings most relevant
	// to THIS task (from any prior agent/model) and inject them as a short
	// brief. Read-only and bounded (top-K); a store with no relevant insight
	// injects nothing, keeping the prompt byte-identical to the pre-insight
	// behavior. Best-effort: a retrieval error never blocks the dispatch.
	// injectedInsightTexts captures the exact insight texts injected into this
	// dispatch's prompt so that, when the author's notes are contributed back to
	// the insight layer, a note that merely parrots an injected hint does not
	// reinforce it (see the AddInsightWith call below). Empty when nothing was
	// injected, in which case the feedback path stays byte-identical.
	var injectedInsightTexts []string
	if insights, ierr := mem.RetrieveInsights(task, insightInjectK); ierr == nil && len(insights) > 0 {
		sysPrompt += renderInsightBrief(insights)
		for _, in := range insights {
			injectedInsightTexts = append(injectedInsightTexts, in.Text)
		}
	}

	// The actual process working directory — needed so agy (which, unlike
	// claude/codex, does not default to operating on the real cwd — see
	// buildDispatchArgs) can be explicitly granted the real project directory
	// instead of silently falling back to its own scratch location. A non-empty
	// per-dispatch workdir wins as the anchor; otherwise fall back to os.Getwd()
	// (then memDir if Getwd fails), matching the pre-workdir behavior.
	var cwd string
	if workdir != "" {
		cwd = workdir
	} else {
		wd, werr := os.Getwd()
		if werr != nil {
			wd = memDir
		}
		cwd = wd
	}

	// Resolve the reasoning effort for this run: a non-empty host override wins;
	// otherwise use the per-tier default for the routed difficulty. A "" result
	// (no override and no tier default) means buildDispatchArgs adds no flag.
	resolvedEffort := effort
	if resolvedEffort == "" {
		resolvedEffort = routing.EffortForDifficulty(route.Difficulty)
	}

	// Decide, per agent, how the instruction reaches it AND how it is granted
	// enough headless privilege to actually complete the work (see
	// buildDispatchArgs for the full per-CLI rationale, including the
	// live-confirmed permission-gate and agy directory-targeting bugs). Only
	// taskToSend is dispatched — the ORIGINAL task is what the memory records
	// below persist.
	taskToSend, extra := buildDispatchArgs(route.Agent, task, sysPrompt, memDir, cwd, false, resolvedEffort, workdir)

	// Record the routing intent before execution so the decision is visible even
	// if the adapter run fails or blocks. Note + record are separate lock-scoped
	// operations; no lock is held across the (potentially long) agent run.
	if err := mem.AppendNote("orchestrator", fmt.Sprintf(
		"dispatch %s: %s/%s (%s) :: %s :: rationale %s",
		key, route.Agent, route.Model, route.Difficulty, task, rationaleSummary(route.Rationale),
	)); err != nil {
		return authorOutcome{}, err
	}
	if err := mem.Upsert(key, map[string]any{
		"task":       task,
		"agent":      route.Agent,
		"model":      route.Model,
		"difficulty": route.Difficulty,
		"rationale":  route.Rationale,
		"result":     nil,
	}, "orchestrator"); err != nil {
		return authorOutcome{}, err
	}

	ad, err := o.GetAdapter(route.Agent)
	if err != nil {
		return authorOutcome{}, err
	}

	// Anchor the sub-agent's process working directory to cwd (the per-dispatch
	// workdir when set, else jindo's own cwd — the same value as before, so the
	// no-workdir path is behaviorally unchanged). Done via a structural type
	// assertion so no adapter-interface change is needed: adapters that honor a
	// process dir implement SetDir (the real *cliAdapter does); test fakes that
	// don't are simply left alone.
	if d, ok := ad.(interface{ SetDir(string) }); ok {
		d.SetDir(cwd)
	}

	// Capture the injection context the agent is about to read from memDir — the
	// keyed-record count and whether a _digest is present — RIGHT BEFORE the run,
	// so it reflects the store state the agent actually sees via --add-dir. Stats
	// is read-only (no entry content pulled), preserving the leanness invariant.
	// Best-effort like the dispatch log it feeds: a Stats failure records a note
	// and proceeds with zero values rather than aborting the dispatch.
	injectedRecords, injectedDigest, statsErr := mem.Stats()
	if statsErr != nil {
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("dispatch log: stats failed for %s: %v", key, statsErr))
	}

	runStart := time.Now()
	stdout, err := ad.RunWith(taskToSend, route.Model, extra)
	durationMs := time.Since(runStart).Milliseconds()
	if err != nil {
		// Close the T2 caveat: a RunWith failure previously returned here with NO
		// dispatch.log line, leaving the routing decision and injection context
		// (captured above) invisible to anyone reading the log. Write the same
		// structured entry the success path writes below, with status "error" and
		// an empty memory_updates summary (the agent never got to respond). Same
		// best-effort semantics as the success-path log write (loop-0002 T2 §4):
		// never let a log failure change the error we return.
		if logErr := appendDispatchLog(memDir, dispatchLogEntry{
			Timestamp:       time.Now().UTC().Format(time.RFC3339),
			Key:             key,
			Task:            task,
			Agent:           route.Agent,
			Model:           route.Model,
			Difficulty:      route.Difficulty,
			Priority:        priority,
			Rationale:       route.Rationale,
			InjectedRecords: injectedRecords,
			InjectedDigest:  injectedDigest,
			MemoryUpdates:   memoryUpdatesSummary{},
			Status:          "error",
			Summary:         truncateForLog(err.Error()),
			DurationMs:      durationMs,
		}); logErr != nil {
			_ = mem.AppendNote("orchestrator", fmt.Sprintf("dispatch log write failed for %s: %v", key, logErr))
		}
		return authorOutcome{}, fmt.Errorf("orchestrator: agent %q run failed: %w", route.Agent, err)
	}

	resp := agentproto.ParseResponse(stdout)

	// Fan out the agent's memory updates FIRST, before the authoritative record
	// below. A note is the agent's durable fact for later agents (per the response
	// contract), so it is BOTH appended to the free-form audit trail AND
	// contributed to the cross-agent insight layer. Keyed values are persisted
	// under an AGENT-OWNED, collision-free key: we NEVER write under the
	// orchestrator's key or another agent's key. If an update names a key already
	// owned by a DIFFERENT agent (per OwnerOf), we refuse to clobber it and
	// allocate a fresh owned key instead.
	//
	// This loop runs BEFORE the authoritative full-result Upsert so that the
	// structured record always wins as the final state of the dispatch's own key.
	// The fan-out's relabel guard only relabels keys NOT owned by route.Agent, so
	// an update that names the dispatch's OWN key (OwnerOf == route.Agent) is
	// written here without relabeling — but the authoritative Upsert below then
	// overwrites it, preventing a stray scalar from clobbering the full record.
	for _, up := range resp.MemoryUpdates {
		if up.Note != "" {
			if err := mem.AppendNote(route.Agent, up.Note); err != nil {
				return authorOutcome{}, err
			}
			// Contribute the note to the insight layer, provenance-tagged
			// (agent/model) and keyed by the task's terms, so a later agent of
			// ANY model can recall it by relevance. AddInsight dedups by
			// normalized text (re-derivation reinforces, not duplicates).
			// Best-effort: the note is already durable in the audit trail, so an
			// insight failure records a note and never fails the dispatch.
			if _, ierr := mem.AddInsightWith(up.Note, route.Agent, route.Model, taskTags(task), injectedInsightTexts); ierr != nil {
				_ = mem.AppendNote("orchestrator", fmt.Sprintf("insight add failed for %s: %v", key, ierr))
			}
		}
		if up.Key != "" || up.Value != nil {
			target := up.Key
			owner := memory.OwnerOf(target)
			// Relabel when the named key is empty, not agent-scoped, or owned by
			// a different agent — anything we cannot safely claim as this agent's.
			if target == "" || owner != route.Agent {
				owned, err := mem.AllocKey(route.Agent)
				if err != nil {
					return authorOutcome{}, fmt.Errorf("orchestrator: alloc key for memory update: %w", err)
				}
				target = owned
			}
			if err := mem.Upsert(target, up.Value, route.Agent); err != nil {
				return authorOutcome{}, err
			}
		}
	}

	// Persist the outcome under the OWNED dispatch key LAST, authored by the
	// executing agent so other agents attribute the result to its producer. Same
	// key as the intent => idempotent overwrite, no duplicate entry on retry.
	// Writing last guarantees the authoritative structured record is the final
	// state of the dispatch key even if the fan-out above named that same key.
	authRecord := map[string]any{
		"task":       task,
		"agent":      route.Agent,
		"model":      route.Model,
		"difficulty": route.Difficulty,
		"rationale":  route.Rationale,
		"result":     resp.Result,
		"status":     resp.Status,
		"summary":    resp.Summary,
	}
	if err := mem.Upsert(key, authRecord, route.Agent); err != nil {
		return authorOutcome{}, err
	}

	// The cross-agent insight layer is fed from the agent's memory_updates NOTES
	// (its durable facts for later agents; see the fan-out loop above), NOT from
	// the dispatch summary — a summary describes what THIS run did, which is a
	// low-signal, one-off action log rather than a reusable project fact.

	// Self-bound the shared store now that this dispatch's own authoritative
	// record is durably written. Running here (per loop-0011-design §1) is the
	// only point that sees the FINAL state of this dispatch — after the fan-out
	// loop and the authoritative Upsert above — so MaybeCompact never folds or
	// caps around a still-placeholder or still-mutating record.
	//
	// Defaults match callCompact's manual `compact` MCP tool exactly
	// (MaxEntries/MaxNotes 200, TTL disabled, deterministic) so the automatic
	// per-dispatch push and the manual pull enforce a single, non-diverging
	// notion of "too many entries" (loop-0011-design §3).
	//
	// Best-effort by design (loop-0011-design §4): unlike the Upsert above — a
	// correctness write whose failure means this dispatch's record was lost — a
	// MaybeCompact failure only means the store did not shrink; the authoritative
	// record is already safe. So a compaction hiccup must NOT surface as a
	// Dispatch failure. We keep visibility (not silent swallowing) by recording
	// the failure as a best-effort note, then proceed to return success.
	if _, err := mem.MaybeCompact(memory.CompactOptions{
		MaxEntries:  200,
		MaxNotes:    200,
		MaxInsights: 200,
		TTLSeconds:  0,
		Now:         0,
		Summarize:   nil,
	}); err != nil {
		// Note append is itself best-effort; its error is intentionally ignored
		// so housekeeping never blocks the primary path.
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("compact failed for %s: %v", key, err))
	}

	// Assemble (but do NOT write) the structured JSONL line recording this
	// dispatch's full shape — the routing "why" (rationale), the injection context
	// the agent actually read (records + digest, captured above pre-run), what it
	// wrote back (memory_updates summary), and the outcome. The caller writes one
	// consolidated line (optionally augmented with the review) so status/summary/
	// memory_updates are final and a review-OFF line stays byte-identical.
	logEntry := dispatchLogEntry{
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		Key:             key,
		Task:            task,
		Agent:           route.Agent,
		Model:           route.Model,
		Difficulty:      route.Difficulty,
		Priority:        priority,
		Rationale:       route.Rationale,
		InjectedRecords: injectedRecords,
		InjectedDigest:  injectedDigest,
		MemoryUpdates:   summarizeMemoryUpdates(resp.MemoryUpdates),
		MemoryUsed:      resp.MemoryUsed,
		Status:          resp.Status,
		Summary:         resp.Summary,
		DurationMs:      durationMs,
	}

	return authorOutcome{
		result: Result{
			Agent:      route.Agent,
			Model:      route.Model,
			Difficulty: route.Difficulty,
			Result:     resp.Result,
			Key:        key,
			Status:     resp.Status,
			Summary:    resp.Summary,
			Rationale:  route.Rationale,
		},
		memDir:     memDir,
		cwd:        cwd,
		key:        key,
		authRecord: authRecord,
		logEntry:   logEntry,
		guidance:   guidance,
		effort:     effort,
		workdir:    workdir,
	}, nil
}

// reviewDirective is the short positional prompt handed to the reviewer adapter;
// the substantive review instruction (schema + author task/result + memory-read)
// rides the BuildReviewPrompt sysPrompt through buildDispatchArgs, exactly as the
// author's BuildSystemPrompt does. A non-empty positional is required because
// some CLIs (e.g. claude -p) demand a prompt argument even when the instruction
// travels via a flag.
const reviewDirective = "Perform the peer review described in your instructions and end with the review JSON block."

// selectReviewers is a seam over routing.SelectReviewers so tests can simulate
// "no cross-model reviewers available" (routing's real config always has 3
// agents, so that error is otherwise unreachable from this package) or shrink
// the reviewer set, without mutating routing's unexported state from outside its
// package.
var selectReviewers = routing.SelectReviewers

// reviewerResult pairs a reviewer's Selection with its parsed review and an ok
// flag (false = best-effort failure: adapter error or unparseable review). It is
// the value each concurrent reviewer goroutine writes into its own slot.
type reviewerResult struct {
	sel routing.Selection
	rev agentproto.ReviewResponse
	ok  bool
}

// reviewWith runs ONE given reviewer selection over ar's result and parses its
// response. It reports ok=false on any best-effort failure — adapter error or
// unparseable review — WITHOUT touching shared memory, so it is safe to call
// from concurrent reviewer goroutines (the caller records failure notes after
// the join). Unlike the old reviewOnce it does NOT select the reviewer itself;
// the selection is supplied by runReviews. The reviewer runs via jindo's OWN
// adapters only (o.GetAdapter), never an external MCP.
func (o *Orchestrator) reviewWith(sel routing.Selection, task string, ar authorOutcome, arts agentproto.ReviewArtifacts) (agentproto.ReviewResponse, bool) {
	ad, err := o.GetAdapter(sel.Agent)
	if err != nil {
		return agentproto.ReviewResponse{}, false
	}

	// The review instruction rides the same per-CLI seam as the author's system
	// prompt (buildDispatchArgs): for claude via --append-system-prompt + --add-dir,
	// for agy/codex prefixed into the task text. Only the review prompt differs.
	// arts carries the REAL changed files (and any exec output) so the reviewer
	// inspects the actual change rather than only the author's self-report.
	reviewPrompt := agentproto.BuildReviewPrompt(ar.memDir, task, ar.result.Result, arts)
	taskToSend, extra := buildDispatchArgs(sel.Agent, reviewDirective, reviewPrompt, ar.memDir, ar.cwd, true, "", "")

	stdout, err := ad.RunWith(taskToSend, sel.Model, extra)
	if err != nil {
		return agentproto.ReviewResponse{}, false
	}

	rev := agentproto.ParseReviewResponse(stdout)
	if rev.Verdict == agentproto.VerdictUnparsed {
		return rev, false
	}
	return rev, true
}

// fanOutReviews runs EVERY reviewer concurrently over ar's result. Each
// goroutine writes ONLY its own index in the pre-sized results slice, so there
// is no shared mutation and no mutex is needed on the results. The returned
// slice is in the same (sorted) order as reviewers.
func (o *Orchestrator) fanOutReviews(reviewers []routing.Selection, task string, ar authorOutcome, arts agentproto.ReviewArtifacts) []reviewerResult {
	results := make([]reviewerResult, len(reviewers))
	var wg sync.WaitGroup
	for i := range reviewers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev, ok := o.reviewWith(reviewers[i], task, ar, arts)
			results[i] = reviewerResult{sel: reviewers[i], rev: rev, ok: ok}
		}(i)
	}
	wg.Wait()
	return results
}

// proposeDirective is the short positional prompt handed to a candidate adapter
// in DispatchMulti; the substantive propose instruction (contract + task +
// memory-read + do-not-write) rides the BuildProposePrompt sysPrompt through
// buildDispatchArgs, exactly as reviewDirective does for reviews. A non-empty
// positional is required because some CLIs (e.g. claude -p) demand a prompt
// argument even when the instruction travels via a flag.
const proposeDirective = "Solve the task described in your instructions in read-only propose mode and end with the response JSON block."

// defaultJudgeModel is the model DispatchMulti synthesizes candidates with when
// synthesis=="judge" and no judge_model is pinned by the caller.
const defaultJudgeModel = "claude-opus-4-8"

// Candidate is one model's read-only "propose" solution in a multi-model
// fan-out: the resolved Agent/Model, the agent's full solution text (Result),
// and the parsed Status ("error" on a best-effort per-candidate failure — a
// routing/adapter/parse failure that does NOT fail the whole DispatchMulti).
type Candidate struct {
	Agent  string
	Model  string
	Result string
	Status string
}

// MultiResult is the outcome of DispatchMulti: one Candidate per requested model
// (same order as the models argument), plus an optional judge Synthesis. Synthesis
// is nil unless a judge ran AND succeeded (a judge failure is best-effort: it
// leaves Synthesis nil rather than failing the call).
type MultiResult struct {
	Candidates []Candidate
	Synthesis  *Candidate
}

// proposeOne runs ONE model's read-only propose and returns its Candidate. It is
// self-contained (no shared memory writes, no dispatch key/log — a propose is not
// a recorded dispatch) so it is safe to call from concurrent goroutines. Any
// best-effort failure (routing, unknown adapter, adapter run error) yields a
// Candidate with Status "error" carrying whatever agent/model could be resolved,
// so one model's failure never fails the whole fan-out. memDir/cwd are resolved
// once by the caller and shared (read-only) across candidates.
func (o *Orchestrator) proposeOne(task, model, guidance, memDir, cwd string) Candidate {
	// model-pin path: routing infers the agent from the model id (agent="").
	route, err := o.Route(task, "", "", model)
	if err != nil {
		return Candidate{Model: model, Status: "error"}
	}

	sysPrompt := agentproto.BuildProposePrompt(memDir, task)
	if guidance != "" {
		sysPrompt += "\n\nTASK-SPECIFIC GUIDANCE (from the caller; follow it for THIS task):\n" + guidance + "\n"
	}

	// Read-only propose run: same per-CLI seam as reviewWith (reviewMode=true), so
	// the candidate can read memory via --add-dir but cannot write/edit files —
	// this is what lets N models run concurrently without clobbering each other.
	taskToSend, extra := buildDispatchArgs(route.Agent, proposeDirective, sysPrompt, memDir, cwd, true, "", "")

	ad, err := o.GetAdapter(route.Agent)
	if err != nil {
		return Candidate{Agent: route.Agent, Model: model, Status: "error"}
	}

	stdout, err := ad.RunWith(taskToSend, route.Model, extra)
	if err != nil {
		return Candidate{Agent: route.Agent, Model: model, Status: "error"}
	}

	resp := agentproto.ParseResponse(stdout)
	return Candidate{Agent: route.Agent, Model: route.Model, Result: resp.Result, Status: resp.Status}
}

// DispatchMulti fans task out to EVERY model in models concurrently in read-only
// "propose" mode and returns each model's candidate solution; when synthesis ==
// "judge" it additionally runs a judge (judgeModel, defaulting to
// defaultJudgeModel when empty) that reads all candidates and produces one
// synthesized solution in MultiResult.Synthesis.
//
// This is a general collaboration primitive, deliberately SEPARATE from the
// dispatch()/runReviews pipeline: candidates are read-only (no file writes, no
// verify, no peer review, no auto-revision) so N models never clobber each
// other's files, and — because nothing is applied — it allocates NO dispatch key
// and writes NO dispatch.log (a propose is not a recorded dispatch). The host
// (or the optional judge) decides what to do with the candidates.
//
// Concurrency mirrors fanOutReviews: a pre-sized Candidates slice where each
// goroutine writes ONLY its own index, so there is no shared mutation and no
// mutex. A per-candidate failure yields a Status "error" Candidate rather than
// failing the whole call; the judge is best-effort (a judge failure leaves
// Synthesis nil). synthesis "" or "none" runs no judge.
func (o *Orchestrator) DispatchMulti(task string, models []string, guidance, synthesis, judgeModel string) (MultiResult, error) {
	if len(models) == 0 {
		return MultiResult{}, fmt.Errorf("orchestrator: DispatchMulti requires at least one model")
	}

	// Resolve the memory directory and process cwd ONCE (read-only, shared across
	// candidates), exactly as runAuthor does — Getwd falling back to memDir.
	mem := o.dispatchStore()
	memDir := mem.Root()
	cwd, err := os.Getwd()
	if err != nil {
		cwd = memDir
	}

	candidates := make([]Candidate, len(models))
	var wg sync.WaitGroup
	for i := range models {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidates[i] = o.proposeOne(task, models[i], guidance, memDir, cwd)
		}(i)
	}
	wg.Wait()

	res := MultiResult{Candidates: candidates}

	if synthesis == "judge" {
		res.Synthesis = o.judgeCandidates(task, candidates, judgeModel, memDir, cwd)
	}
	return res, nil
}

// judgeCandidates runs a single read-only judge over the candidates' solutions
// and returns the synthesized Candidate, or nil on ANY best-effort failure
// (routing/adapter/run) so a judge failure never fails DispatchMulti. It embeds
// every candidate's Result (including empty ones from failed candidates, keeping
// the numbering aligned with the candidate list the host sees).
func (o *Orchestrator) judgeCandidates(task string, candidates []Candidate, judgeModel, memDir, cwd string) *Candidate {
	if judgeModel == "" {
		judgeModel = defaultJudgeModel
	}
	route, err := o.Route(task, "", "", judgeModel)
	if err != nil {
		return nil
	}
	ad, err := o.GetAdapter(route.Agent)
	if err != nil {
		return nil
	}

	solutions := make([]string, len(candidates))
	for i, c := range candidates {
		solutions[i] = c.Result
	}
	sysPrompt := agentproto.BuildJudgePrompt(memDir, task, solutions)
	taskToSend, extra := buildDispatchArgs(route.Agent, proposeDirective, sysPrompt, memDir, cwd, true, "", "")

	stdout, err := ad.RunWith(taskToSend, route.Model, extra)
	if err != nil {
		return nil
	}
	resp := agentproto.ParseResponse(stdout)
	return &Candidate{Agent: route.Agent, Model: route.Model, Result: resp.Result, Status: resp.Status}
}

// GateResult is the autonomous loop's stop-gate outcome: the count of
// not-yet-done plan steps (StepsRemaining), the OBJECTIVE integration signal
// (Verify), the read-only goal-met judge's verdict (GoalMet/GoalMetReason), and
// whether the loop may terminate (CanStop). CanStop is conservative by
// construction: true ONLY when no steps remain AND verify passed AND the judge
// affirmatively confirmed the goal is met — the gate must never claim the goal
// is met when it could not check.
type GateResult struct {
	StepsRemaining int          `json:"steps_remaining"`
	Verify         VerifyResult `json:"verify"`
	GoalMet        bool         `json:"goal_met"`
	GoalMetReason  string       `json:"goal_met_reason"`
	CanStop        bool         `json:"can_stop"`
}

// goalCheckDirective is the short positional prompt handed to the goal-met judge
// adapter; the substantive instruction (read memory + inspect repo + strict
// goal-met verdict + JSON block) rides the BuildGoalCheckPrompt sysPrompt through
// buildDispatchArgs, exactly as proposeDirective does for a propose. A non-empty
// positional is required because some CLIs (e.g. claude -p) demand a prompt
// argument even when the instruction travels via a flag.
const goalCheckDirective = "Judge whether the stated goal is met as described in your instructions and end with the goal-check JSON block."

// PlanGate is the autonomous loop's OBJECTIVE + JUDGED stop gate. It runs the
// active plan's INTEGRATION verify commands (the machine signal) in workdir AND
// a read-only goal-met judge (a strong model that reads shared memory and
// inspects workdir), and reports whether the loop may stop. It is conservative
// by construction: CanStop is true ONLY when no steps remain, the verify gate
// objectively passed, AND the judge affirmatively confirmed the goal is met.
//
// workdir, when empty, defaults to jindo's process cwd (Getwd falling back to
// memDir), mirroring DispatchMulti. judgeModel pins the goal-met judge's model;
// empty uses defaultJudgeModel (planning/judging default to a strong model).
//
// An invalid/unsafe verifyCmds list is a gate CONFIG error, refused before
// anything runs (mirroring dispatch's pre-run ValidateVerifyCmds). An empty
// verifyCmds means there is no objective gate — the goal-met judge alone decides
// — so verify is treated as trivially passed. The goal-met judge is best-effort:
// ANY failure (routing/adapter/run/parse) yields GoalMet=false with a short
// reason, so the gate never claims a goal is met when it could not check.
func (o *Orchestrator) PlanGate(goal, spec string, verifyCmds []string, stepsRemaining int, workdir, judgeModel string) (GateResult, error) {
	mem := o.dispatchStore()
	memDir := mem.Root()

	// Resolve the working directory ONCE (mirrors DispatchMulti's Getwd->memDir
	// fallback): a non-empty workdir is the explicit anchor, else jindo's cwd.
	if workdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = memDir
		}
		workdir = cwd
	}

	// Objective machine gate: run the plan's INTEGRATION verify commands in
	// workdir. Validate BEFORE running anything so an invalid/unsafe list refuses
	// the gate as a config error rather than doing partial work. An empty list is
	// no objective gate at all, so it is trivially passed and the judge decides.
	var vr VerifyResult
	if len(verifyCmds) > 0 {
		if err := ValidateVerifyCmds(verifyCmds); err != nil {
			return GateResult{}, err
		}
		vr = runVerify(workdir, verifyCmds)
	} else {
		vr = VerifyResult{Passed: true}
	}

	// Read-only goal-met judge, anchored to workdir. It sees the objective verify
	// outcome (integrationSummary) alongside its own inspection.
	goalMet, goalMetReason := o.judgeGoalMet(goal, spec, verifySummary(vr), workdir, memDir, judgeModel)

	return GateResult{
		StepsRemaining: stepsRemaining,
		Verify:         vr,
		GoalMet:        goalMet,
		GoalMetReason:  goalMetReason,
		CanStop:        stepsRemaining == 0 && vr.Passed && goalMet,
	}, nil
}

// verifySummary renders the objective verify outcome as the short line handed to
// the goal-met judge so it weighs the machine signal alongside its own
// inspection: "integration verify passed", or "integration verify FAILED: <cmd>
// exit <code>" naming the offending command.
func verifySummary(vr VerifyResult) string {
	if vr.Passed {
		return "integration verify passed"
	}
	return fmt.Sprintf("integration verify FAILED: %s exit %d", vr.FailedCmd, vr.ExitCode)
}

// judgeGoalMet runs ONE read-only goal-met judge (a strong model, defaulting to
// defaultJudgeModel) over goal+spec, anchored to workdir, and returns its
// verdict. It mirrors proposeOne/reviewWith's read-only single-dispatch seam
// (buildDispatchArgs reviewMode=true) so the judge can read memory + inspect
// files but cannot write them. integrationSummary is the objective verify signal
// (see verifySummary) so the judge sees it too. Best-effort: ANY failure
// (routing/adapter/run/parse) returns (false, "goal-met judge unavailable: ...")
// — the gate must NOT claim the goal is met when it could not check.
func (o *Orchestrator) judgeGoalMet(goal, spec, integrationSummary, workdir, memDir, judgeModel string) (bool, string) {
	if judgeModel == "" {
		judgeModel = defaultJudgeModel
	}
	// model-pin path: routing infers the agent from the model id (agent="").
	route, err := o.Route(goal, "", "", judgeModel)
	if err != nil {
		return false, "goal-met judge unavailable: " + err.Error()
	}
	ad, err := o.GetAdapter(route.Agent)
	if err != nil {
		return false, "goal-met judge unavailable: " + err.Error()
	}

	sysPrompt := agentproto.BuildGoalCheckPrompt(memDir, goal, spec, integrationSummary)
	// Read-only run anchored to workdir: same per-CLI seam as reviewWith/proposeOne
	// (reviewMode=true), so the judge reads memory + inspects files via the
	// directory grants but cannot write/edit. Passing workdir as BOTH the process
	// cwd and the explicit workdir grants points every CLI's directory access at
	// the directory verify ran in.
	taskToSend, extra := buildDispatchArgs(route.Agent, goalCheckDirective, sysPrompt, memDir, workdir, true, "", workdir)

	stdout, err := ad.RunWith(taskToSend, route.Model, extra)
	if err != nil {
		return false, "goal-met judge unavailable: " + err.Error()
	}

	goalMet, reason, ok := agentproto.ParseGoalCheckResponse(stdout)
	if !ok {
		return false, "goal-met judge produced no parseable verdict"
	}
	return goalMet, reason
}

// noteReviewFailures records, sequentially after the concurrent join, one
// best-effort note per reviewer that failed (ok==false), so failures stay
// visible without any memory writes happening on the concurrent path.
func (o *Orchestrator) noteReviewFailures(results []reviewerResult, key string) {
	mem := o.dispatchStore()
	for _, r := range results {
		if !r.ok {
			_ = mem.AppendNote("orchestrator", fmt.Sprintf("review: reviewer %q failed or emitted unparseable review for %s", r.sel.Agent, key))
		}
	}
}

// perReviewerRecords builds one reviewRecord per reviewer (in reviewer order):
// the reviewer's agent/model, its verdict (VerdictUnparsed when it failed), its
// per-severity finding counts, and the Errored flag. These carry no aggregate
// fields (RevisionRounds/FinalStatus stay zero) — those live on the aggregate.
func perReviewerRecords(results []reviewerResult) []reviewRecord {
	recs := make([]reviewRecord, len(results))
	for i, r := range results {
		verdict := agentproto.VerdictUnparsed
		if r.ok {
			verdict = r.rev.Verdict
		}
		recs[i] = reviewRecord{
			ReviewerAgent: r.sel.Agent,
			ReviewerModel: r.sel.Model,
			Verdict:       verdict,
			Findings:      countFindings(r.rev.Findings),
			Errored:       !r.ok,
		}
	}
	return recs
}

// mergeFindings concatenates the findings of every reviewer that succeeded (the
// UNION handed to the author on a revision round). Failed reviewers contribute
// nothing.
func mergeFindings(results []reviewerResult) []agentproto.ReviewFinding {
	var all []agentproto.ReviewFinding
	for _, r := range results {
		if r.ok {
			all = append(all, r.rev.Findings...)
		}
	}
	return all
}

// runReviews runs the best-effort concurrent multi-reviewer stage over a
// completed author run: EVERY cross-model reviewer (all agents except the
// author) reviews the result at once. It returns the FINAL author outcome (the
// re-dispatched author's if a revision round ran, else the original), the
// AGGREGATE reviewRecord that feeds the existing log/memory/status path
// unchanged, and the per-reviewer records exposed on Result.Reviews.
//
// It never returns an error path for a reviewer failure — those degrade to
// errored per-reviewer records that leave the author's result unchanged. A hard
// error can only come from the author re-dispatch, which is handled inline and
// surfaces via a review_failed final status rather than a returned error, so the
// pipeline still records a consolidated log line.
func (o *Orchestrator) runReviews(task, priority string, ar authorOutcome, arts agentproto.ReviewArtifacts) (authorOutcome, reviewRecord, []reviewRecord) {
	mem := o.dispatchStore()

	reviewers, err := selectReviewers(task, priority, ar.result.Agent)
	if err != nil || len(reviewers) == 0 {
		// Best-effort: no cross-model reviewers available. Record the reason once
		// and leave the author's result untouched.
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("review: no cross-model reviewers for %s: %v", ar.key, err))
		return ar, reviewRecord{
			Verdict:        agentproto.VerdictUnparsed,
			RevisionRounds: 0,
			FinalStatus:    ar.result.Status,
			Errored:        true,
		}, nil
	}

	// Round 1: concurrent fan-out to every reviewer, then record failure notes
	// sequentially after the join.
	results := o.fanOutReviews(reviewers, task, ar, arts)
	o.noteReviewFailures(results, ar.key)
	perReviewer := perReviewerRecords(results)

	anyCritical := false
	for _, r := range results {
		if r.ok && agentproto.HasCritical(r.rev.Findings) {
			anyCritical = true
			break
		}
	}

	if !anyCritical {
		// No critical finding from any reviewer: accept the author's result as-is.
		return ar, aggregateReviews(perReviewer, ar.result.Status, 0), perReviewer
	}

	// A critical finding from at least one reviewer: EXACTLY ONE revision round.
	// Re-dispatch to the SAME author (explicit agent) via the non-reviewing core,
	// with the UNION of all reviewers' findings appended so the author sees
	// everything to fix. A re-dispatch adapter failure is a hard failure of the
	// revision, not a reviewer failure — we do not propagate it as a Go error (the
	// pipeline must still emit its consolidated record); we mark the outcome
	// review_failed and keep the ORIGINAL author result + round-1 per-reviewer list.
	revisedTask := task + "\n\n" + renderFindings(agentproto.ReviewResponse{Findings: mergeFindings(results)})
	ar2, err := o.runAuthor(revisedTask, ar.result.Agent, priority, "", ar.guidance, ar.effort, ar.workdir)
	if err != nil {
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("review: re-dispatch failed for %s: %v", ar.key, err))
		return ar, aggregateReviews(perReviewer, "review_failed", 1), perReviewer
	}

	// ONE concurrent re-review of the revised result with the SAME reviewer set.
	// The review prompt still uses the ORIGINAL task (matching pre-fan-out
	// behavior); only the reviewed result (ar2) changes. We reuse the round-1
	// artifacts: the changed-file PATHS overlap the revision's file set, and the
	// reviewer opens them on disk where ar2's revised content already lives.
	results2 := o.fanOutReviews(reviewers, task, ar2, arts)
	o.noteReviewFailures(results2, ar2.key)
	perReviewer2 := perReviewerRecords(results2)

	finalStatus := ar2.result.Status
	for _, r := range results2 {
		if r.ok && agentproto.HasCritical(r.rev.Findings) {
			// Still critical after the single allowed revision: gate the dispatch.
			finalStatus = "review_failed"
			break
		}
	}
	return ar2, aggregateReviews(perReviewer2, finalStatus, 1), perReviewer2
}

// verdictRank maps a review verdict to a severity rank so aggregateReviews can
// pick the WORST across reviewers: rejected(3) > changes_requested(2) >
// approved(1) > anything else / unparsed(0).
func verdictRank(verdict string) int {
	switch verdict {
	case "rejected":
		return 3
	case "changes_requested":
		return 2
	case "approved":
		return 1
	default:
		return 0
	}
}

// verdictForRank is the inverse of verdictRank for the ranks it produces; rank 0
// (no reviewer above unparsed) maps to VerdictUnparsed.
func verdictForRank(rank int) string {
	switch rank {
	case 3:
		return "rejected"
	case 2:
		return "changes_requested"
	case 1:
		return "approved"
	default:
		return agentproto.VerdictUnparsed
	}
}

// aggregateReviews folds the per-reviewer records into the single reviewRecord
// that feeds the EXISTING log/memory/status path (dispatchLogEntry.Review, the
// authoritative record's "review" key, and calibrate) so those schemas stay
// UNCHANGED under multi-reviewer fan-out. The aggregate joins reviewer
// agents/models (same order) into comma-separated lists, takes the WORST verdict
// by rank, SUMS every per-severity finding count, and is Errored only when EVERY
// reviewer errored.
func aggregateReviews(perReviewer []reviewRecord, finalStatus string, revisionRounds int) reviewRecord {
	agents := make([]string, len(perReviewer))
	models := make([]string, len(perReviewer))
	var findings findingCounts
	worstRank := 0
	allErrored := len(perReviewer) > 0
	for i, r := range perReviewer {
		agents[i] = r.ReviewerAgent
		models[i] = r.ReviewerModel
		findings.Total += r.Findings.Total
		findings.Critical += r.Findings.Critical
		findings.Major += r.Findings.Major
		findings.Minor += r.Findings.Minor
		findings.Info += r.Findings.Info
		if rank := verdictRank(r.Verdict); rank > worstRank {
			worstRank = rank
		}
		if !r.Errored {
			allErrored = false
		}
	}
	return reviewRecord{
		ReviewerAgent:  strings.Join(agents, ","),
		ReviewerModel:  strings.Join(models, ","),
		Verdict:        verdictForRank(worstRank),
		Findings:       findings,
		RevisionRounds: revisionRounds,
		FinalStatus:    finalStatus,
		Errored:        allErrored,
	}
}

// finishReviewed records a completed review: it augments the final author's
// authoritative record with a queryable "review" entry (best-effort, like the
// other post-authoritative writes), writes ONE consolidated dispatch.log line
// carrying the Review summary, and returns the Result with its Status set to the
// review's final status (e.g. "review_failed" when critical findings survived the
// single revision round).
func (o *Orchestrator) finishReviewed(ar authorOutcome, agg reviewRecord, perReviewer []reviewRecord) (Result, error) {
	mem := o.dispatchStore()

	// Add the AGGREGATE review to the authoritative record so it is queryable via
	// the memory tool under the same "review" key as before — keeping the memory
	// schema unchanged under multi-reviewer fan-out. Re-Upsert under the SAME owned
	// key (idempotent overwrite) authored by the executing agent. Best-effort: the
	// authoritative record is already durable, so a failure here must not fail the
	// dispatch.
	ar.authRecord["review"] = agg
	if err := mem.Upsert(ar.key, ar.authRecord, ar.result.Agent); err != nil {
		_ = mem.AppendNote("orchestrator", fmt.Sprintf("review: record augment failed for %s: %v", ar.key, err))
	}

	// Consolidated dispatch.log line, augmented with the AGGREGATE review and
	// reflecting the pipeline's final status — so the dispatch.log schema stays
	// unchanged (one *reviewRecord).
	entry := ar.logEntry
	entry.Review = &agg
	entry.Status = agg.FinalStatus
	o.writeDispatchLog(ar.memDir, entry)

	res := ar.result
	res.Status = agg.FinalStatus
	// Expose every reviewer's outcome (per-reviewer, not the aggregate) on the
	// Result so the host can gate on individual verdicts/findings.
	res.Reviews = perReviewer
	// Make the review's trust status EXPLICIT (see ReviewStatus): this path only
	// runs when review was requested. Completed iff at least one reviewer returned a
	// real (non-errored) verdict; GatePassed iff completed AND no unresolved critical
	// finding flipped the status to "review_failed"; Confidence folds the two into a
	// single label.
	completed := false
	for _, r := range perReviewer {
		if !r.Errored {
			completed = true
			break
		}
	}
	gatePassed := completed && res.Status != "review_failed"
	confidence := "unverified"
	if res.Status == "review_failed" {
		confidence = "review_failed"
	} else if gatePassed {
		confidence = "reviewed"
	}
	res.Review = &ReviewStatus{
		Requested:  true,
		Completed:  completed,
		GatePassed: gatePassed,
		Confidence: confidence,
	}
	return res, nil
}

// writeDispatchLog appends one dispatch.log line, best-effort: a write failure is
// recorded as a note (never swallowed) and never changes the dispatch outcome —
// the authoritative record is already durable.
func (o *Orchestrator) writeDispatchLog(memDir string, entry dispatchLogEntry) {
	if err := appendDispatchLog(memDir, entry); err != nil {
		_ = o.dispatchStore().AppendNote("orchestrator", fmt.Sprintf("dispatch log write failed for %s: %v", entry.Key, err))
	}
}

// countFindings tallies findings by severity for the reviewRecord, without
// carrying the (possibly large) finding bodies.
func countFindings(findings []agentproto.ReviewFinding) findingCounts {
	c := findingCounts{Total: len(findings)}
	for _, f := range findings {
		switch f.Severity {
		case "critical":
			c.Critical++
		case "major":
			c.Major++
		case "minor":
			c.Minor++
		case "info":
			c.Info++
		}
	}
	return c
}

// renderFindings turns a review into the plain-text block appended to the task on
// a revision re-dispatch, so the author sees exactly what to fix.
func renderFindings(rev agentproto.ReviewResponse) string {
	var b strings.Builder
	b.WriteString("A peer reviewer requested changes before this work can be accepted. ")
	b.WriteString("Address the following findings and re-do the task:\n")
	if rev.Summary != "" {
		b.WriteString("Reviewer summary: ")
		b.WriteString(rev.Summary)
		b.WriteString("\n")
	}
	for _, f := range rev.Findings {
		b.WriteString(fmt.Sprintf("- [%s] %s: %s\n", f.Severity, f.Title, f.Message))
	}
	return b.String()
}

// renderVerifyFailure turns a failed VerifyResult into the plain-text block
// appended to the task on an automatic verify-revision re-dispatch, so the same
// author sees exactly which objective command failed and its (truncated) output
// to fix. Mirrors renderFindings, but sourced from the machine gate rather than a
// peer reviewer.
func renderVerifyFailure(vr VerifyResult) string {
	var b strings.Builder
	b.WriteString("The objective verify gate failed and must pass before this work can be accepted. ")
	b.WriteString("Fix the cause and re-do the task so the command below exits zero:\n")
	b.WriteString(fmt.Sprintf("Failed command: %s\n", vr.FailedCmd))
	b.WriteString(fmt.Sprintf("Exit code: %d\n", vr.ExitCode))
	if vr.Output != "" {
		b.WriteString("Command output:\n")
		b.WriteString(vr.Output)
		b.WriteString("\n")
	}
	return b.String()
}

// rationaleSummary renders the routing rationale as a compact, deterministic
// one-line fragment for the dispatch note: the total score plus the matched
// signal names (sorted for stable output). It surfaces the "why" of the routing
// decision in the free-form note stream, complementing the structured rationale
// persisted in the dispatch record.
func rationaleSummary(r routing.Rationale) string {
	names := make([]string, 0, len(r.Matched))
	for name := range r.Matched {
		names = append(names, name)
	}
	sort.Strings(names)
	matched := "none"
	if len(names) > 0 {
		matched = strings.Join(names, ",")
	}
	return fmt.Sprintf("total=%.2f matched=%s", r.Total, matched)
}

// insightInjectK bounds how many cross-agent insights are injected into a
// dispatch prompt. Kept small so the brief stays a curated hint, not a dump:
// the point of the insight layer is relevance-ranked recall, not re-serializing
// the store into every prompt.
const insightInjectK = 5

// taskTags derives a few retrieval keywords from a task string so a contributed
// insight is findable by future tasks that share terminology even when the
// summary text words it differently. Bounded to keep tags a hint, not the whole
// task. Reuses the memory tokenizer so tags and retrieval scoring share a basis.
func taskTags(task string) []string {
	return memory.KeywordsOf(task, 6)
}

// renderInsightBrief formats retrieved insights as a compact, deterministic
// prompt section: one line per insight with its provenance and reinforcement so
// the reading agent can weigh it (a learning three different models re-derived
// is stronger than a one-off). Returns "" for an empty slice so callers append
// nothing when there is nothing relevant.
func renderInsightBrief(insights []memory.Insight) string {
	if len(insights) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nCROSS-AGENT INSIGHTS (learnings from prior agents on this project, most relevant first — treat as hints, verify before relying):\n")
	for _, in := range insights {
		src := in.Agent
		if in.Model != "" {
			src += "/" + in.Model
		}
		if src == "" {
			src = "unknown"
		}
		fmt.Fprintf(&b, "- %s  [src: %s, reinforced %dx]\n", truncateForLog(in.Text), src, in.Hits)
	}
	return b.String()
}

// dispatchLogFile is the JSONL sink appended per dispatch, living next to
// memory.json under the memory root so the log and the store it observes are
// co-located.
const dispatchLogFile = "dispatch.log"

// dispatchLogEntry is the one compact JSON object appended per dispatch. It
// carries the routing rationale (reusing routing.Rationale so matched
// signals/total/threshold/tier stay in one shape), the injection context the
// agent read (InjectedRecords/InjectedDigest, from mem.Stats), a summary of the
// agent's memory updates, and the outcome.
type dispatchLogEntry struct {
	Timestamp       string               `json:"timestamp"`
	Key             string               `json:"key"`
	Task            string               `json:"task"`
	Agent           string               `json:"agent"`
	Model           string               `json:"model"`
	Difficulty      string               `json:"difficulty"`
	Priority        string               `json:"priority,omitempty"`
	Rationale       routing.Rationale    `json:"rationale"`
	InjectedRecords int                  `json:"injected_records"`
	InjectedDigest  bool                 `json:"injected_digest"`
	MemoryUpdates   memoryUpdatesSummary `json:"memory_updates"`
	MemoryUsed      []string             `json:"memory_used,omitempty"`
	Status          string               `json:"status"`
	Summary         string               `json:"summary"`
	// DurationMs is the wall-clock latency of the author adapter run alone (the
	// ad.RunWith call in runAuthor) — it deliberately excludes routing, memory
	// I/O, and (on the revision round) reviewer time, since the goal is
	// per-adapter-invocation latency, not pipeline latency. On the revision
	// round, this is the final author run's duration only, matching the
	// consolidated line's own status/summary being the final outcome.
	DurationMs int64 `json:"duration_ms"`
	// Review is set ONLY when a review pipeline ran for this dispatch (see
	// DispatchWithReview). It is omitempty so a review-OFF dispatch's log line is
	// byte-identical to before this field existed — the backward-compat invariant.
	Review *reviewRecord `json:"review,omitempty"`
}

// reviewRecord is the additive summary of a cross-model peer review round,
// recorded both in the dispatch log entry (Review field) and under the "review"
// key of the authoritative dispatch record so it is queryable via the memory
// tool. It is only ever produced when the review pipeline ran.
type reviewRecord struct {
	ReviewerAgent  string        `json:"reviewer_agent"`
	ReviewerModel  string        `json:"reviewer_model"`
	Verdict        string        `json:"verdict"`
	Findings       findingCounts `json:"findings"`
	RevisionRounds int           `json:"revision_rounds"`
	FinalStatus    string        `json:"final_status"`
	// Errored marks a best-effort reviewer failure (no reviewer available,
	// adapter error, or unparseable review): the review did NOT change the
	// dispatch outcome, but the attempt is still recorded for visibility.
	Errored bool `json:"errored,omitempty"`
}

// findingCounts is the per-severity tally of a review's findings, carried in the
// reviewRecord instead of the (possibly large) finding bodies.
type findingCounts struct {
	Total    int `json:"total"`
	Critical int `json:"critical,omitempty"`
	Major    int `json:"major,omitempty"`
	Minor    int `json:"minor,omitempty"`
	Info     int `json:"info,omitempty"`
}

// memoryUpdatesSummary is the compact shape of what the agent asked to persist:
// the count of updates plus the keys and notes it named (from resp.MemoryUpdates,
// i.e. the agent's requested targets — not the possibly-relabeled owned keys).
type memoryUpdatesSummary struct {
	Count int      `json:"count"`
	Keys  []string `json:"keys,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// summarizeMemoryUpdates reduces the agent's memory updates to their touched
// keys and notes for the dispatch log, without carrying the (possibly large)
// values.
func summarizeMemoryUpdates(ups []agentproto.MemoryUpdate) memoryUpdatesSummary {
	s := memoryUpdatesSummary{Count: len(ups)}
	for _, up := range ups {
		if up.Key != "" {
			s.Keys = append(s.Keys, up.Key)
		}
		if up.Note != "" {
			s.Notes = append(s.Notes, up.Note)
		}
	}
	return s
}

// truncateForLog bounds a message (e.g. a RunWith error, which can embed
// arbitrary CLI stderr) to a reasonable length before it goes into the
// dispatch log, so one runaway adapter failure can't bloat dispatch.log.
func truncateForLog(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

// maxDispatchLogSize caps dispatch.log before rotation kicks in (see
// rotateDispatchLogIfNeeded). It is a var (not a const) so tests can shrink it
// to exercise rotation without writing 5MB of fixture data.
var maxDispatchLogSize int64 = 5 * 1024 * 1024

// rotateDispatchLogIfNeeded renames <memDir>/dispatch.log to
// <memDir>/dispatch.log.1 (overwriting any existing .1) when the current log
// is at or past maxDispatchLogSize, keeping exactly one prior generation
// rather than letting the file grow unbounded. It is best-effort: a missing
// log (nothing to rotate yet) or a stat/rename failure is swallowed, matching
// appendDispatchLog's own contract that log maintenance must never fail a
// dispatch.
func rotateDispatchLogIfNeeded(memDir string) {
	path := filepath.Join(memDir, dispatchLogFile)
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() < maxDispatchLogSize {
		return
	}
	_ = os.Rename(path, path+".1")
}

// appendDispatchLog marshals entry and appends it as one line to
// <memDir>/dispatch.log (O_APPEND|O_CREATE|O_WRONLY, one JSON object per line),
// rotating the log first if it has grown past maxDispatchLogSize. It is
// deliberately small and local; callers treat its error as best-effort.
func appendDispatchLog(memDir string, entry dispatchLogEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	rotateDispatchLogIfNeeded(memDir)
	f, err := os.OpenFile(filepath.Join(memDir, dispatchLogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}
