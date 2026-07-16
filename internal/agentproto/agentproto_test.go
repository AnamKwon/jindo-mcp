package agentproto

import (
	"strings"
	"testing"
)

func TestBuildSystemPrompt(t *testing.T) {
	dir := "/var/jindo/memory-abc"
	p := BuildSystemPrompt(dir)

	// (a) memory-read instruction that names the bounded dir.
	if !strings.Contains(p, dir) {
		t.Errorf("prompt does not mention memory dir %q:\n%s", dir, p)
	}
	if !strings.Contains(strings.ToLower(p), "memory") {
		t.Errorf("prompt lacks a memory-read instruction:\n%s", p)
	}

	// (b) digest guidance: the agent must consult both live entries and the
	// folded "_digest" summary, not just one or the other.
	if !strings.Contains(p, "_digest") {
		t.Errorf("prompt does not mention the reserved %q entry:\n%s", "_digest", p)
	}
	if !strings.Contains(p, "both the live recent entries and") {
		t.Errorf("prompt does not instruct the agent to check both live entries and _digest:\n%s", p)
	}

	// (c) literal description of the JSON contract keys.
	for _, key := range []string{"status", "result", "summary", "memory_updates", "memory_used"} {
		if !strings.Contains(p, key) {
			t.Errorf("prompt does not mention contract key %q:\n%s", key, p)
		}
	}

	// (d) STEP 2 guidance: state assumptions, prefer minimal diff, verify own
	// work, and note what was left out of scope.
	for _, phrase := range []string{"assumption", "smallest correct change", "verify your own work", "left out of scope"} {
		if !strings.Contains(p, phrase) {
			t.Errorf("prompt lacks STEP 2 guidance phrase %q:\n%s", phrase, p)
		}
	}
}

func TestParseResponse_MemoryUsedRoundTrips(t *testing.T) {
	stdout := `{
  "status": "ok",
  "result": "edited foo.go",
  "summary": "fixed the bug",
  "memory_updates": [],
  "memory_used": ["foo.owner", "_digest"]
}`

	resp := ParseResponse(stdout)

	if len(resp.MemoryUsed) != 2 || resp.MemoryUsed[0] != "foo.owner" || resp.MemoryUsed[1] != "_digest" {
		t.Errorf("MemoryUsed = %v, want [foo.owner _digest]", resp.MemoryUsed)
	}
}

func TestParseResponse_MemoryUsedOmittedIsEmpty(t *testing.T) {
	stdout := `{"status": "ok", "result": "r", "summary": "s", "memory_updates": []}`

	resp := ParseResponse(stdout)

	if len(resp.MemoryUsed) != 0 {
		t.Errorf("MemoryUsed = %v, want empty", resp.MemoryUsed)
	}
}

func TestParseResponse_ProseThenValidJSON(t *testing.T) {
	stdout := `Working on the task now.
I inspected the code and made the change.

{
  "status": "ok",
  "result": "edited foo.go",
  "summary": "fixed the bug",
  "memory_updates": [
    {"key": "foo.owner", "value": "team-a"},
    {"note": "watch out for the retry path"}
  ]
}`

	resp := ParseResponse(stdout)

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q", resp.Status, "ok")
	}
	if resp.Result != "edited foo.go" {
		t.Errorf("Result = %q, want %q", resp.Result, "edited foo.go")
	}
	if resp.Summary != "fixed the bug" {
		t.Errorf("Summary = %q, want %q", resp.Summary, "fixed the bug")
	}
	if len(resp.MemoryUpdates) != 2 {
		t.Fatalf("len(MemoryUpdates) = %d, want 2", len(resp.MemoryUpdates))
	}
	if resp.MemoryUpdates[0].Key != "foo.owner" {
		t.Errorf("MemoryUpdates[0].Key = %q, want %q", resp.MemoryUpdates[0].Key, "foo.owner")
	}
	if v, ok := resp.MemoryUpdates[0].Value.(string); !ok || v != "team-a" {
		t.Errorf("MemoryUpdates[0].Value = %v, want %q", resp.MemoryUpdates[0].Value, "team-a")
	}
	if resp.MemoryUpdates[1].Note != "watch out for the retry path" {
		t.Errorf("MemoryUpdates[1].Note = %q, want %q", resp.MemoryUpdates[1].Note, "watch out for the retry path")
	}
}

func TestParseResponse_LastTopLevelObjectWins(t *testing.T) {
	stdout := `Here is an EXAMPLE of the format you should emit:
{"status": "example", "result": "not real", "summary": "ignore me", "memory_updates": []}

Now the actual result:
{"status": "ok", "result": "real result", "summary": "done", "memory_updates": []}`

	resp := ParseResponse(stdout)

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q (last object should win)", resp.Status, "ok")
	}
	if resp.Result != "real result" {
		t.Errorf("Result = %q, want %q", resp.Result, "real result")
	}
	if resp.Summary != "done" {
		t.Errorf("Summary = %q, want %q", resp.Summary, "done")
	}
}

func TestParseResponse_BracesInsideStrings(t *testing.T) {
	// The result value contains braces and an escaped quote; neither should
	// throw off the brace-depth scan.
	stdout := `prose here
{"status": "ok", "result": "use {this} and say \"hi\" {nested {deep}}", "summary": "s", "memory_updates": []}`

	resp := ParseResponse(stdout)

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q", resp.Status, "ok")
	}
	want := `use {this} and say "hi" {nested {deep}}`
	if resp.Result != want {
		t.Errorf("Result = %q, want %q", resp.Result, want)
	}
	if resp.Summary != "s" {
		t.Errorf("Summary = %q, want %q", resp.Summary, "s")
	}
}

func TestParseResponse_MalformedTrailingJSON(t *testing.T) {
	stdout := `did some work
{"status": "ok", "result": "half a bloc`

	resp := ParseResponse(stdout)

	if resp.Status != "unparsed" {
		t.Errorf("Status = %q, want %q", resp.Status, "unparsed")
	}
	if resp.Result != stdout {
		t.Errorf("Result = %q, want raw stdout %q", resp.Result, stdout)
	}
}

func TestParseResponse_EmptyStdout(t *testing.T) {
	resp := ParseResponse("")

	if resp.Status != "unparsed" {
		t.Errorf("Status = %q, want %q", resp.Status, "unparsed")
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty", resp.Result)
	}
}

// A supplied non-empty status must never be overwritten (e.g. "error").
func TestParseResponse_SuppliedStatusPreserved(t *testing.T) {
	stdout := `{"status": "error", "result": "boom", "summary": "failed", "memory_updates": []}`

	resp := ParseResponse(stdout)

	if resp.Status != "error" {
		t.Errorf("Status = %q, want %q (supplied status preserved)", resp.Status, "error")
	}
}

// An empty supplied status is defaulted to "ok".
func TestParseResponse_EmptyStatusDefaultsToOK(t *testing.T) {
	stdout := `{"result": "r", "summary": "s", "memory_updates": []}`

	resp := ParseResponse(stdout)

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q (empty status defaults to ok)", resp.Status, "ok")
	}
}

func TestBuildReviewPrompt(t *testing.T) {
	dir := "/var/jindo/memory-xyz"
	authorTask := "refactor the scheduler to remove the data race"
	authorResult := "moved the lock acquisition above the map write"
	changedFile := "internal/scheduler/scheduler.go"
	p := BuildReviewPrompt(dir, authorTask, authorResult, ReviewArtifacts{ChangedFiles: []string{changedFile}})

	// (a) names the bounded memory dir it must read.
	if !strings.Contains(p, dir) {
		t.Errorf("review prompt does not mention memory dir %q:\n%s", dir, p)
	}
	// (b) embeds the author's task and result so the reviewer can judge them.
	if !strings.Contains(p, authorTask) {
		t.Errorf("review prompt does not embed the author task:\n%s", p)
	}
	if !strings.Contains(p, authorResult) {
		t.Errorf("review prompt does not embed the author result:\n%s", p)
	}
	// (c) literal description of the review contract keys and the enum vocab.
	for _, key := range []string{"verdict", "findings", "severity", "title", "message", "summary"} {
		if !strings.Contains(p, key) {
			t.Errorf("review prompt does not mention contract key %q:\n%s", key, p)
		}
	}
	for _, v := range []string{"approved", "changes_requested", "rejected", "critical"} {
		if !strings.Contains(p, v) {
			t.Errorf("review prompt does not mention vocabulary %q:\n%s", v, p)
		}
	}
	// (d) sharpened correctness guidance and an explicit security checklist.
	for _, phrase := range []string{"invariant", "SECURITY", "injection", "secret"} {
		if !strings.Contains(p, phrase) {
			t.Errorf("review prompt does not mention %q:\n%s", phrase, p)
		}
	}
	// (e) real artifacts: the passed changed-file path appears, and the reviewer
	// is told to inspect the actually-changed files rather than the self-report.
	if !strings.Contains(p, changedFile) {
		t.Errorf("review prompt does not list the changed file %q:\n%s", changedFile, p)
	}
	if !strings.Contains(p, "CHANGED FILES") || !strings.Contains(p, "inspect") {
		t.Errorf("review prompt does not instruct the reviewer to inspect the changed files:\n%s", p)
	}

	// When no artifacts are supplied the prompt behaves as before: no artifact
	// sections leak into the output.
	bare := BuildReviewPrompt(dir, authorTask, authorResult, ReviewArtifacts{})
	if strings.Contains(bare, "CHANGED FILES") || strings.Contains(bare, "EXECUTION / VERIFY OUTPUT") {
		t.Errorf("empty ReviewArtifacts should add no artifact sections:\n%s", bare)
	}
}

func TestParseReviewResponse_ProseThenValidJSON(t *testing.T) {
	stdout := `I read the shared memory and reviewed the author's work.
The change looks incomplete.

{"verdict":"changes_requested","findings":[{"severity":"critical","title":"race remains","message":"the map write is still unguarded"}],"summary":"one critical issue"}`

	rev := ParseReviewResponse(stdout)

	if rev.Verdict != "changes_requested" {
		t.Errorf("Verdict = %q, want %q", rev.Verdict, "changes_requested")
	}
	if len(rev.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(rev.Findings))
	}
	if rev.Findings[0].Severity != "critical" {
		t.Errorf("Findings[0].Severity = %q, want %q", rev.Findings[0].Severity, "critical")
	}
	if rev.Summary != "one critical issue" {
		t.Errorf("Summary = %q, want %q", rev.Summary, "one critical issue")
	}
}

// A JSON review block preceded by an EXAMPLE object (and an unrelated {}) must
// still resolve to the last review-shaped block.
func TestParseReviewResponse_LastReviewShapedWins(t *testing.T) {
	stdout := `Here is the format: {"note":"not a review"}
Example: {"verdict":"approved","findings":[],"summary":"example only"}
Actual review:
{"verdict":"rejected","findings":[{"severity":"critical"}],"summary":"real"}`

	rev := ParseReviewResponse(stdout)

	if rev.Verdict != "rejected" {
		t.Errorf("Verdict = %q, want %q (last review-shaped block wins)", rev.Verdict, "rejected")
	}
	if rev.Summary != "real" {
		t.Errorf("Summary = %q, want %q", rev.Summary, "real")
	}
}

func TestParseReviewResponse_GarbageFallback(t *testing.T) {
	// No JSON object at all, and an object with no review shape, both fall back.
	for _, stdout := range []string{
		"the reviewer crashed and printed only this line of prose",
		`{"unrelated":"object","with":"no verdict or findings"}`,
		"",
	} {
		rev := ParseReviewResponse(stdout)
		if rev.Verdict != VerdictUnparsed {
			t.Errorf("ParseReviewResponse(%q).Verdict = %q, want %q", stdout, rev.Verdict, VerdictUnparsed)
		}
	}
}

func TestHasCritical(t *testing.T) {
	if HasCritical(nil) {
		t.Errorf("HasCritical(nil) = true, want false")
	}
	if HasCritical([]ReviewFinding{{Severity: "major"}, {Severity: "info"}}) {
		t.Errorf("HasCritical(no critical) = true, want false")
	}
	if !HasCritical([]ReviewFinding{{Severity: "minor"}, {Severity: "critical"}}) {
		t.Errorf("HasCritical(has critical) = false, want true")
	}
}

func TestBuildPlanPrompt(t *testing.T) {
	const goal = "add a plan MCP tool that decomposes a goal into steps"
	dir := "/var/jindo/memory-xyz"
	p := BuildPlanPrompt(goal, dir)

	// (a) the goal to plan is embedded verbatim.
	if !strings.Contains(p, goal) {
		t.Errorf("prompt does not embed the goal %q:\n%s", goal, p)
	}
	// (b) it names the bounded memory dir (read-shared-memory instruction).
	if !strings.Contains(p, dir) {
		t.Errorf("prompt does not mention memory dir %q:\n%s", dir, p)
	}
	// (c) it must forbid doing the work / editing files (plan-only).
	if !strings.Contains(strings.ToLower(p), "not") || !strings.Contains(strings.ToLower(p), "edit") {
		t.Errorf("prompt does not instruct the planner to avoid editing files:\n%s", p)
	}
	// (d) literal description of the plan contract keys.
	for _, key := range []string{"steps", "summary", "id", "title", "prompt", "difficulty", "suggested_model", "suggested_verify", "depends_on"} {
		if !strings.Contains(p, key) {
			t.Errorf("prompt does not mention contract key %q:\n%s", key, p)
		}
	}
	// (e) it demands a real, runnable verify command per step.
	if !strings.Contains(p, "verify") {
		t.Errorf("prompt does not require verifiable steps:\n%s", p)
	}
	// (f) it flags security-sensitive steps for extra review.
	if !strings.Contains(p, "SECURITY") {
		t.Errorf("prompt does not flag security-sensitive steps:\n%s", p)
	}
}

func TestBuildProposePrompt(t *testing.T) {
	const task = "implement a thread-safe LRU cache"
	dir := "/var/jindo/memory-xyz"
	p := BuildProposePrompt(dir, task)

	// (a) the task to solve is embedded verbatim.
	if !strings.Contains(p, task) {
		t.Errorf("prompt does not embed the task %q:\n%s", task, p)
	}
	// (b) it names the bounded memory dir (read-shared-memory instruction).
	if !strings.Contains(p, dir) {
		t.Errorf("prompt does not mention memory dir %q:\n%s", dir, p)
	}
	// (c) it must instruct NOT to write/create/edit files (read-only propose).
	low := strings.ToLower(p)
	if !strings.Contains(low, "not") || !strings.Contains(low, "write") {
		t.Errorf("prompt does not instruct the agent to avoid writing files:\n%s", p)
	}
	// (d) it ends with the SAME response contract keys ParseResponse reads.
	for _, key := range []string{"status", "result", "summary", "memory_updates"} {
		if !strings.Contains(p, key) {
			t.Errorf("prompt does not mention contract key %q:\n%s", key, p)
		}
	}
}

func TestBuildJudgePrompt(t *testing.T) {
	const task = "implement a thread-safe LRU cache"
	dir := "/var/jindo/memory-xyz"
	candidates := []string{"candidate-alpha-solution", "candidate-beta-solution", "candidate-gamma-solution"}
	p := BuildJudgePrompt(dir, task, candidates)

	// (a) the task is embedded verbatim.
	if !strings.Contains(p, task) {
		t.Errorf("prompt does not embed the task %q:\n%s", task, p)
	}
	// (b) it names the bounded memory dir.
	if !strings.Contains(p, dir) {
		t.Errorf("prompt does not mention memory dir %q:\n%s", dir, p)
	}
	// (c) EVERY candidate solution is embedded (delimited/numbered).
	for i, c := range candidates {
		if !strings.Contains(p, c) {
			t.Errorf("prompt does not embed candidate %d %q:\n%s", i+1, c, p)
		}
	}
	// (d) same response contract keys ParseResponse reads.
	for _, key := range []string{"status", "result", "summary", "memory_updates"} {
		if !strings.Contains(p, key) {
			t.Errorf("prompt does not mention contract key %q:\n%s", key, p)
		}
	}
}

func TestMemoryReadingPromptsForbidMCPRecursion(t *testing.T) {
	dir := "/var/jindo/memory-xyz"
	prompts := map[string]string{
		"author":     BuildSystemPrompt(dir),
		"reviewer":   BuildReviewPrompt(dir, "task", "result", ReviewArtifacts{}),
		"planner":    BuildPlanPrompt("goal", dir),
		"candidate":  BuildProposePrompt(dir, "task"),
		"judge":      BuildJudgePrompt(dir, "task", []string{"candidate"}),
		"goal-check": BuildGoalCheckPrompt(dir, "goal", "spec", "tests passed"),
	}
	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(prompt, "filesystem access") {
				t.Fatalf("prompt does not require direct filesystem memory access:\n%s", prompt)
			}
			if !strings.Contains(prompt, "Do not call or invoke any MCP") {
				t.Fatalf("prompt does not forbid recursive MCP memory access:\n%s", prompt)
			}
		})
	}
}

func TestParsePlanResponse_ProseThenJSON(t *testing.T) {
	stdout := `I read the shared memory and decomposed the goal.

Here is an EXAMPLE shape you should emit (ignore this one):
{"steps": [], "summary": "example only"}

Now the actual plan:
{
  "steps": [
    {"id": "s1", "title": "add PlanStep", "difficulty": "standard", "suggested_model": "claude-sonnet-5", "suggested_verify": ["go build ./..."], "depends_on": []},
    {"id": "s2", "title": "wire the tool", "difficulty": "hard", "suggested_model": "claude-opus-4-8", "suggested_verify": ["go test ./..."], "depends_on": ["s1"]}
  ],
  "summary": "two-step plan"
}`

	steps, summary, _, ok := ParsePlanResponse(stdout)
	if !ok {
		t.Fatalf("ParsePlanResponse ok = false, want true")
	}
	if len(steps) != 2 {
		t.Fatalf("len(steps) = %d, want 2", len(steps))
	}
	if steps[0].ID != "s1" || steps[0].Difficulty != "standard" || steps[0].SuggestedModel != "claude-sonnet-5" {
		t.Errorf("steps[0] = %+v, want id=s1 difficulty=standard model=claude-sonnet-5", steps[0])
	}
	if len(steps[0].SuggestedVerify) != 1 || steps[0].SuggestedVerify[0] != "go build ./..." {
		t.Errorf("steps[0].SuggestedVerify = %v, want [go build ./...]", steps[0].SuggestedVerify)
	}
	if len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "s1" {
		t.Errorf("steps[1].DependsOn = %v, want [s1]", steps[1].DependsOn)
	}
	if summary != "two-step plan" {
		t.Errorf("summary = %q, want %q", summary, "two-step plan")
	}
}

func TestParsePlanResponse_Garbage(t *testing.T) {
	for _, stdout := range []string{
		"no json here at all",
		"",
		`{"summary": "no steps array"}`,
		`{"steps": [], "summary": "empty steps"}`,
	} {
		steps, summary, _, ok := ParsePlanResponse(stdout)
		if ok {
			t.Errorf("ParsePlanResponse(%q) ok = true, want false", stdout)
		}
		if steps != nil || summary != "" {
			t.Errorf("ParsePlanResponse(%q) = (%v, %q), want (nil, \"\")", stdout, steps, summary)
		}
	}
}

func TestParseGoalCheckResponse_ProseThenJSON(t *testing.T) {
	stdout := `I read shared memory and inspected the repository.

Here is the SHAPE you should emit (ignore this example):
{"goal_met": true, "reason": "example only"}

Verdict:
{"goal_met": false, "reason": "the CLI flag is added but has no test"}`

	goalMet, reason, ok := ParseGoalCheckResponse(stdout)
	if !ok {
		t.Fatalf("ParseGoalCheckResponse ok = false, want true")
	}
	// The LAST goal_met-bearing object wins over the earlier example.
	if goalMet {
		t.Errorf("goalMet = true, want false")
	}
	if reason != "the CLI flag is added but has no test" {
		t.Errorf("reason = %q, want the last verdict's reason", reason)
	}
}

func TestParseGoalCheckResponse_ExplicitTrue(t *testing.T) {
	goalMet, reason, ok := ParseGoalCheckResponse(`{"goal_met": true, "reason": "done"}`)
	if !ok || !goalMet || reason != "done" {
		t.Errorf("ParseGoalCheckResponse = (%v, %q, %v), want (true, \"done\", true)", goalMet, reason, ok)
	}
}

func TestParseGoalCheckResponse_Garbage(t *testing.T) {
	for _, stdout := range []string{
		"no json here at all",
		"",
		`{"reason": "no goal_met field"}`,
		`{}`,
	} {
		goalMet, reason, ok := ParseGoalCheckResponse(stdout)
		if ok {
			t.Errorf("ParseGoalCheckResponse(%q) ok = true, want false", stdout)
		}
		if goalMet || reason != "" {
			t.Errorf("ParseGoalCheckResponse(%q) = (%v, %q), want (false, \"\")", stdout, goalMet, reason)
		}
	}
}
