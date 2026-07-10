// Package mcp implements a pure-stdlib Model Context Protocol server speaking
// JSON-RPC 2.0 over a newline-delimited stdio transport. It exposes jindo's
// orchestrator to an MCP client through five tools:
//
//   - dispatch:   route a coding task to the right agent/model and run it,
//   - memory:     read the shared-memory store (one key, or everything),
//   - agents:     report the agent -> difficulty -> model mapping,
//   - compact:    fold old shared-memory entries into a digest to bound growth,
//   - calibrate:  aggregate dispatch.log into a routing calibration report.
//
// Transport contract: the client writes one JSON object per line to the
// Server's reader; the Server writes one JSON response object per line to its
// writer. This mirrors the line-framed stdio transport MCP hosts use for local
// servers and keeps the whole thing dependency-free and trivially testable over
// in-memory buffers.
//
// The Server holds the *orchestrator.Orchestrator it drives (for dispatch and,
// via o.Mem, for the memory tool) and reads the static agent/model table from
// the routing package. It carries no per-request mutable state, so a single
// Server can serve a stream while handling its requests concurrently.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"jindo/internal/calibrate"
	"jindo/internal/jobs"
	"jindo/internal/memory"
	"jindo/internal/meta"
	"jindo/internal/orchestrator"
	"jindo/internal/plan"
	"jindo/internal/routing"
)

// protocolVersion is the MCP protocol revision this server implements. Reported
// verbatim in the initialize result.
const protocolVersion = "2024-11-05"

// serverName is the advertised MCP server name.
const serverName = "jindo-mcp"

// JSON-RPC 2.0 error codes (subset used here). Values are fixed by the spec.
const (
	codeParseError     = -32700 // malformed JSON in a request line
	codeMethodNotFound = -32601 // unknown method (or unknown tool name)
	codeInvalidParams  = -32602 // params/arguments failed to decode or are invalid
	codeInternalError  = -32603 // a tool ran but failed (e.g. dispatch/memory error)
)

// Request is a decoded JSON-RPC 2.0 request. ID is kept as RawMessage so it can
// be echoed back verbatim (a request may carry a string, number, or null id),
// and Params is kept raw so each method decodes its own parameter shape.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result / Error is set on
// any given response; both use omitempty so the unused field is omitted.
// JSONRPC is always "2.0".
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// nullID is the literal JSON null, used as the response id when a request could
// not be parsed (so its id is unknown), per JSON-RPC.
var nullID = json.RawMessage("null")

// Server is an MCP server bound to an orchestrator. It is stateless beyond the
// injected orchestrator, so HandleLine is a pure request->response function and
// Serve is just a read/dispatch/write loop over it.
type Server struct {
	o    *orchestrator.Orchestrator
	jobs *jobs.Manager
	plan *plan.Manager
}

// NewServer builds a Server that drives o. The agent/model mapping is read from
// the routing package directly at call time, so no extra wiring is needed. The
// async job manager persists terminal jobs under <mem root>/jobs/<id>.json
// (Manager appends the "jobs" subdirectory itself), alongside the rest of
// jindo's on-disk state. The plan manager persists the single active plan under
// <mem root>/plan.json, so the step-loop tools (plan_next/plan_record/
// plan_revise/plan_status) survive a restart alongside jobs.
func NewServer(o *orchestrator.Orchestrator) *Server {
	return &Server{
		o:    o,
		jobs: jobs.NewManager(o.Mem.Root()),
		plan: plan.NewManager(o.Mem.Root()),
	}
}

// maxLineBytes bounds a single request line. MCP payloads (a task prompt, a
// memory dump) can be large, so the read buffer is sized to this ceiling; a
// line exceeding it is refused with one parse error rather than consuming
// unbounded memory or killing the connection.
const maxLineBytes = 8 << 20 // 8 MiB

// Serve reads newline-delimited JSON-RPC requests from r and writes one JSON
// response line to w per request, until r is exhausted (io.EOF) or a read/write
// error occurs. A blank line is skipped. A request with no id is a JSON-RPC
// notification and draws no response, matching the spec (MCP clients send
// notifications/initialized this way after handshake).
//
// Each request is handled in its own goroutine so a long-polling job_status
// (which blocks up to maxJobWaitSec) never stalls the reading or answering of
// other requests on the stream. Responses carry their own id, so they may be
// written in any order; all writes to w are serialized through a mutex so
// response lines never interleave, and Serve waits for every in-flight handler
// to finish (so no response is lost) before returning.
//
// A single line exceeding maxLineBytes does not kill the connection: it draws
// one parse-error response and the rest of that line is discarded up to the
// next '\n', after which serving continues. Returns the first read/write error;
// a clean EOF is reported as nil.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, maxLineBytes)
	bw := bufio.NewWriter(w)
	var (
		mu       sync.Mutex // serializes all writes to bw
		wg       sync.WaitGroup
		writeErr error // first write error, guarded by mu
	)
	// writeResp writes one response line and flushes it, under mu so concurrent
	// handlers (and the oversized-line parse error) never interleave on the
	// wire. The first write error is remembered and short-circuits later writes.
	writeResp := func(resp []byte) {
		mu.Lock()
		defer mu.Unlock()
		if writeErr != nil {
			return
		}
		if _, err := bw.Write(resp); err != nil {
			writeErr = err
			return
		}
		if err := bw.WriteByte('\n'); err != nil {
			writeErr = err
			return
		}
		if err := bw.Flush(); err != nil {
			writeErr = err
		}
	}
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Oversized line: the bounded read buffer refuses to grow past
			// maxLineBytes. Emit one parse error and discard the remainder of
			// the line up to the next '\n', then keep serving.
			writeResp(mustMarshal(errorResponse(nullID, codeParseError, "parse error")))
			derr := discardLine(br)
			if derr == io.EOF {
				break
			}
			if derr != nil {
				wg.Wait()
				return derr
			}
			continue
		}
		// Strip the trailing '\n' (absent only on a final unterminated line
		// delivered together with io.EOF); skip a blank line exactly as before.
		trimmed := line
		if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
			trimmed = trimmed[:n-1]
		}
		if len(trimmed) > 0 {
			// ReadSlice aliases the reader's buffer, overwritten by the next
			// read, so copy before handing the line to the handler goroutine.
			cp := make([]byte, len(trimmed))
			copy(cp, trimmed)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if resp := s.HandleLine(cp); resp != nil {
					writeResp(resp) // nil => notification: write nothing
				}
			}()
		}
		if err != nil {
			wg.Wait()
			if err == io.EOF {
				return writeErr
			}
			return err
		}
		// Stop early if the writer has broken; no point reading further.
		mu.Lock()
		we := writeErr
		mu.Unlock()
		if we != nil {
			wg.Wait()
			return we
		}
	}
	wg.Wait()
	return writeErr
}

// discardLine consumes and discards bytes from br up to and including the next
// '\n' (or EOF). It is used to drop the remainder of an oversized request line
// so the server can recover and serve the next request. Returns nil once a
// newline is consumed, or the terminating read error (io.EOF or worse).
func discardLine(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			continue
		}
		return err
	}
}

// HandleLine decodes one request line and returns the marshaled response bytes
// (without a trailing newline), or nil when the request is a notification that
// draws no response. It never returns an error: a malformed line becomes a
// parse-error response, and any marshaling failure of a normal response is
// downgraded to a synthesized internal-error response so the transport loop
// always has a well-formed line to write. It holds no per-request Server state,
// so it is safe to call concurrently (Serve runs one call per goroutine).
func (s *Server) HandleLine(line []byte) []byte {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return mustMarshal(errorResponse(nullID, codeParseError, "parse error"))
	}
	// A request without an id is a notification: process for side effects only
	// (none of our methods have side effects on notification), send nothing.
	if len(req.ID) == 0 {
		return nil
	}
	resp := s.handle(&req)
	return mustMarshal(resp)
}

// handle dispatches a parsed request to its method handler and returns the
// response to send. Unknown methods yield method-not-found.
func (s *Server) handle(req *Request) Response {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "tools/list":
		return s.toolsList(req)
	case "tools/call":
		return s.toolsCall(req)
	default:
		return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

// initialize reports protocol version, server identity, and tool capability.
func (s *Server) initialize(req *Request) Response {
	return resultResponse(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": meta.Version,
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	})
}

// toolDef is one entry of the tools/list result: name, human description, and
// the JSON Schema for the tool's arguments object.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// verifySchemaProp is the shared JSON Schema for the optional "verify" argument
// on both dispatch and dispatch_async: an array of single-program, shell-free
// commands run after the work to objectively gate the result. It is defined once
// so the two tools advertise an identical shape.
var verifySchemaProp = map[string]any{
	"type":        "array",
	"items":       map[string]any{"type": "string"},
	"description": "Optional objective verification commands (tests/build/lint) run AFTER the work — and after any review — in the dispatch's working directory to gate the result. Each entry is ONE allowlisted program with args and NO shell (no pipes, redirects, subshells, or && / ||); an invalid entry fails the call with invalid params. On any command exiting non-zero the dispatch status becomes \"verify_failed\" (no automatic revision; the caller decides what to do). Omit to skip objective verification.",
}

// effortSchemaProp is the shared JSON Schema for the optional "effort" argument
// on both dispatch and dispatch_async: the reasoning-effort level for the
// author run. It is defined once so the two tools advertise an identical shape.
var effortSchemaProp = map[string]any{
	"type":        "string",
	"description": "Optional reasoning-effort level for the author run: one of \"low\", \"medium\", \"high\", \"xhigh\", \"max\". Overrides the per-difficulty-tier default effort for THIS dispatch. Applied per-agent (claude --effort; codex model_reasoning_effort, which clamps \"max\" to \"xhigh\"); agy encodes effort in its model name and ignores this. Omit to use the tier default.",
}

// workdirSchemaProp is the shared JSON Schema for the optional "workdir"
// argument on both dispatch and dispatch_async: the working directory the
// dispatched work runs in. Defined once so the two tools advertise an identical
// shape.
var workdirSchemaProp = map[string]any{
	"type":        "string",
	"description": "Optional absolute working directory the dispatched work runs in: the author sub-agent is anchored here (process cwd + granted write access) and verify commands run here. Created if missing. Omit to use the server's current working directory.",
}

// validEfforts is the closed set of reasoning-effort levels the dispatch tools
// accept, matching claude's supported range (codex is adapted at the edge; see
// orchestrator.effortForCodex). An empty effort is always valid (means "use the
// tier default"); a non-empty value outside this set is rejected as
// invalid-params before any dispatch work runs.
var validEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

// validateEffort reports an error message when effort is a non-empty value
// outside validEfforts, else "". "" is returned for a valid or empty effort.
func validateEffort(effort string) string {
	if effort == "" || validEfforts[effort] {
		return ""
	}
	return "effort must be one of low, medium, high, xhigh, max"
}

// planStepSchema is the JSON Schema for one plan step object, shared by the
// plan_revise add_steps/update_steps arrays. Only id is required (an update
// merges the non-empty fields it carries; a new step supplies the rest).
var planStepSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id":               map[string]any{"type": "string"},
		"title":            map[string]any{"type": "string"},
		"prompt":           map[string]any{"type": "string", "description": "The concrete task to dispatch for this step."},
		"difficulty":       map[string]any{"type": "string"},
		"suggested_model":  map[string]any{"type": "string"},
		"suggested_verify": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"depends_on":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"status":           map[string]any{"type": "string", "description": "pending|done|failed (update only; omit to leave unchanged)"},
	},
	"required": []string{"id"},
}

// tools returns the fixed catalog advertised by tools/list, in a stable order.
func tools() []toolDef {
	return []toolDef{
		{
			Name:        "dispatch",
			Description: "Route a coding task to the right agent/model and run it. The caller may pin the executing agent and/or the exact model, may inject task-specific guidance into the author's system prompt, may override the per-tier reasoning effort, and may supply objective verify commands that gate the result; omit any of these to use the defaults.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":  map[string]any{"type": "string"},
					"agent": map[string]any{"type": "string"},
					"model": map[string]any{"type": "string", "description": "Optional exact model id to run (e.g. \"claude-opus-4-8\"). When set, the caller pins the model and jindo skips score-based routing; the agent is inferred from the model unless also given. Omit to let jindo route by task difficulty."},
					"priority": map[string]any{
						"type":        "string",
						"description": "Optional routing priority hint: one of \"cost\", \"quality\", \"latency\". Reweights intra-tier agent selection; omit for the default weighting.",
					},
					"guidance": map[string]any{"type": "string", "description": "Optional task-specific guidance injected into the author agent's system prompt for THIS dispatch (e.g. language conventions, a checklist, or skill content). Omit for the default generic contract."},
					"effort":   effortSchemaProp,
					"review": map[string]any{
						"type":        "boolean",
						"description": "Optional opt-in cross-model peer review of the dispatched result. Defaults to false (no review); set true to have a different model review the result and, on a critical finding, trigger one revision round.",
					},
					"verify":  verifySchemaProp,
					"workdir": workdirSchemaProp,
				},
				"required": []string{"task"},
			},
		},
		{
			Name: "dispatch_async",
			Description: "Dispatch a coding task in the background and return immediately with a job_id (does not wait for the result). Use this for long/hard tasks that could exceed the MCP tool timeout. The caller may pin the executing agent and/or the exact model, may inject task-specific guidance into the author's system prompt, may override the per-tier reasoning effort, and may supply objective verify commands that gate the result; omit any of these to use the defaults. " +
				"POLLING CONTRACT: after calling this you MUST poll job_status with the returned job_id until its status is 'done' (or 'error') before proceeding; do NOT treat a 'running' status as a result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":  map[string]any{"type": "string"},
					"agent": map[string]any{"type": "string"},
					"model": map[string]any{"type": "string", "description": "Optional exact model id to run (e.g. \"claude-opus-4-8\"). When set, the caller pins the model and jindo skips score-based routing; the agent is inferred from the model unless also given. Omit to let jindo route by task difficulty."},
					"priority": map[string]any{
						"type":        "string",
						"description": "Optional routing priority hint: one of \"cost\", \"quality\", \"latency\". Reweights intra-tier agent selection; omit for the default weighting.",
					},
					"guidance": map[string]any{"type": "string", "description": "Optional task-specific guidance injected into the author agent's system prompt for THIS dispatch (e.g. language conventions, a checklist, or skill content). Omit for the default generic contract."},
					"effort":   effortSchemaProp,
					"review": map[string]any{
						"type":        "boolean",
						"description": "Optional opt-in cross-model peer review of the dispatched result. Defaults to false (no review); set true to have a different model review the result and, on a critical finding, trigger one revision round.",
					},
					"verify":  verifySchemaProp,
					"workdir": workdirSchemaProp,
				},
				"required": []string{"task"},
			},
		},
		{
			Name:        "dispatch_multi",
			Description: "Fan a task out to multiple models concurrently in read-only \"propose\" mode: each model returns its OWN complete candidate solution (no files are written, so the candidates never clobber each other). Returns each model's candidate; with synthesis=\"judge\" it also returns a jindo-synthesized answer merging the candidates. This is the general collaboration primitive for coding AND non-coding tasks; the host decides when to use it versus single dispatch and normally synthesizes the candidates itself.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":     map[string]any{"type": "string", "description": "The task or question to fan out to every model."},
					"models":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The exact model ids to run concurrently (e.g. [\"claude-opus-4-8\", \"gpt-5.5\"]). Each runs read-only and the agent is inferred from the model id."},
					"guidance": map[string]any{"type": "string", "description": "Optional task-specific guidance injected into every candidate's system prompt for THIS task. Omit for the default generic contract."},
					"synthesis": map[string]any{
						"type":        "string",
						"description": "Optional synthesis mode: \"none\" (default) returns only the raw candidates; \"judge\" additionally runs a judge model that merges them into one synthesized answer.",
					},
					"judge_model": map[string]any{"type": "string", "description": "Optional exact model id for the judge when synthesis=\"judge\". Omit to default to a strong judge model."},
				},
				"required": []string{"task", "models"},
			},
		},
		{
			Name:        "job_status",
			Description: "Poll the status of an async dispatch job. Long-polls: the server waits up to wait_sec seconds (default 25, capped at 30) for the job to finish before responding, then returns status 'running' | 'done' | 'error' plus the full dispatch result when done. Keep polling while status is 'running'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":   map[string]any{"type": "string"},
					"wait_sec": map[string]any{"type": "integer"},
				},
				"required": []string{"job_id"},
			},
		},
		{
			Name:        "plan",
			Description: "Decompose a goal into an ordered step plan via a capable model AND establish it as the active, step-gated plan state. Returns steps, each with a per-step prompt (the concrete task to dispatch), difficulty, suggested_model, suggested_verify commands, and depends_on prerequisites, plus a plan-level verify_cmds (the INTEGRATION gate proving the whole goal is done, distinct from each step's suggested_verify) and the caller's spec echoed back; persists the plan to shared memory (under the returned key) and the full plan — including spec and verify_cmds — to the active plan state (all steps pending). Does NOT execute the steps. Drive the plan ONE step at a time: call plan_next to get the next runnable step, dispatch its prompt (with review=true, at its suggested_model, gated by its suggested_verify), then plan_record the outcome, and repeat; use plan_revise to adapt the remaining plan. The caller may pin the planning agent and/or the exact model; omit both to plan with the default agent's hard-tier model.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal":  map[string]any{"type": "string", "description": "The goal to decompose into an ordered step plan."},
					"spec":  map[string]any{"type": "string", "description": "Optional clarified intent anchoring the plan (and later re-plans). Persisted on the active plan state and echoed back; omit if none."},
					"agent": map[string]any{"type": "string", "description": "Optional agent to run the planner (e.g. \"claude\"). Omit to use the default planning agent."},
					"model": map[string]any{"type": "string", "description": "Optional exact model id to run the planner. When set it is pinned; omit to use the chosen agent's hard-tier model (planning defaults to a strong model)."},
				},
				"required": []string{"goal"},
			},
		},
		{
			Name:        "plan_next",
			Description: "Return the next runnable step of the active plan (the first pending step in order whose depends_on are all done) plus the count of pending steps remaining. Response {step, remaining}: step is null when no step is runnable — if remaining>0 the plan is blocked on unmet dependencies, if remaining==0 the plan is complete. HOST LOOP: after plan establishes the plan, call plan_next, dispatch the returned step's prompt (review=true, at its suggested_model, gated by its suggested_verify), then plan_record its outcome and call plan_next again until step is null and remaining is 0. When step is null and remaining is 0, call plan_gate to decide whether the loop may terminate (it runs the integration verify_cmds and a goal-met judge).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "plan_record",
			Description: "Record the outcome of a plan step: set its status to \"done\" or \"failed\" (a failed record increments the step's attempt count) with an optional note. Returns the updated step and the count of pending steps remaining. An unknown step_id is invalid-params. Call after dispatching a step's prompt, then call plan_next for the next step.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"step_id": map[string]any{"type": "string", "description": "The id of the plan step to record."},
					"status":  map[string]any{"type": "string", "description": "The step outcome: \"done\" or \"failed\"."},
					"note":    map[string]any{"type": "string", "description": "Optional free-text note recording the outcome (e.g. a verify summary or failure reason)."},
				},
				"required": []string{"step_id", "status"},
			},
		},
		{
			Name:        "plan_revise",
			Description: "Adapt the remaining active plan: append new steps, update fields of existing steps by id (field-merge; a step's status is only changed when the update sets a non-empty status, so a done step is not reset), and remove steps by id. Returns {ok, remaining}. Use between steps to re-plan as you learn more, then continue driving with plan_next.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"add_steps":       map[string]any{"type": "array", "items": planStepSchema, "description": "New steps to append (each starts pending)."},
					"update_steps":    map[string]any{"type": "array", "items": planStepSchema, "description": "Steps to update by id; only non-empty fields are applied."},
					"remove_step_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Ids of steps to remove from the plan."},
				},
			},
		},
		{
			Name:        "plan_status",
			Description: "Return the full active plan state: {plan: {goal, steps, created_at}} with every step's status/attempts/note, or {plan: null} when no plan is active. Read-only snapshot for inspecting progress.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "plan_gate",
			Description: "The autonomous loop's stop gate: runs the active plan's integration verify_cmds in workdir AND a read-only goal-met judge, returning {steps_remaining, verify, goal_met, goal_met_reason, can_stop}. Call after plan_next reports no runnable steps (remaining 0) to decide whether the loop may terminate; can_stop is true only when no steps remain AND verify passed AND the goal is judged met.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workdir":     map[string]any{"type": "string", "description": "Optional working directory the integration verify_cmds run in and the goal-met judge is anchored to. Defaults to the server's working directory."},
					"judge_model": map[string]any{"type": "string", "description": "Optional exact model id for the read-only goal-met judge. Omit to use the default strong judge model."},
				},
			},
		},
		{
			Name:        "memory",
			Description: "Read jindo shared memory: one key's value, or {entries, insights} when key is omitted (insights = the cross-agent learning layer)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "agents",
			Description: "List the agent -> difficulty -> model routing table, plus a per-agent map reporting whether each agent's CLI is installed (available)",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "compact",
			Description: "Trigger memory compaction to bound the working set (drops superseded/expired entries, folds the cold tail into a digest, keeps last-N notes).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"max_entries": map[string]any{"type": "integer"},
					"max_notes":   map[string]any{"type": "integer"},
				},
			},
		},
		{
			Name:        "calibrate",
			Description: "Aggregate dispatch.log into a routing calibration report: status distribution per tier/model, signal match frequencies, near-threshold dispatch count, and suggested (advisory-only) threshold/weight adjustments. With apply=true, additionally derive conservative routing tuning from the report and write it to the routing overrides file (default .jindo/routing_overrides.json) that routing.ApplyOverrides consumes; without apply the call is report-only and writes nothing.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the dispatch.log JSONL file. Defaults to .jindo/dispatch.log relative to the server's working directory.",
					},
					"apply": map[string]any{
						"type":        "boolean",
						"description": "When true, derive conservative routing tuning from the report and write it to overrides_path (creating/overwriting the file). Defaults to false: report-only, no file is written.",
					},
					"overrides_path": map[string]any{
						"type":        "string",
						"description": "Where to write the derived routing overrides when apply=true. Defaults to .jindo/routing_overrides.json relative to the server's working directory.",
					},
				},
			},
		},
	}
}

// toolsList returns the tool catalog.
func (s *Server) toolsList(req *Request) Response {
	return resultResponse(req.ID, map[string]any{"tools": tools()})
}

// callParams is the tools/call parameter envelope: which tool, and its raw
// arguments object (decoded per-tool).
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolsCall parses the envelope and routes to the named tool. An undecodable
// envelope is invalid-params; an unknown tool name is method-not-found. A tool
// that runs but fails surfaces as an internal-error JSON-RPC response (hard
// error), per the transport contract, rather than an isError tool result.
func (s *Server) toolsCall(req *Request) Response {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	switch p.Name {
	case "dispatch":
		return s.callDispatch(req, p.Arguments)
	case "dispatch_async":
		return s.callDispatchAsync(req, p.Arguments)
	case "dispatch_multi":
		return s.callDispatchMulti(req, p.Arguments)
	case "job_status":
		return s.callJobStatus(req, p.Arguments)
	case "plan":
		return s.callPlan(req, p.Arguments)
	case "plan_next":
		return s.callPlanNext(req)
	case "plan_record":
		return s.callPlanRecord(req, p.Arguments)
	case "plan_revise":
		return s.callPlanRevise(req, p.Arguments)
	case "plan_status":
		return s.callPlanStatus(req)
	case "plan_gate":
		return s.callPlanGate(req, p.Arguments)
	case "memory":
		return s.callMemory(req, p.Arguments)
	case "agents":
		return s.callAgents(req)
	case "compact":
		return s.callCompact(req, p.Arguments)
	case "calibrate":
		return s.callCalibrate(req, p.Arguments)
	default:
		return errorResponse(req.ID, codeMethodNotFound, "unknown tool: "+p.Name)
	}
}

// dispatchArgs is the decoded {task, agent?, model?, priority?, guidance?,
// effort?, review?} argument shape shared by the sync "dispatch" and async
// "dispatch_async" tools.
type dispatchArgs struct {
	Task     string   `json:"task"`
	Agent    string   `json:"agent"`
	Model    string   `json:"model"`
	Priority string   `json:"priority"`
	Guidance string   `json:"guidance"`
	Effort   string   `json:"effort"`
	Review   bool     `json:"review"`
	Verify   []string `json:"verify"`
	Workdir  string   `json:"workdir"`
}

// runDispatch executes in against the orchestrator (via DispatchModel, which
// honors the optional model pin, the optional task-specific guidance, and the
// Review flag) and builds the JSON-able payload map both the sync and async
// tools return on success.
func runDispatch(o *orchestrator.Orchestrator, in dispatchArgs) (map[string]any, error) {
	res, err := o.DispatchModel(in.Task, in.Agent, in.Priority, in.Model, in.Guidance, in.Effort, in.Review, in.Verify, in.Workdir)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"agent":      res.Agent,
		"model":      res.Model,
		"difficulty": res.Difficulty,
		"result":     res.Result,
		"key":        res.Key,
		"status":     res.Status,
		"summary":    res.Summary,
		"rationale":  res.Rationale,
	}
	// Surface EVERY cross-model reviewer's outcome (verdict, reviewer, finding
	// counts) only when review:true ran, so the host can see WHAT each reviewer
	// found and gate the next step. Omitted for review-off dispatch, keeping the
	// legacy payload shape.
	if len(res.Reviews) > 0 {
		payload["reviews"] = res.Reviews
	}
	// Surface the OBJECTIVE verify gate's outcome only when verify commands ran,
	// so the host can see WHICH command failed (and its output/exit code) and gate
	// the next step. Omitted for a no-verify dispatch, keeping the legacy payload.
	if res.Verify != nil {
		payload["verify"] = res.Verify
	}
	// Surface the git-derived changed-files manifest only when the dispatch ran
	// in a git repo AND something changed, so the host can see WHAT files were
	// touched without inspecting the filesystem itself. Omitted (nil/empty)
	// outside a git repo or when nothing changed, keeping the legacy payload.
	if len(res.Files) > 0 {
		payload["files"] = res.Files
	}
	return payload, nil
}

// callDispatch runs the dispatch tool: decode {task, agent?, priority?, review?},
// route+execute via the orchestrator, and return the Result as a JSON text
// content block. review is opt-in (default false, legacy Dispatch path); when
// true, DispatchWithReview runs a best-effort cross-model peer review after the
// author result, which may change the returned status (e.g. "review_failed").
func (s *Server) callDispatch(req *Request, args json.RawMessage) Response {
	var in dispatchArgs
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.Task == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: task is required")
	}
	// Reject an unknown reasoning-effort level at the boundary (invalid-params)
	// before any dispatch work runs; "" is valid (use the tier default).
	if msg := validateEffort(in.Effort); msg != "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+msg)
	}
	// Validate the objective verify gate at the MCP boundary so an invalid/unsafe
	// command list is refused as invalid-params BEFORE any dispatch work runs,
	// rather than surfacing as an internal error mid-pipeline.
	if err := orchestrator.ValidateVerifyCmds(in.Verify); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	payload, err := runDispatch(s.o, in)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "dispatch failed: "+err.Error())
	}
	return textResult(req.ID, payload)
}

// callDispatchMulti runs the dispatch_multi tool: decode {task, models,
// guidance?, synthesis?, judge_model?}, fan the task out to every model in
// read-only propose mode via the orchestrator, and return the candidates (plus
// an optional judge synthesis) as a JSON text content block. task and a
// non-empty models list are required (else invalid-params).
func (s *Server) callDispatchMulti(req *Request, args json.RawMessage) Response {
	var in struct {
		Task       string   `json:"task"`
		Models     []string `json:"models"`
		Guidance   string   `json:"guidance"`
		Synthesis  string   `json:"synthesis"`
		JudgeModel string   `json:"judge_model"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.Task == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: task is required")
	}
	if len(in.Models) == 0 {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: models is required and must be non-empty")
	}
	res, err := s.o.DispatchMulti(in.Task, in.Models, in.Guidance, in.Synthesis, in.JudgeModel)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "dispatch_multi failed: "+err.Error())
	}
	candidates := make([]map[string]any, len(res.Candidates))
	for i, c := range res.Candidates {
		candidates[i] = map[string]any{
			"agent":  c.Agent,
			"model":  c.Model,
			"result": c.Result,
			"status": c.Status,
		}
	}
	payload := map[string]any{"candidates": candidates}
	if res.Synthesis != nil {
		payload["synthesis"] = map[string]any{
			"agent":  res.Synthesis.Agent,
			"model":  res.Synthesis.Model,
			"result": res.Synthesis.Result,
		}
	}
	return textResult(req.ID, payload)
}

// callDispatchAsync runs the dispatch_async tool: decode the same
// {task, agent?, priority?, review?} shape as dispatch, submit it to the job
// manager to run in the background, and return {job_id, status:"running"}
// immediately. The caller is expected to poll job_status with the returned id.
func (s *Server) callDispatchAsync(req *Request, args json.RawMessage) Response {
	var in dispatchArgs
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.Task == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: task is required")
	}
	// Reject an unknown reasoning-effort level at submit time (invalid-params),
	// so the caller learns immediately rather than polling job_status. "" is
	// valid (use the tier default).
	if msg := validateEffort(in.Effort); msg != "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+msg)
	}
	// Reject an invalid/unsafe verify list at submit time (invalid-params), so the
	// caller learns immediately rather than having to poll job_status only to find
	// the background job errored on validation.
	if err := orchestrator.ValidateVerifyCmds(in.Verify); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	id := s.jobs.Submit(func() (map[string]any, error) {
		return runDispatch(s.o, in)
	})
	return textResult(req.ID, map[string]any{
		"job_id": id,
		"status": jobs.StatusRunning,
	})
}

// callPlan runs the plan tool: decode {goal, agent?, model?}, require goal
// (invalid-params if empty), decompose it into an ordered step plan via the
// orchestrator (which runs a read-only planner and persists the plan to shared
// memory), and return {agent, model, steps, summary, key} as a JSON text block.
// A planner that produces no parseable plan surfaces as an internal error.
func (s *Server) callPlan(req *Request, args json.RawMessage) Response {
	var in struct {
		Goal  string `json:"goal"`
		Spec  string `json:"spec"`
		Agent string `json:"agent"`
		Model string `json:"model"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.Goal == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: goal is required")
	}
	res, err := s.o.Plan(in.Goal, in.Spec, in.Agent, in.Model)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "plan failed: "+err.Error())
	}
	// Establish the returned steps as the active, step-gated plan state (all
	// pending) so the host can drive it one step at a time via plan_next/
	// plan_record. Best-effort: a persistence failure must not fail the plan.
	psteps := make([]plan.Step, 0, len(res.Steps))
	for _, st := range res.Steps {
		psteps = append(psteps, plan.Step{
			ID: st.ID, Title: st.Title, Prompt: st.Prompt,
			Difficulty: st.Difficulty, SuggestedModel: st.SuggestedModel,
			SuggestedVerify: st.SuggestedVerify, DependsOn: st.DependsOn,
			Status: "pending",
		})
	}
	_ = s.plan.SaveWith(in.Goal, res.Spec, res.VerifyCmds, psteps)
	return textResult(req.ID, map[string]any{
		"agent":       res.Agent,
		"model":       res.Model,
		"steps":       res.Steps,
		"summary":     res.Summary,
		"key":         res.Key,
		"spec":        res.Spec,
		"verify_cmds": res.VerifyCmds,
		"note":        "Plan is now the active step-gated state (all pending). Drive it one step at a time: plan_next → dispatch the step's prompt (review=true, at suggested_model, gated by suggested_verify) → plan_record → repeat; plan_revise to adapt.",
	})
}

// callPlanNext runs the plan_next tool: return the next runnable step of the
// active plan (first pending step whose depends_on are all done, in order) plus
// the count of pending steps remaining. step is null when nothing is runnable —
// remaining>0 then means the plan is blocked on unmet deps, remaining==0 means
// it is complete.
func (s *Server) callPlanNext(req *Request) Response {
	step, remaining, ok := s.plan.Next()
	if !ok {
		return textResult(req.ID, map[string]any{"step": nil, "remaining": remaining})
	}
	return textResult(req.ID, map[string]any{"step": step, "remaining": remaining})
}

// callPlanRecord runs the plan_record tool: decode {step_id, status, note?},
// set the step's status ("done"|"failed"; failed increments attempts), and
// return the updated step plus the remaining pending count. Unknown step_id or
// an invalid status is invalid-params.
func (s *Server) callPlanRecord(req *Request, args json.RawMessage) Response {
	var in struct {
		StepID string `json:"step_id"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.StepID == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: step_id is required")
	}
	if in.Status != "done" && in.Status != "failed" {
		return errorResponse(req.ID, codeInvalidParams, `invalid params: status must be "done" or "failed"`)
	}
	step, err := s.plan.Record(in.StepID, in.Status, in.Note)
	if err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	_, remaining, _ := s.plan.Next()
	return textResult(req.ID, map[string]any{"step": step, "remaining": remaining})
}

// callPlanRevise runs the plan_revise tool: decode {add_steps?, update_steps?,
// remove_step_ids?} and adapt the remaining active plan (append new pending
// steps, field-merge updates by id, remove by id). Returns {ok, remaining}.
func (s *Server) callPlanRevise(req *Request, args json.RawMessage) Response {
	var in struct {
		AddSteps      []plan.Step `json:"add_steps"`
		UpdateSteps   []plan.Step `json:"update_steps"`
		RemoveStepIDs []string    `json:"remove_step_ids"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if err := s.plan.Revise(in.AddSteps, in.UpdateSteps, in.RemoveStepIDs); err != nil {
		return errorResponse(req.ID, codeInternalError, "plan_revise failed: "+err.Error())
	}
	_, remaining, _ := s.plan.Next()
	return textResult(req.ID, map[string]any{"ok": true, "remaining": remaining})
}

// callPlanStatus runs the plan_status tool: return the full active plan state,
// or {plan: null} when no plan is active.
func (s *Server) callPlanStatus(req *Request) Response {
	st, ok := s.plan.Load()
	if !ok {
		return textResult(req.ID, map[string]any{"plan": nil})
	}
	return textResult(req.ID, map[string]any{"plan": st})
}

// callPlanGate runs the plan_gate tool: the autonomous loop's stop gate. It
// loads the active plan, counts the not-yet-done steps, and asks the
// orchestrator to run the plan's INTEGRATION verify_cmds in workdir AND a
// read-only goal-met judge, returning {steps_remaining, verify, goal_met,
// goal_met_reason, can_stop}. No active plan is invalid-params; a gate config
// error (invalid verify_cmds) or judge/verify machinery failure surfaces as an
// internal error. can_stop is true only when no steps remain AND verify passed
// AND the goal is judged met.
func (s *Server) callPlanGate(req *Request, args json.RawMessage) Response {
	var in struct {
		Workdir    string `json:"workdir"`
		JudgeModel string `json:"judge_model"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	st, ok := s.plan.Load()
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: no active plan")
	}
	// Steps remaining for the gate is every step not terminally done (a failed
	// step counts as remaining — the goal is not complete while it stands).
	remaining := 0
	for _, step := range st.Steps {
		if step.Status != plan.StatusDone {
			remaining++
		}
	}
	res, err := s.o.PlanGate(st.Goal, st.Spec, st.VerifyCmds, remaining, in.Workdir, in.JudgeModel)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "plan_gate failed: "+err.Error())
	}
	return textResult(req.ID, map[string]any{
		"steps_remaining": res.StepsRemaining,
		"verify":          res.Verify,
		"goal_met":        res.GoalMet,
		"goal_met_reason": res.GoalMetReason,
		"can_stop":        res.CanStop,
	})
}

// defaultJobWaitSec and maxJobWaitSec bound job_status's long-poll: the server
// blocks up to wait_sec seconds (default/cap below) for the job to reach a
// terminal state before answering with whatever status it currently has.
const (
	defaultJobWaitSec = 25
	maxJobWaitSec     = 30
)

// callJobStatus runs the job_status tool: decode {job_id, wait_sec?}, long-poll
// the job manager for up to wait_sec seconds (default 25, capped at 30), and
// report the resulting status. An unknown job_id is an invalid-params error, a
// terminal job includes its full dispatch payload or error message, and a
// still-running job reports status "running" only.
func (s *Server) callJobStatus(req *Request, args json.RawMessage) Response {
	var in struct {
		JobID   string `json:"job_id"`
		WaitSec int    `json:"wait_sec"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.JobID == "" {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: job_id is required")
	}
	wait := in.WaitSec
	if wait <= 0 {
		wait = defaultJobWaitSec
	}
	if wait > maxJobWaitSec {
		wait = maxJobWaitSec
	}
	job, ok := s.jobs.Wait(in.JobID, time.Duration(wait)*time.Second)
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: unknown job_id: "+in.JobID)
	}
	switch job.Status {
	case jobs.StatusDone:
		return textResult(req.ID, map[string]any{
			"status": job.Status,
			"result": job.Result,
		})
	case jobs.StatusError:
		return textResult(req.ID, map[string]any{
			"status": job.Status,
			"error":  job.Err,
		})
	default:
		return textResult(req.ID, map[string]any{
			"status": jobs.StatusRunning,
		})
	}
}

// callMemory runs the memory tool: with a key, return {found, value} for that
// key; without a key, return the whole store from All(). Read cannot error
// (missing => found:false); All() can, and a store error is an internal error.
func (s *Server) callMemory(req *Request, args json.RawMessage) Response {
	var in struct {
		Key string `json:"key"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if in.Key != "" {
		value, found := s.o.Mem.Read(in.Key)
		return textResult(req.ID, map[string]any{
			"key":   in.Key,
			"found": found,
			"value": value,
		})
	}
	all, err := s.o.Mem.All()
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "memory read failed: "+err.Error())
	}
	// Surface both memory tiers: the flat task-entry store (entries) and the
	// curated cross-agent insight layer (insights). Insights are best-effort —
	// a read error there must not hide the entries the caller asked for.
	insights, _ := s.o.Mem.Insights()
	return textResult(req.ID, map[string]any{
		"entries":  all,
		"insights": insights,
	})
}

// callAgents runs the agents tool: return the routing agent -> tier -> model
// mapping alongside a per-agent install-availability map, as a JSON text
// content block. "agents" is the routing table; "available" reports whether
// each agent's CLI is currently installed (all true when the availability seam
// is unset, e.g. in tests/library use).
func (s *Server) callAgents(req *Request) Response {
	return textResult(req.ID, map[string]any{
		"agents":    routing.AgentsModels(),
		"available": routing.AgentAvailability(),
	})
}

// callCompact runs the compact tool: decode optional {max_entries, max_notes},
// apply sensible defaults, call Compact on the shared memory, and return a JSON
// summary of the CompactResult. TTLSeconds and Now are left at zero (disabling
// TTL dropping) to keep server-side compaction deterministic without a clock.
func (s *Server) callCompact(req *Request, args json.RawMessage) Response {
	var in struct {
		MaxEntries int `json:"max_entries"`
		MaxNotes   int `json:"max_notes"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	maxEntries := in.MaxEntries
	if maxEntries == 0 {
		maxEntries = 200
	}
	maxNotes := in.MaxNotes
	if maxNotes == 0 {
		maxNotes = 200
	}
	opts := memory.CompactOptions{
		MaxEntries: maxEntries,
		MaxNotes:   maxNotes,
		// TTLSeconds and Now intentionally left 0: TTL dropping requires a
		// non-zero Now to be safe; callers that want TTL should set it via the
		// CLI or MaybeCompact, not the MCP tool.
		TTLSeconds: 0,
		Now:        0,
		Summarize:  nil,
	}
	res, err := s.o.Mem.Compact(opts)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "compact failed: "+err.Error())
	}
	return textResult(req.ID, map[string]any{
		"entries_before": res.EntriesBefore,
		"entries_after":  res.EntriesAfter,
		"notes_before":   res.NotesBefore,
		"notes_after":    res.NotesAfter,
		"folded":         res.Folded,
		"digested":       res.Digested,
	})
}

// defaultDispatchLogPath is dispatch.log's location relative to the server's
// working directory when no path is supplied, matching the memory root
// (".jindo") that cmd/jindo-mcp/main.go passes to memory.New and that
// appendDispatchLog writes dispatch.log under.
const defaultDispatchLogPath = ".jindo/dispatch.log"

// defaultOverridesPath is where the calibrate apply path writes derived routing
// tuning when overrides_path is not supplied. It matches the file
// routing.ApplyOverrides reads under the memory root (".jindo").
const defaultOverridesPath = ".jindo/routing_overrides.json"

// callCalibrate runs the calibrate tool: decode optional {path, apply,
// overrides_path}, default path to defaultDispatchLogPath, load and aggregate
// the dispatch.log, and return its rendered report as text.
//
// The default (apply=false or absent) is report-only and byte-identical to the
// pre-apply behavior: no file is written. With apply=true it additionally
// derives conservative routing tuning from the report and writes it to
// overrides_path (the file routing.ApplyOverrides consumes), reporting the path
// and applied deltas — or "no changes to apply" for a clean log, in which case
// no file is written so an operator's existing overrides are never clobbered.
func (s *Server) callCalibrate(req *Request, args json.RawMessage) Response {
	var in struct {
		Path          string `json:"path"`
		Apply         bool   `json:"apply"`
		OverridesPath string `json:"overrides_path"`
	}
	if err := decodeArgs(args, &in); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	path := in.Path
	if path == "" {
		path = defaultDispatchLogPath
	}
	report, err := calibrate.Load(path)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "calibrate failed: "+err.Error())
	}

	result := map[string]any{
		"report": report.String(),
	}

	if in.Apply {
		overridesPath := in.OverridesPath
		if overridesPath == "" {
			overridesPath = defaultOverridesPath
		}
		ov := report.DeriveOverrides()
		if ov.Empty() {
			result["applied"] = false
			result["apply_message"] = "no changes to apply"
		} else {
			data, err := ov.Marshal()
			if err != nil {
				return errorResponse(req.ID, codeInternalError, "calibrate apply: marshal overrides failed: "+err.Error())
			}
			if err := os.WriteFile(overridesPath, data, 0o644); err != nil {
				return errorResponse(req.ID, codeInternalError, "calibrate apply: write overrides failed: "+err.Error())
			}
			result["applied"] = true
			result["overrides_path"] = overridesPath
			result["overrides"] = ov
		}
	}

	return textResult(req.ID, result)
}

// decodeArgs unmarshals a tool's raw arguments into out, treating absent/empty
// arguments as an empty object (so tools with all-optional fields work when the
// client omits "arguments" entirely).
func decodeArgs(args json.RawMessage, out any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, out)
}

// textResult wraps payload as a single JSON text content block in an MCP
// tool-result: {content: [{type:"text", text: <json>}], isError: false}. The
// payload is JSON-encoded into the text field so structured data survives the
// text-only content channel.
func textResult(id json.RawMessage, payload any) Response {
	text, err := json.Marshal(payload)
	if err != nil {
		return errorResponse(id, codeInternalError, "marshal result failed: "+err.Error())
	}
	return resultResponse(id, map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": string(text)},
		},
		"isError": false,
	})
}

// resultResponse builds a success response with the given id and result.
func resultResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

// errorResponse builds an error response with the given id, code, and message.
func errorResponse(id json.RawMessage, code int, message string) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// mustMarshal encodes a Response to bytes. A Response is composed only of
// JSON-safe values, so marshaling cannot realistically fail; if it somehow
// does, a hand-built internal-error line is returned so the transport always
// has a valid line to write.
func mustMarshal(resp Response) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		return []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":"marshal response failed"}}`,
			codeInternalError))
	}
	return b
}
