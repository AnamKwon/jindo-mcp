// Package agentproto defines the response contract shared by the orchestrator
// and the headless coding agents it drives. It is pure and side-effect free:
// it only builds the system-prompt text and parses agent stdout back into the
// structured Response contract. No I/O, no dependency on memory/agent packages.
package agentproto

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responseContractBlock is the literal status/result/summary/memory_updates
// (+memory_used) JSON schema text that terminates the read-only prompts
// (BuildProposePrompt, BuildJudgePrompt). It is the SAME contract BuildSystemPrompt
// spells out inline, factored out here only so the propose/judge prompts stay
// byte-identical to what ParseResponse expects without re-typing the schema.
const responseContractBlock = `{
  "status": string,   // outcome, e.g. "ok" or "error"
  "result": string,   // the concrete result / deliverable
  "summary": string,  // short summary of what you did
  "memory_updates": [ // array of context entries to persist (may be empty)
    {
      "key":   string, // optional: identifier for a stored value
      "note":  string, // optional: free-form note for later agents
      "value": any     // optional: the value stored under key
    }
  ],
  "memory_used": [ // optional: keys from shared memory you actually read
    string          // and relied on (may be empty or omitted)
  ]
}

Emit only ONE such JSON object and make it the final content of your output so
it can be parsed reliably. Code fences are optional, but the object must be
valid JSON.
`

// MemoryUpdate is one entry an agent may emit to record context for later
// agents. Every field is optional: a keyed value (Key+Value) records a fact in
// the shared store, while a free-form Note leaves a human-readable breadcrumb.
type MemoryUpdate struct {
	Key   string `json:"key,omitempty"`
	Note  string `json:"note,omitempty"`
	Value any    `json:"value,omitempty"`
}

// Response is the single JSON block a headless agent must emit at the end of
// its output. Status/Result/Summary are always present in the contract;
// MemoryUpdates carries the context the agent wants persisted.
type Response struct {
	Status        string         `json:"status"`
	Result        string         `json:"result"`
	Summary       string         `json:"summary"`
	MemoryUpdates []MemoryUpdate `json:"memory_updates"`
	MemoryUsed    []string       `json:"memory_used,omitempty"`
}

// BuildSystemPrompt returns the system-prompt text that instructs a headless
// coding agent to (a) read the shared bounded memory under memoryDir before
// doing anything, (b) perform the requested work, and (c) terminate its output
// with exactly one JSON block matching the Response contract. The schema is
// spelled out literally so the agent emits output that ParseResponse can read.
func BuildSystemPrompt(memoryDir string) string {
	var b strings.Builder

	b.WriteString("You are a headless coding agent driven by an orchestrator.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before doing any work, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context: memory.json and the .jindo store\n")
	b.WriteString("record facts and notes left by earlier agents. Load that context so your work\n")
	b.WriteString("builds on what is already known instead of repeating or contradicting it.\n")
	b.WriteString("Some older context may already be folded into a single reserved entry named\n")
	b.WriteString("\"_digest\" rather than kept as individual entries — if present, treat its body\n")
	b.WriteString("as a compressed summary of historical facts and notes, not as noise to skip.\n")
	b.WriteString("Check for both the live recent entries and \"_digest\" explicitly; reading only\n")
	b.WriteString("one of the two will silently lose context. Keep track of which memory keys you\n")
	b.WriteString("actually relied on, so you can list them in memory_used in your final report.\n\n")

	b.WriteString("STEP 2 - DO THE REQUESTED WORK.\n")
	b.WriteString("Carry out the task described in the user prompt. If the task is ambiguous or\n")
	b.WriteString("underspecified, do not silently guess: state the assumption you are making and\n")
	b.WriteString("proceed on that basis. Prefer the smallest correct change that satisfies the\n")
	b.WriteString("task - touch only the code the task actually requires, and avoid unrelated\n")
	b.WriteString("refactors or cleanup. Before reporting, verify your own work: run the relevant\n")
	b.WriteString("tests, build, or type-check where possible, and describe how you verified the\n")
	b.WriteString("result. Note any edge cases you considered, and explicitly state what you did\n")
	b.WriteString("NOT do or left out of scope.\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this response contract (a single top-level {...} block):\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"status\": string,   // outcome, e.g. \"ok\" or \"error\"\n")
	b.WriteString("  \"result\": string,   // the concrete result / deliverable\n")
	b.WriteString("  \"summary\": string,  // short summary of what you did\n")
	b.WriteString("  \"memory_updates\": [ // array of context entries to persist (may be empty)\n")
	b.WriteString("    {\n")
	b.WriteString("      \"key\":   string, // optional: identifier for a stored value\n")
	b.WriteString("      \"note\":  string, // optional: free-form note for later agents\n")
	b.WriteString("      \"value\": any     // optional: the value stored under key\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"memory_used\": [ // optional: keys from shared memory you actually read\n")
	b.WriteString("    string          // and relied on (may be empty or omitted)\n")
	b.WriteString("  ]\n")
	b.WriteString("}\n\n")
	b.WriteString("Emit only ONE such JSON object and make it the final content of your output so\n")
	b.WriteString("it can be parsed reliably. Code fences are optional, but the object must be\n")
	b.WriteString("valid JSON.\n")

	return b.String()
}

// ReviewFinding is one issue a cross-model reviewer raises against the author's
// work. Severity is one of "critical", "major", "minor", "info"; a "critical"
// finding is the signal the orchestrator acts on (forces a revision round).
type ReviewFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"message,omitempty"`
}

// ReviewResponse is the single JSON block a reviewer agent must emit at the end
// of its output. Verdict is one of "approved", "changes_requested",
// "rejected"; Findings carries the individual issues; Summary is a short
// human-readable overview.
type ReviewResponse struct {
	Verdict  string          `json:"verdict"`
	Findings []ReviewFinding `json:"findings"`
	Summary  string          `json:"summary"`
}

// VerdictUnparsed is the sentinel verdict ParseReviewResponse returns when no
// review JSON block could be recovered from the reviewer's stdout. It is not a
// valid emitted verdict (the schema only allows approved/changes_requested/
// rejected), so callers can treat it as a reviewer failure without ambiguity.
const VerdictUnparsed = "unparsed"

// ReviewArtifacts carries the REAL evidence of the author's work into the
// review prompt, so the reviewer judges the actual change rather than only the
// author's self-report. Both fields are optional: an empty ReviewArtifacts
// makes BuildReviewPrompt behave exactly as before (no artifact sections).
type ReviewArtifacts struct {
	// ChangedFiles is the repo-relative paths the author's work created,
	// modified, or deleted. When non-empty the reviewer is told to OPEN and
	// inspect these files (it has repo read access via --add-dir).
	ChangedFiles []string
	// ExecOutput is captured execution/verify output, if any, included verbatim
	// as additional evidence.
	ExecOutput string
}

// BuildReviewPrompt returns the system-prompt text that instructs a headless
// reviewer agent to (a) read the shared bounded memory under memoryDir, (b)
// review the author's work (authorResult) against the task it was given
// (authorTask) — inspecting the real artifacts in arts when supplied rather
// than trusting the self-report — and (c) terminate its output with exactly one
// JSON block matching the ReviewResponse contract. The schema is spelled out
// literally — mirroring BuildSystemPrompt — so the reviewer emits output
// ParseReviewResponse can read.
func BuildReviewPrompt(memoryDir, authorTask, authorResult string, arts ReviewArtifacts) string {
	var b strings.Builder

	b.WriteString("You are a headless PEER REVIEWER driven by an orchestrator. You do NOT do\n")
	b.WriteString("the work yourself; you review another agent's work and report findings.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before reviewing, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context (memory.json and the .jindo store,\n")
	b.WriteString("plus a possible reserved \"_digest\" summary of older facts). Load that context\n")
	b.WriteString("so your review is grounded in what the author actually had available.\n\n")

	b.WriteString("STEP 2 - REVIEW THE AUTHOR'S WORK AGAINST THE TASK.\n")
	b.WriteString("The author was asked to do this task:\n")
	b.WriteString("---- TASK ----\n")
	b.WriteString(authorTask)
	b.WriteString("\n---- END TASK ----\n\n")
	b.WriteString("The author reported this result:\n")
	b.WriteString("---- AUTHOR RESULT ----\n")
	b.WriteString(authorResult)
	b.WriteString("\n---- END AUTHOR RESULT ----\n\n")

	// Real artifacts (additive): when present, steer the reviewer to inspect the
	// actual change instead of trusting the self-report above.
	if len(arts.ChangedFiles) > 0 {
		b.WriteString("The author's work changed these files (paths relative to the repo root):\n")
		b.WriteString("---- CHANGED FILES ----\n")
		for _, p := range arts.ChangedFiles {
			b.WriteString(p)
			b.WriteString("\n")
		}
		b.WriteString("---- END CHANGED FILES ----\n")
		b.WriteString("Do NOT trust the self-report alone. You have repository read access\n")
		b.WriteString("(--add-dir), so OPEN and inspect these actually-changed files to confirm\n")
		b.WriteString("what the author really did before you judge the work.\n\n")
	}
	if arts.ExecOutput != "" {
		b.WriteString("The author's execution/verify produced this output:\n")
		b.WriteString("---- EXECUTION / VERIFY OUTPUT ----\n")
		b.WriteString(arts.ExecOutput)
		b.WriteString("\n---- END EXECUTION / VERIFY OUTPUT ----\n")
		b.WriteString("Consider this output as evidence when judging whether the work is correct.\n\n")
	}

	b.WriteString("Judge whether the result correctly and completely satisfies the task. Verify\n")
	b.WriteString("the change actually does what the task asked, preserves existing invariants,\n")
	b.WriteString("and handles edge cases the author may have missed. Distinguish must-fix\n")
	b.WriteString("defects from mere nits: raise a finding of severity \"critical\" ONLY for a\n")
	b.WriteString("defect that must be fixed before the work can be accepted (wrong behavior,\n")
	b.WriteString("broken invariant, unsafe change); use \"major\", \"minor\", or \"info\" for lesser\n")
	b.WriteString("concerns.\n\n")

	b.WriteString("SECURITY CHECKLIST.\n")
	b.WriteString("Regardless of the task's stated scope, look for these classes of defect:\n")
	b.WriteString("  - injection (SQL injection, command injection, XSS)\n")
	b.WriteString("  - hardcoded secrets or credentials\n")
	b.WriteString("  - authorization or permission gaps\n")
	b.WriteString("  - unsafe operations (eval/exec, unsafe deserialization)\n")
	b.WriteString("  - missing input validation\n")
	b.WriteString("Raise a finding of severity \"critical\" or \"major\" for any real security\n")
	b.WriteString("defect you find.\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this review contract (a single top-level {...} block):\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": string,   // one of \"approved\", \"changes_requested\", \"rejected\"\n")
	b.WriteString("  \"findings\": [        // array of findings (may be empty)\n")
	b.WriteString("    {\n")
	b.WriteString("      \"severity\": string, // one of \"critical\", \"major\", \"minor\", \"info\"\n")
	b.WriteString("      \"title\":    string, // short label for the issue\n")
	b.WriteString("      \"message\":  string  // what is wrong and what to do about it\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"summary\": string    // short overall summary of your review\n")
	b.WriteString("}\n\n")
	b.WriteString("Emit only ONE such JSON object and make it the final content of your output so\n")
	b.WriteString("it can be parsed reliably. Code fences are optional, but the object must be\n")
	b.WriteString("valid JSON.\n")

	return b.String()
}

// ParseReviewResponse extracts the last balanced top-level JSON object from
// arbitrary reviewer stdout and unmarshals it into a ReviewResponse, reusing the
// same tolerant brace-depth scanner as ParseResponse (topLevelObjectSpans). It
// prefers the LAST span that both unmarshals cleanly AND looks like a review (a
// non-empty verdict or at least one finding), so an example object printed
// earlier in the prose — or an unrelated {} — does not masquerade as the result.
//
// On any failure (no object found, or none looks like a review) it returns
// ReviewResponse{Verdict: VerdictUnparsed, Summary: stdout}, so callers can
// treat an unparseable review as a reviewer failure. It never panics.
func ParseReviewResponse(stdout string) ReviewResponse {
	spans := topLevelObjectSpans(stdout)

	for i := len(spans) - 1; i >= 0; i-- {
		var rr ReviewResponse
		if err := json.Unmarshal([]byte(spans[i]), &rr); err != nil {
			continue
		}
		// Skip objects that do not look like a review (e.g. a stray {} or an
		// unrelated JSON block) so they cannot be mistaken for an empty verdict.
		if rr.Verdict == "" && len(rr.Findings) == 0 {
			continue
		}
		return rr
	}

	return ReviewResponse{Verdict: VerdictUnparsed, Summary: stdout}
}

// HasCritical reports whether any finding has severity "critical" — the single
// condition the orchestrator uses to force a revision round.
func HasCritical(findings []ReviewFinding) bool {
	for _, f := range findings {
		if f.Severity == "critical" {
			return true
		}
	}
	return false
}

// ParseResponse extracts the last balanced top-level JSON object from arbitrary
// agent stdout and unmarshals it into a Response. Agents print prose followed
// by a JSON block (and prose may itself contain example objects), so the scan
// tracks brace depth while skipping string literals — braces inside strings do
// not count toward depth. Every complete top-level {...} span is collected, and
// the LAST one that unmarshals cleanly into a Response wins.
//
// On success the parsed Response is returned; if the agent left status empty it
// is defaulted to "ok", but a supplied status is never overwritten. On any
// failure (no object found, or none unmarshals cleanly) it returns
// Response{Status: "unparsed", Result: stdout}. It never panics.
func ParseResponse(stdout string) Response {
	spans := topLevelObjectSpans(stdout)

	// Prefer the LAST span that unmarshals cleanly, so the agent's final block
	// wins over any example objects printed earlier in the prose.
	for i := len(spans) - 1; i >= 0; i-- {
		var resp Response
		if err := json.Unmarshal([]byte(spans[i]), &resp); err != nil {
			continue
		}
		if resp.Status == "" {
			resp.Status = "ok"
		}
		return resp
	}

	return Response{Status: "unparsed", Result: stdout}
}

// PlanStep is one node of a decomposed plan: a small, independently-verifiable
// unit of work. ID is a stable identifier other steps reference via DependsOn.
// Title is a short human label; Prompt is the concrete task text the caller
// dispatches to a sub-agent to actually perform the step.
// Difficulty ("trivial"|"standard"|"hard") drives the model tier the caller
// runs the step at; SuggestedModel is the exact model to run it; SuggestedVerify
// lists allowlisted test/build commands that gate the step; DependsOn lists the
// prerequisite step ids that must complete first.
type PlanStep struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	Difficulty      string   `json:"difficulty"`
	SuggestedModel  string   `json:"suggested_model"`
	SuggestedVerify []string `json:"suggested_verify"`
	DependsOn       []string `json:"depends_on"`
}

// BuildPlanPrompt returns the system-prompt text that instructs a headless
// PLANNER to (a) read the shared bounded memory under memoryDir (like the other
// prompts), (b) decompose the GOAL into small, ordered, independently-verifiable
// steps WITHOUT doing the work or editing files, and (c) terminate its output
// with exactly one JSON block matching the plan contract {steps,summary,
// verify_cmds}. The
// schema is spelled out literally — mirroring BuildSystemPrompt — so the planner
// emits output ParsePlanResponse can read.
func BuildPlanPrompt(goal, memoryDir string) string {
	var b strings.Builder

	b.WriteString("You are a headless PLANNER driven by an orchestrator. You do NOT do the\n")
	b.WriteString("work yourself and you MUST NOT edit, create, or delete any files; you only\n")
	b.WriteString("read context and produce an ordered plan for OTHER agents to execute.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before planning, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context (memory.json and the .jindo store,\n")
	b.WriteString("plus a possible reserved \"_digest\" summary of older facts). Load that context\n")
	b.WriteString("so the plan builds on what is already known instead of repeating or\n")
	b.WriteString("contradicting it.\n\n")

	b.WriteString("STEP 2 - DECOMPOSE THE GOAL INTO STEPS.\n")
	b.WriteString("The goal to plan is:\n")
	b.WriteString("---- GOAL ----\n")
	b.WriteString(goal)
	b.WriteString("\n---- END GOAL ----\n\n")
	b.WriteString("Break the goal into small, ordered, independently-verifiable steps. For each\n")
	b.WriteString("step decide:\n")
	b.WriteString("  - prompt: the concrete, self-contained task text to hand a sub-agent to\n")
	b.WriteString("    actually perform THIS step (what to do and where), written as an\n")
	b.WriteString("    executable instruction — not a restatement of the title. The orchestrator\n")
	b.WriteString("    dispatches this prompt verbatim to an executor.\n")
	b.WriteString("  - difficulty: one of \"trivial\", \"standard\", \"hard\" — this drives the model\n")
	b.WriteString("    tier the orchestrator runs the step at (harder work gets a stronger model).\n")
	b.WriteString("  - suggested_model: the exact model id to run that step (e.g. a strong model\n")
	b.WriteString("    for a \"hard\" step, a cheaper one for a \"trivial\" step).\n")
	b.WriteString("  - suggested_verify: allowlisted test/build commands (each ONE program with\n")
	b.WriteString("    args, NO shell pipes/redirects) that gate the step — how to prove it is done.\n")
	b.WriteString("    EVERY step must carry at least one real, runnable suggested_verify command\n")
	b.WriteString("    (a test, build, or lint invocation an executor can actually run) that proves\n")
	b.WriteString("    that specific step is complete — not a generic project-wide command copied\n")
	b.WriteString("    across steps. If a step has no way to verify it (no test, build, or lint can\n")
	b.WriteString("    observe its effect), redesign or merge the step instead of leaving it\n")
	b.WriteString("    unverifiable.\n")
	b.WriteString("  - depends_on: the ids of prerequisite steps that must complete first (may be\n")
	b.WriteString("    empty for steps with no prerequisites).\n")
	b.WriteString("Give each step a short stable id (e.g. \"s1\", \"s2\") that later steps reference in\n")
	b.WriteString("depends_on. Do NOT perform the steps and do NOT modify any files — only plan.\n\n")
	b.WriteString("ALSO emit, in the SAME JSON block, a top-level \"verify_cmds\" array: the\n")
	b.WriteString("INTEGRATION verification commands that prove the WHOLE goal is done, distinct\n")
	b.WriteString("from each step's suggested_verify (which proves only that one step). Each entry\n")
	b.WriteString("is an allowlisted command, ONE program with args, NO shell pipes/redirects or\n")
	b.WriteString("other metacharacters. Choose commands that, if they all pass, demonstrate the\n")
	b.WriteString("overall goal is complete (may be empty if none apply).\n\n")
	b.WriteString("SECURITY-SENSITIVE STEPS.\n")
	b.WriteString("If a step touches authentication, authorization, secrets, input handling, or\n")
	b.WriteString("file/exec/network operations, flag it: prefix that step's title with\n")
	b.WriteString("\"[SECURITY]\" and set difficulty to at least \"standard\" so it gets extra review\n")
	b.WriteString("and verification, and make sure its suggested_verify actually exercises the\n")
	b.WriteString("security-relevant behavior (e.g. an auth test, not just a build).\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this plan contract (a single top-level {...} block):\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"steps\": [           // ordered array of steps (at least one)\n")
	b.WriteString("    {\n")
	b.WriteString("      \"id\":               string, // stable id, referenced by depends_on\n")
	b.WriteString("      \"title\":            string, // what this step accomplishes\n")
	b.WriteString("      \"prompt\":           string, // concrete task text to dispatch for this step\n")
	b.WriteString("      \"difficulty\":       string, // \"trivial\" | \"standard\" | \"hard\"\n")
	b.WriteString("      \"suggested_model\":  string, // exact model id to run this step\n")
	b.WriteString("      \"suggested_verify\": [string], // allowlisted verify commands (may be empty)\n")
	b.WriteString("      \"depends_on\":       [string]  // prerequisite step ids (may be empty)\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"summary\": string,   // short overview of the overall plan\n")
	b.WriteString("  \"verify_cmds\": [string] // INTEGRATION verify commands proving the whole goal\n")
	b.WriteString("}\n\n")
	b.WriteString("Emit only ONE such JSON object and make it the final content of your output so\n")
	b.WriteString("it can be parsed reliably. Code fences are optional, but the object must be\n")
	b.WriteString("valid JSON.\n")

	return b.String()
}

// BuildProposePrompt returns the system-prompt text that instructs a headless
// agent to SOLVE task in READ-ONLY "propose" mode: it reads the shared bounded
// memory under memoryDir, produces its COMPLETE candidate solution in the
// response contract's result field, but MUST NOT write, create, or edit any
// files (it is one of several candidates fanned out concurrently; a later step
// applies the chosen solution). It ends with the SAME status/result/summary/
// memory_updates JSON block as BuildSystemPrompt so ParseResponse reads it
// unchanged.
func BuildProposePrompt(memoryDir, task string) string {
	var b strings.Builder

	b.WriteString("You are a headless agent driven by an orchestrator, working in READ-ONLY\n")
	b.WriteString("\"propose\" mode. You are ONE of several candidate agents solving the SAME\n")
	b.WriteString("task in parallel; a later step picks or synthesizes the best solution and\n")
	b.WriteString("applies it. Your job is to produce your complete candidate solution as TEXT,\n")
	b.WriteString("NOT to change the project.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before doing any work, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context (memory.json and the .jindo store,\n")
	b.WriteString("plus a possible reserved \"_digest\" summary of older facts). Load that context\n")
	b.WriteString("so your solution builds on what is already known instead of repeating or\n")
	b.WriteString("contradicting it.\n\n")

	b.WriteString("STEP 2 - SOLVE THE TASK (DO NOT WRITE FILES).\n")
	b.WriteString("The task to solve is:\n")
	b.WriteString("---- TASK ----\n")
	b.WriteString(task)
	b.WriteString("\n---- END TASK ----\n\n")
	b.WriteString("Solve it completely and put your ENTIRE solution in the result field of the\n")
	b.WriteString("JSON block below: for a coding task, the full code as text (complete enough to\n")
	b.WriteString("apply directly); for a question, the answer WITH brief reasoning. You may read\n")
	b.WriteString("files and memory to ground your answer, but you MUST NOT write, create, edit,\n")
	b.WriteString("or delete any files — do not modify the project in any way. Return the solution\n")
	b.WriteString("as text only; a later step applies the chosen candidate.\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this response contract (a single top-level {...} block):\n\n")
	b.WriteString(responseContractBlock)

	return b.String()
}

// BuildJudgePrompt returns the system-prompt text that instructs a headless
// JUDGE to read task and the N candidate solutions (embedded below, numbered and
// delimited) and produce the SINGLE best synthesized solution — merging the
// candidates' strengths, picking the best, and correcting errors — putting the
// final synthesized solution in result and a brief rationale in summary. It is
// READ-ONLY (proposes a synthesis as text; does not modify files) and ends with
// the SAME status/result/summary/memory_updates JSON block as BuildSystemPrompt
// so ParseResponse reads it unchanged.
func BuildJudgePrompt(memoryDir, task string, candidates []string) string {
	var b strings.Builder

	b.WriteString("You are a headless JUDGE driven by an orchestrator. Several candidate agents\n")
	b.WriteString("each solved the SAME task independently; your job is to read all of their\n")
	b.WriteString("solutions and produce the SINGLE best synthesized solution. You do NOT modify\n")
	b.WriteString("any files — you return the synthesized solution as TEXT.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before judging, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context (memory.json and the .jindo store,\n")
	b.WriteString("plus a possible reserved \"_digest\" summary of older facts). Load that context\n")
	b.WriteString("so your synthesis is grounded in what the candidates actually had available.\n\n")

	b.WriteString("STEP 2 - SYNTHESIZE THE BEST SOLUTION FROM THE CANDIDATES.\n")
	b.WriteString("The task the candidates solved is:\n")
	b.WriteString("---- TASK ----\n")
	b.WriteString(task)
	b.WriteString("\n---- END TASK ----\n\n")
	b.WriteString("Here are the candidate solutions to synthesize:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "---- CANDIDATE %d ----\n", i+1)
		b.WriteString(c)
		fmt.Fprintf(&b, "\n---- END CANDIDATE %d ----\n", i+1)
	}
	b.WriteString("\nProduce the single best solution: merge the candidates' strengths, pick the\n")
	b.WriteString("best where they conflict, and correct any errors you find. Put the final\n")
	b.WriteString("synthesized solution in the result field of the JSON block below (for code,\n")
	b.WriteString("the full code as text; for a question, the answer WITH brief reasoning) and a\n")
	b.WriteString("brief rationale for your synthesis in summary. Do NOT write, create, edit, or\n")
	b.WriteString("delete any files — return the synthesized solution as text only.\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this response contract (a single top-level {...} block):\n\n")
	b.WriteString(responseContractBlock)

	return b.String()
}

// BuildGoalCheckPrompt returns the system-prompt text that instructs a headless
// GOAL-MET JUDGE to decide STRICTLY whether a stated goal (and its clarifying
// spec) is ACTUALLY met by the current state of the repository — not merely that
// some code changed or an agent claimed success. It reads the shared bounded
// memory under memoryDir AND inspects the repository/working directory, weighs
// the objective integrationSummary (the machine verify signal the loop already
// ran), and MUST NOT write, create, or edit any files. It terminates with
// exactly one JSON block {"goal_met": true|false, "reason": "..."} so
// ParseGoalCheckResponse reads it unchanged. It is the JUDGED half of the
// autonomous loop's stop gate (the objective half is the integration verify).
func BuildGoalCheckPrompt(memoryDir, goal, spec, integrationSummary string) string {
	var b strings.Builder

	b.WriteString("You are a headless GOAL-MET JUDGE driven by an orchestrator, working in\n")
	b.WriteString("READ-ONLY mode. Your job is to decide STRICTLY whether the stated goal is\n")
	b.WriteString("ACTUALLY met by the current state of the repository — not merely whether some\n")
	b.WriteString("code changed or an agent claimed success. You do NOT modify any files; you\n")
	b.WriteString("return only a verdict.\n\n")

	b.WriteString("STEP 1 - READ SHARED MEMORY FIRST.\n")
	b.WriteString("Before judging, read the shared memory under the bounded directory:\n")
	b.WriteString("    ")
	b.WriteString(memoryDir)
	b.WriteString("\n")
	b.WriteString("This directory holds prior agents' context (memory.json and the .jindo store,\n")
	b.WriteString("plus a possible reserved \"_digest\" summary of older facts). Load that context\n")
	b.WriteString("so your judgment is grounded in what was actually attempted.\n\n")

	b.WriteString("STEP 2 - INSPECT THE REPOSITORY AND JUDGE THE GOAL.\n")
	b.WriteString("The goal to judge is:\n")
	b.WriteString("---- GOAL ----\n")
	b.WriteString(goal)
	b.WriteString("\n---- END GOAL ----\n\n")
	if spec != "" {
		b.WriteString("The clarified intent (spec) that further defines \"done\" is:\n")
		b.WriteString("---- SPEC ----\n")
		b.WriteString(spec)
		b.WriteString("\n---- END SPEC ----\n\n")
	}
	b.WriteString("The objective integration-verify signal (the build/test/lint the loop already\n")
	b.WriteString("ran) is:\n")
	b.WriteString("    ")
	b.WriteString(integrationSummary)
	b.WriteString("\n\n")
	b.WriteString("Inspect the repository/working directory (read files, look at the actual\n")
	b.WriteString("implementation and its tests) and judge STRICTLY whether the goal AND spec are\n")
	b.WriteString("genuinely satisfied by what is present — not merely that files were touched or\n")
	b.WriteString("that some progress was made. If any required part is missing, incomplete, or\n")
	b.WriteString("unverified, the goal is NOT met. You may read files and memory, but you MUST\n")
	b.WriteString("NOT write, create, edit, or delete any files.\n\n")

	b.WriteString("STEP 3 - END WITH EXACTLY ONE JSON BLOCK.\n")
	b.WriteString("After any prose, the LAST thing in your output must be exactly one JSON object\n")
	b.WriteString("matching this goal-check contract (a single top-level {...} block):\n\n")
	b.WriteString("{\n")
	b.WriteString("  \"goal_met\": true|false, // STRICT: true only if the goal+spec are actually met\n")
	b.WriteString("  \"reason\":   string      // brief justification for the verdict\n")
	b.WriteString("}\n\n")
	b.WriteString("Emit only ONE such JSON object and make it the final content of your output so\n")
	b.WriteString("it can be parsed reliably. Code fences are optional, but the object must be\n")
	b.WriteString("valid JSON.\n")

	return b.String()
}

// ParsePlanResponse extracts the last balanced top-level JSON object from
// arbitrary planner stdout and unmarshals it into {steps,summary,verify_cmds},
// reusing the same tolerant brace-depth scanner as ParseResponse
// (topLevelObjectSpans). It prefers the LAST span that both unmarshals cleanly
// AND carries a non-empty steps array, so an example object printed earlier in
// the prose — or an unrelated {} — does not masquerade as the plan. verifyCmds
// is the plan's INTEGRATION gate (proving the whole goal), distinct from each
// step's suggested_verify.
//
// ok is false when no object with a non-empty steps array is found; in that case
// steps is nil, summary is empty, and verifyCmds is nil. It never panics.
func ParsePlanResponse(stdout string) (steps []PlanStep, summary string, verifyCmds []string, ok bool) {
	spans := topLevelObjectSpans(stdout)

	for i := len(spans) - 1; i >= 0; i-- {
		var plan struct {
			Steps      []PlanStep `json:"steps"`
			Summary    string     `json:"summary"`
			VerifyCmds []string   `json:"verify_cmds"`
		}
		if err := json.Unmarshal([]byte(spans[i]), &plan); err != nil {
			continue
		}
		// Skip objects that do not look like a plan (e.g. a stray {} or an
		// unrelated JSON block) so they cannot be mistaken for an empty plan.
		if len(plan.Steps) == 0 {
			continue
		}
		return plan.Steps, plan.Summary, plan.VerifyCmds, true
	}

	return nil, "", nil, false
}

// ParseGoalCheckResponse extracts the last balanced top-level JSON object from a
// goal-met judge's stdout and unmarshals it into {goal_met,reason}, reusing the
// same tolerant brace-depth scanner as ParseResponse/ParsePlanResponse
// (topLevelObjectSpans). It prefers the LAST span that both unmarshals cleanly
// AND carries a "goal_met" field, so an example object printed earlier in the
// prose — or an unrelated {} — does not masquerade as the verdict.
//
// ok is false when no object carrying goal_met is found; in that case goalMet is
// false and reason is empty. It never panics.
func ParseGoalCheckResponse(stdout string) (goalMet bool, reason string, ok bool) {
	spans := topLevelObjectSpans(stdout)

	for i := len(spans) - 1; i >= 0; i-- {
		var v struct {
			GoalMet *bool  `json:"goal_met"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(spans[i]), &v); err != nil {
			continue
		}
		// Skip objects that do not carry a goal_met field (e.g. a stray {} or an
		// unrelated JSON block) so they cannot be mistaken for a verdict. Using a
		// *bool distinguishes "absent" from an explicit false.
		if v.GoalMet == nil {
			continue
		}
		return *v.GoalMet, v.Reason, true
	}

	return false, "", false
}

// topLevelObjectSpans scans s and returns every complete balanced top-level
// {...} substring, in order of appearance. Brace depth is tracked so that only
// depth-0 objects are captured; braces that appear inside JSON string literals
// are ignored, with \" (and any other backslash escape) handled so an escaped
// quote does not terminate the string.
func topLevelObjectSpans(s string) []string {
	var spans []string

	depth := 0
	start := -1
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]

		if inString {
			switch {
			case escaped:
				// Previous byte was a backslash; this byte is consumed as the
				// escaped character (covers \" \\ \/ etc.) and cannot end the
				// string or start a new escape.
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					spans = append(spans, s[start:i+1])
					start = -1
				}
			}
		}
	}

	return spans
}
