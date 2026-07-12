package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"jindo/internal/agent"
	"jindo/internal/memory"
	"jindo/internal/orchestrator"
	"jindo/internal/plan"
	"jindo/internal/routing"
	"jindo/internal/tmux"
)

// newTestServer builds a Server over an orchestrator whose collaborators are
// all deterministic: a tmux seam that answers has-session with rc=0 (session
// present, so no creation subprocesses) and records nothing else meaningful, a
// GetAdapter that returns the real adapter with its Exec overridden to a canned
// string (no subprocess, no LLM), and a real SharedMemory rooted in a temp dir.
// Routing is left as the production routing.Select.
func newTestServer(t *testing.T, canned string) (*Server, *memory.SharedMemory) {
	t.Helper()
	mem := memory.New(t.TempDir())

	// Recognize all three agents so whichever tier routing picks can be
	// dispatched; rc=0 => Exists() true => EnsureSession() is a no-op.
	mgr := tmux.New("jindo", []string{"claude", "codex", "agy"})
	mgr.Tmux = func(args ...string) (string, int, error) {
		if len(args) > 0 && args[0] == "has-session" {
			return "", 0, nil
		}
		return "", 0, nil
	}

	o := orchestrator.New(mem, mgr)
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			return nil, err
		}
		ad.Exec = func(argv []string) (string, error) { return canned, nil }
		return ad, nil
	}
	return NewServer(o), mem
}

// call sends one request line through HandleLine and decodes the response.
func call(t *testing.T, s *Server, line string) Response {
	t.Helper()
	raw := s.HandleLine([]byte(line))
	if raw == nil {
		t.Fatalf("HandleLine(%q) returned nil (unexpected notification)", line)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("response not valid JSON: %v (raw=%s)", err, raw)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0 (raw=%s)", resp.JSONRPC, raw)
	}
	return resp
}

// resultMap decodes a success response's result into a generic map, failing if
// the response carried an error instead.
func resultMap(t *testing.T, resp Response) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result not an object: %v (result=%s)", err, b)
	}
	return m
}

// contentText extracts the single text content block from an MCP tool result.
func contentText(t *testing.T, resp Response) string {
	t.Helper()
	m := resultMap(t, resp)
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result.content empty or wrong type: %v", m["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not an object: %v", content[0])
	}
	text, ok := block["text"].(string)
	if !ok {
		t.Fatalf("content[0].text not a string: %v", block["text"])
	}
	return text
}

func TestInitialize(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	m := resultMap(t, resp)

	info, ok := m["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo missing/wrong type: %v", m["serverInfo"])
	}
	if info["name"] != "jindo-mcp" {
		t.Fatalf("serverInfo.name = %v, want jindo-mcp", info["name"])
	}
}

func TestToolsList(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m := resultMap(t, resp)

	toolsRaw, ok := m["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing/wrong type: %v", m["tools"])
	}
	if len(toolsRaw) != 16 {
		t.Fatalf("tools length = %d, want 16", len(toolsRaw))
	}
	got := make([]string, 0, 9)
	for _, tr := range toolsRaw {
		td, ok := tr.(map[string]any)
		if !ok {
			t.Fatalf("tool entry not an object: %v", tr)
		}
		name, _ := td["name"].(string)
		got = append(got, name)
		if td["inputSchema"] == nil {
			t.Fatalf("tool %q missing inputSchema", name)
		}
	}
	sort.Strings(got)
	want := []string{"agents", "calibrate", "compact", "dispatch", "dispatch_async", "dispatch_multi", "dispatch_multi_async", "job_status", "memory", "models_refresh", "plan", "plan_gate", "plan_next", "plan_record", "plan_revise", "plan_status"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
}

func TestToolsCallDispatch(t *testing.T) {
	s, mem := newTestServer(t, "canned-dispatch-result")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment"}}}`)

	// A well-formed tool result with non-empty content and no error.
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	text := contentText(t, resp)
	if text == "" {
		t.Fatalf("dispatch content text empty")
	}
	if !strings.Contains(text, "canned-dispatch-result") {
		t.Fatalf("dispatch content missing canned result: %s", text)
	}

	// The orchestrator actually ran: the record is persisted under the
	// agent-partitioned dispatch key reported in the result, carrying the result.
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("dispatch content not JSON: %v (text=%s)", err, text)
	}
	if res.Key == "" {
		t.Fatalf("dispatch content missing key: %s", text)
	}
	stored, ok := mem.Read(res.Key)
	if !ok {
		t.Fatalf("memory has no %s after dispatch", res.Key)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map", res.Key, stored)
	}
	if entry["result"] != "canned-dispatch-result" {
		t.Fatalf("%s result = %v, want canned-dispatch-result", res.Key, entry["result"])
	}
}

// initGitRepo creates a temp git repository with one commit and returns its path.
// Used by the isolate-default-on dispatch test so DispatchAuto's git guard finds a
// real repo to branch off.
func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	return repo
}

// TestToolsCallDispatchIsolateDefaultsOn proves the MCP-level default: omitting
// the isolate argument routes a write dispatch through isolation for a git
// workdir (isolation payload present, base tree untouched), while isolate:false
// forces the legacy in-place path (no isolation payload). The canned adapter
// writes no files, so the isolated run takes the no-change branch and discards
// its worktree — enough to prove routing without depending on real file writes.
func TestToolsCallDispatchIsolateDefaultsOn(t *testing.T) {
	repo := initGitRepo(t)
	gitStatus := func() string {
		c := exec.Command("git", "-C", repo, "status", "--porcelain")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git status: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("omitted isolate routes to isolation for a git workdir", func(t *testing.T) {
		s, _ := newTestServer(t, "canned")
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","workdir":"`+repo+`"}}}`)
		if resp.Error != nil {
			t.Fatalf("dispatch returned error: %+v", resp.Error)
		}
		text := contentText(t, resp)
		var res struct {
			Isolation *orchestrator.Isolation `json:"isolation"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatalf("dispatch content not JSON: %v (text=%s)", err, text)
		}
		if res.Isolation == nil {
			t.Fatalf("expected an isolation payload when isolate defaults on for a git workdir; got: %s", text)
		}
		if res.Isolation.Skipped {
			t.Fatalf("isolation must not be skipped for a real git repo; got %+v", res.Isolation)
		}
		// The caller's base tree was never written by the isolated run.
		if _, err := os.Stat(filepath.Join(repo, "marker.go")); !os.IsNotExist(err) {
			t.Fatalf("base tree must NOT contain marker.go (err=%v)", err)
		}
		if st := gitStatus(); st != "" {
			t.Fatalf("base tree must be clean after isolation, got:\n%s", st)
		}
	})

	t.Run("isolate:false runs in place with no isolation payload", func(t *testing.T) {
		s, _ := newTestServer(t, "canned")
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","workdir":"`+repo+`","isolate":false}}}`)
		if resp.Error != nil {
			t.Fatalf("dispatch returned error: %+v", resp.Error)
		}
		text := contentText(t, resp)
		if strings.Contains(text, `"isolation"`) {
			t.Fatalf("isolate:false must not surface an isolation payload; got: %s", text)
		}
	})
}

// TestToolsCallDispatchStatusSummary verifies that when the adapter returns
// contract-following JSON stdout (status/result/summary/memory_updates), the
// dispatch tool's content surfaces status and summary alongside the existing
// agent/model/difficulty/result/key fields, per agentproto.Response ->
// orchestrator.Result -> callDispatch's returned map.
func TestToolsCallDispatchStatusSummary(t *testing.T) {
	canned := `{"status":"ok","result":"did the thing","summary":"a short summary","memory_updates":[]}`
	s, _ := newTestServer(t, canned)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment"}}}`)

	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	text := contentText(t, resp)

	var res struct {
		Agent      string `json:"agent"`
		Model      string `json:"model"`
		Difficulty string `json:"difficulty"`
		Result     string `json:"result"`
		Key        string `json:"key"`
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		Rationale  struct {
			Total     float64 `json:"total"`
			Tier      string  `json:"tier"`
			Threshold string  `json:"threshold"`
		} `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("dispatch content not JSON: %v (text=%s)", err, text)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok (text=%s)", res.Status, text)
	}
	if res.Summary != "a short summary" {
		t.Fatalf("summary = %q, want %q (text=%s)", res.Summary, "a short summary", text)
	}
	if res.Agent == "" || res.Model == "" || res.Difficulty == "" || res.Result == "" || res.Key == "" {
		t.Fatalf("dispatch content missing existing fields: %s", text)
	}
	// The routing rationale is exposed alongside the decision, with its tier
	// matching the reported difficulty.
	if res.Rationale.Tier != res.Difficulty {
		t.Fatalf("rationale tier = %q, want difficulty %q (text=%s)", res.Rationale.Tier, res.Difficulty, text)
	}
}

// TestToolsCallDispatchPriority verifies the optional "priority" argument
// decodes and flows through to routing: it must be echoed in the returned
// rationale, and (using the standard scope+constraints task where the
// best-coverage agent is not the cheapest, per
// routing.TestSelectPriorityCostFlipsChoice) priority=cost must resolve to a
// different agent than the default/no-priority call.
func TestToolsCallDispatchPriority(t *testing.T) {
	task := "implement a new api endpoint with validation"

	sDefault, _ := newTestServer(t, "canned")
	respDefault := call(t, sDefault,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"`+task+`"}}}`)
	if respDefault.Error != nil {
		t.Fatalf("dispatch (no priority) returned error: %+v", respDefault.Error)
	}
	var defaultRes struct {
		Agent     string `json:"agent"`
		Rationale struct {
			Priority string `json:"priority"`
		} `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(contentText(t, respDefault)), &defaultRes); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if defaultRes.Rationale.Priority != "" {
		t.Fatalf("rationale.priority = %q, want empty when no priority supplied", defaultRes.Rationale.Priority)
	}

	sCost, _ := newTestServer(t, "canned")
	respCost := call(t, sCost,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"`+task+`","priority":"cost"}}}`)
	if respCost.Error != nil {
		t.Fatalf("dispatch (priority=cost) returned error: %+v", respCost.Error)
	}
	var costRes struct {
		Agent     string `json:"agent"`
		Rationale struct {
			Priority string `json:"priority"`
		} `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(contentText(t, respCost)), &costRes); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if costRes.Rationale.Priority != "cost" {
		t.Fatalf("rationale.priority = %q, want cost", costRes.Rationale.Priority)
	}
	if costRes.Agent == defaultRes.Agent {
		t.Fatalf("priority=cost agent = %q, want different from no-priority agent %q", costRes.Agent, defaultRes.Agent)
	}
}

func TestToolsCallAgents(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"agents"}}`)
	text := contentText(t, resp)
	for _, name := range []string{"claude", "codex", "agy"} {
		if !strings.Contains(text, name) {
			t.Fatalf("agents content missing %q: %s", name, text)
		}
	}
	// The result carries BOTH the routing table and a per-agent install
	// availability map. With the seam unset (as in tests) every agent is
	// available, so decode the payload and assert the "available" map is present
	// and reports true for each agent.
	var payload struct {
		Agents    map[string]map[string]string `json:"agents"`
		Available map[string]bool              `json:"available"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode agents result: %v (text=%s)", err, text)
	}
	if len(payload.Agents) == 0 {
		t.Fatalf("agents result missing the routing table: %s", text)
	}
	if len(payload.Available) == 0 {
		t.Fatalf("agents result missing the availability map: %s", text)
	}
	for _, name := range []string{"claude", "codex", "agy"} {
		if !payload.Available[name] {
			t.Fatalf("available[%q] = false, want true (seam unset): %s", name, text)
		}
	}
}

func TestMalformedJSONLine(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{not json`)
	if resp.Error == nil {
		t.Fatalf("malformed line: expected error object, got %+v", resp)
	}
	if resp.Error.Code != codeParseError {
		t.Fatalf("malformed line error code = %d, want %d", resp.Error.Code, codeParseError)
	}
}

func TestUnknownMethod(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":5,"method":"no/such/method"}`)
	if resp.Error == nil {
		t.Fatalf("unknown method: expected error, got %+v", resp)
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method error code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
}

func TestToolsCallCompact(t *testing.T) {
	s, mem := newTestServer(t, "canned")

	// Seed a few entries so compact has real work to do; empty store must also not
	// error, but seeding lets us observe EntriesBefore > 0 in at least one sub-test.
	mem.Write("k1", map[string]any{"task": "t1"}, "test")
	mem.Write("k2", map[string]any{"task": "t2"}, "test")
	mem.Write("k3", map[string]any{"task": "t3"}, "test")

	t.Run("with empty args", func(t *testing.T) {
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"compact","arguments":{}}}`)
		if resp.Error != nil {
			t.Fatalf("compact returned error: %+v", resp.Error)
		}
		m := resultMap(t, resp)
		if isErr, _ := m["isError"].(bool); isErr {
			t.Fatalf("compact result has isError=true")
		}
		text := contentText(t, resp)
		if text == "" {
			t.Fatalf("compact content text empty")
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatalf("compact content not JSON: %v (text=%s)", err, text)
		}
		for _, field := range []string{"entries_before", "entries_after", "notes_before", "notes_after", "folded", "digested"} {
			if _, ok := result[field]; !ok {
				t.Fatalf("compact result missing field %q: %v", field, result)
			}
		}
	})

	t.Run("with max_entries and max_notes", func(t *testing.T) {
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"compact","arguments":{"max_entries":100,"max_notes":50}}}`)
		if resp.Error != nil {
			t.Fatalf("compact returned error: %+v", resp.Error)
		}
		text := contentText(t, resp)
		var result map[string]any
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatalf("compact content not JSON: %v (text=%s)", err, text)
		}
		if _, ok := result["entries_before"]; !ok {
			t.Fatalf("compact result missing entries_before: %v", result)
		}
	})

	t.Run("no arguments field", func(t *testing.T) {
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"compact"}}`)
		if resp.Error != nil {
			t.Fatalf("compact with no arguments returned error: %+v", resp.Error)
		}
	})
}

// TestToolsCallCalibrate writes a minimal dispatch.log to a temp file and
// checks the calibrate tool returns non-error, non-empty report text over it.
func TestToolsCallCalibrate(t *testing.T) {
	s, _ := newTestServer(t, "canned")

	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatch.log")
	line := `{"timestamp":"t1","key":"k1","task":"add auth","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o644); err != nil {
		t.Fatalf("write dispatch.log fixture: %v", err)
	}

	params, err := json.Marshal(map[string]any{
		"name":      "calibrate",
		"arguments": map[string]any{"path": logPath},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "tools/call",
		"params":  json.RawMessage(params),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp := call(t, s, string(req))
	if resp.Error != nil {
		t.Fatalf("calibrate returned error: %+v", resp.Error)
	}
	m := resultMap(t, resp)
	if isErr, _ := m["isError"].(bool); isErr {
		t.Fatalf("calibrate result has isError=true")
	}
	text := contentText(t, resp)
	if text == "" {
		t.Fatalf("calibrate content text empty")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("calibrate content not JSON: %v (text=%s)", err, text)
	}
	report, _ := result["report"].(string)
	if !strings.Contains(report, "parsed=1") {
		t.Fatalf("calibrate report missing parsed=1: %s", report)
	}
}

// TestToolsCallDispatchReviewSchema verifies tools/list advertises the optional
// "review" boolean argument on the dispatch tool's input schema.
func TestToolsCallDispatchReviewSchema(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":20,"method":"tools/list"}`)
	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)
	for _, tr := range toolsRaw {
		td, _ := tr.(map[string]any)
		if td["name"] != "dispatch" {
			continue
		}
		schema, _ := td["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		review, ok := props["review"].(map[string]any)
		if !ok {
			t.Fatalf("dispatch inputSchema missing review property: %v", schema)
		}
		if review["type"] != "boolean" {
			t.Fatalf("dispatch review property type = %v, want boolean", review["type"])
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if r == "review" {
				t.Fatalf("review must not be required: %v", required)
			}
		}
		return
	}
	t.Fatalf("dispatch tool not found in tools/list: %v", toolsRaw)
}

// TestToolsCallDispatchReviewOmittedKeepsLegacyPath verifies that without the
// review argument, dispatch behaves exactly as before: the persisted record
// carries no "review" key, since the legacy Dispatch (no-review) path never
// augments the authoritative record with a review entry (see
// orchestrator.finishReviewed, only reached when review runs).
func TestToolsCallDispatchReviewOmittedKeepsLegacyPath(t *testing.T) {
	s, mem := newTestServer(t, "canned-no-review")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment"}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &res); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	stored, ok := mem.Read(res.Key)
	if !ok {
		t.Fatalf("memory has no %s after dispatch", res.Key)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map", res.Key, stored)
	}
	if _, has := entry["review"]; has {
		t.Fatalf("%s unexpectedly has a review entry with review omitted: %v", res.Key, entry)
	}
}

// TestToolsCallDispatchReviewTrueRoutesToReviewPath verifies that review:true
// routes through orchestrator.DispatchWithReview: the persisted authoritative
// record gains a "review" entry (written only by finishReviewed, the
// review-pipeline's post-processing step — see orchestrator.go), which the
// legacy no-review path never produces. The stubbed adapter answers every
// agent (author and reviewer alike) with the same canned, non-JSON text, so
// the review is best-effort-unparseable; that still exercises the review path
// (a distinct code path from the legacy one) without requiring a real
// cross-model reviewer.
func TestToolsCallDispatchReviewTrueRoutesToReviewPath(t *testing.T) {
	s, mem := newTestServer(t, "canned-with-review")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","review":true}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var res struct {
		Key    string `json:"key"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &res); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	stored, ok := mem.Read(res.Key)
	if !ok {
		t.Fatalf("memory has no %s after dispatch", res.Key)
	}
	entry, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map", res.Key, stored)
	}
	if _, has := entry["review"]; !has {
		t.Fatalf("%s missing review entry with review:true: %v", res.Key, entry)
	}
}

// TestToolsCallDispatchReviewExposesVerdictInPayload pins the host-visibility fix:
// a review:true dispatch's RETURNED payload (not just memory) must carry a
// non-empty "reviews" array whose elements each carry a verdict, so the host can
// gate on what each reviewer found. review:false must omit the reviews key
// entirely (legacy payload shape).
func TestToolsCallDispatchReviewExposesVerdictInPayload(t *testing.T) {
	s, _ := newTestServer(t, "canned-with-review")

	// review:true -> payload has a reviews array whose elements carry a verdict.
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","review":true}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var withReview struct {
		Reviews []struct {
			Verdict       string `json:"verdict"`
			ReviewerAgent string `json:"reviewer_agent"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &withReview); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if len(withReview.Reviews) == 0 {
		t.Fatalf("review:true payload missing reviews array: %s", contentText(t, resp))
	}
	for _, r := range withReview.Reviews {
		if r.Verdict == "" {
			t.Fatalf("review:true payload reviews element has empty verdict: %s", contentText(t, resp))
		}
	}

	// review:false (omitted) -> no reviews key in payload
	respOff := call(t, s,
		`{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment"}}}`)
	var raw map[string]any
	if err := json.Unmarshal([]byte(contentText(t, respOff)), &raw); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if _, has := raw["reviews"]; has {
		t.Fatalf("review-off payload must omit reviews key: %v", raw)
	}
}

// TestToolsCallDispatchReviewStatusInPayload pins the EXPLICIT review-trust
// surface: a review:true dispatch's RETURNED payload must carry a "review_status"
// object {requested, completed, gate_passed, confidence} so the host cannot
// mistake review:true for a passed quality gate. With this fixture every reviewer
// returns an unparseable review (errored), so the review does NOT complete:
// completed=false, gate_passed=false, confidence="unverified". review:false must
// omit the review_status key entirely (legacy payload shape).
func TestToolsCallDispatchReviewStatusInPayload(t *testing.T) {
	s, _ := newTestServer(t, "canned-with-review")

	// review:true -> payload carries review_status with the trust flags.
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","review":true}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var withReview struct {
		ReviewStatus *struct {
			Requested  bool   `json:"requested"`
			Completed  bool   `json:"completed"`
			GatePassed bool   `json:"gate_passed"`
			Confidence string `json:"confidence"`
		} `json:"review_status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &withReview); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if withReview.ReviewStatus == nil {
		t.Fatalf("review:true payload missing review_status: %s", contentText(t, resp))
	}
	if !withReview.ReviewStatus.Requested {
		t.Fatalf("review_status.requested = false, want true")
	}
	if withReview.ReviewStatus.Completed || withReview.ReviewStatus.GatePassed {
		t.Fatalf("review_status = %+v, want completed=false gate_passed=false (reviewers errored)", *withReview.ReviewStatus)
	}
	if withReview.ReviewStatus.Confidence != "unverified" {
		t.Fatalf("review_status.confidence = %q, want unverified", withReview.ReviewStatus.Confidence)
	}

	// review:false (omitted) -> no review_status key in payload.
	respOff := call(t, s,
		`{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment"}}}`)
	var raw map[string]any
	if err := json.Unmarshal([]byte(contentText(t, respOff)), &raw); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if _, has := raw["review_status"]; has {
		t.Fatalf("review-off payload must omit review_status key: %v", raw)
	}
}

// TestToolsCallDispatchAsync verifies dispatch_async returns a job_id and
// status "running" immediately, without waiting for the background dispatch to
// finish.
func TestToolsCallDispatchAsync(t *testing.T) {
	s, _ := newTestServer(t, "canned-async")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"dispatch_async","arguments":{"task":"add a comment"}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch_async returned error: %+v", resp.Error)
	}
	var res struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &res); err != nil {
		t.Fatalf("dispatch_async content not JSON: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("dispatch_async missing job_id: %s", contentText(t, resp))
	}
	if res.Status != "running" {
		t.Fatalf("dispatch_async status = %q, want running", res.Status)
	}

	// Drain the background job before returning: t.TempDir()'s cleanup
	// races the goroutine's best-effort persist write otherwise (see
	// internal/jobs.Manager.Submit). Long-polling job_status until
	// terminal makes the wait deterministic without weakening the
	// immediate-running assertion above.
	statusReq := `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"job_status","arguments":{"job_id":"` + res.JobID + `","wait_sec":5}}}`
	statusResp := call(t, s, statusReq)
	if statusResp.Error != nil {
		t.Fatalf("job_status returned error: %+v", statusResp.Error)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, statusResp)), &status); err != nil {
		t.Fatalf("job_status content not JSON: %v", err)
	}
	if status.Status != "done" {
		t.Fatalf("job_status status = %q, want done", status.Status)
	}
}

// TestToolsCallJobStatusDone dispatches asynchronously and polls job_status
// with a long-poll wait, expecting the job to reach "done" with the full
// dispatch payload (mirroring the sync dispatch tool's fields).
func TestToolsCallJobStatusDone(t *testing.T) {
	s, _ := newTestServer(t, "canned-job-done")
	asyncResp := call(t, s,
		`{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"dispatch_async","arguments":{"task":"add a comment"}}}`)
	var async struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(contentText(t, asyncResp)), &async); err != nil {
		t.Fatalf("dispatch_async content not JSON: %v", err)
	}

	statusReq := `{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"job_status","arguments":{"job_id":"` + async.JobID + `","wait_sec":5}}}`
	statusResp := call(t, s, statusReq)
	if statusResp.Error != nil {
		t.Fatalf("job_status returned error: %+v", statusResp.Error)
	}
	var status struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(contentText(t, statusResp)), &status); err != nil {
		t.Fatalf("job_status content not JSON: %v", err)
	}
	if status.Status != "done" {
		t.Fatalf("job_status status = %q, want done (result=%v)", status.Status, status.Result)
	}
	if status.Result["result"] != "canned-job-done" {
		t.Fatalf("job_status result missing dispatch payload: %v", status.Result)
	}
	if status.Result["key"] == "" || status.Result["key"] == nil {
		t.Fatalf("job_status result missing key field: %v", status.Result)
	}
}

// TestToolsCallJobStatusError verifies a dispatch that errors (the
// sensitive-path policy gate refuses tasks referencing .env before any agent
// runs, per internal/policy) surfaces through job_status as status "error"
// with the failure message, not "done".
func TestToolsCallJobStatusError(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	asyncResp := call(t, s,
		`{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"dispatch_async","arguments":{"task":"edit .env"}}}`)
	var async struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(contentText(t, asyncResp)), &async); err != nil {
		t.Fatalf("dispatch_async content not JSON: %v", err)
	}

	statusReq := `{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"job_status","arguments":{"job_id":"` + async.JobID + `","wait_sec":5}}}`
	statusResp := call(t, s, statusReq)
	if statusResp.Error != nil {
		t.Fatalf("job_status returned error: %+v", statusResp.Error)
	}
	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(contentText(t, statusResp)), &status); err != nil {
		t.Fatalf("job_status content not JSON: %v", err)
	}
	if status.Status != "error" {
		t.Fatalf("job_status status = %q, want error", status.Status)
	}
	if status.Error == "" {
		t.Fatalf("job_status error message empty")
	}
}

// TestToolsCallJobStatusUnknownID verifies polling a job id that was never
// submitted (and has no persisted file) is reported as an invalid-params
// error, not a fabricated status.
func TestToolsCallJobStatusUnknownID(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"job_status","arguments":{"job_id":"does-not-exist"}}}`)
	if resp.Error == nil {
		t.Fatalf("job_status with unknown id: expected error, got %+v", resp)
	}
	if resp.Error.Code != codeInvalidParams {
		t.Fatalf("job_status unknown id error code = %d, want %d", resp.Error.Code, codeInvalidParams)
	}
}

// TestToolsCallDispatchModelPin verifies the dispatch tool honors an explicit
// model pin: with agent+model given (an unlisted model, so score-based routing
// could never pick it), the returned payload reports that exact model and the
// "explicit" difficulty, proving the model was routed verbatim through
// DispatchModel/routing.SelectModel.
func TestToolsCallDispatchModelPin(t *testing.T) {
	s, _ := newTestServer(t, "canned-model-pin")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","agent":"claude","model":"claude-opus-4-8-preview"}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var res struct {
		Agent      string `json:"agent"`
		Model      string `json:"model"`
		Difficulty string `json:"difficulty"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &res); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if res.Model != "claude-opus-4-8-preview" {
		t.Fatalf("dispatch routed model = %q, want claude-opus-4-8-preview (pinned)", res.Model)
	}
	if res.Agent != "claude" {
		t.Fatalf("dispatch agent = %q, want claude", res.Agent)
	}
	if res.Difficulty != "explicit" {
		t.Fatalf("dispatch difficulty = %q, want explicit (unlisted pinned model)", res.Difficulty)
	}
}

// TestToolsCallDispatchModelSchema verifies tools/list advertises the optional
// "model" string argument on both the dispatch and dispatch_async input schemas.
func TestToolsCallDispatchModelSchema(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":41,"method":"tools/list"}`)
	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)

	seen := map[string]bool{}
	for _, tr := range toolsRaw {
		td, _ := tr.(map[string]any)
		name, _ := td["name"].(string)
		if name != "dispatch" && name != "dispatch_async" {
			continue
		}
		schema, _ := td["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		model, ok := props["model"].(map[string]any)
		if !ok {
			t.Fatalf("%s inputSchema missing model property: %v", name, schema)
		}
		if model["type"] != "string" {
			t.Fatalf("%s model property type = %v, want string", name, model["type"])
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if r == "model" {
				t.Fatalf("%s: model must not be required: %v", name, required)
			}
		}
		seen[name] = true
	}
	if !seen["dispatch"] || !seen["dispatch_async"] {
		t.Fatalf("model property not found on both tools; saw %v", seen)
	}
}

// TestToolsCallDispatchGuidanceRoutesToSystemPrompt proves the dispatch tool's
// optional "guidance" argument reaches the author's system prompt (via claude's
// --append-system-prompt flag), exercising the full mcp -> orchestrator wiring
// rather than just the orchestrator-level unit test.
func TestToolsCallDispatchGuidanceRoutesToSystemPrompt(t *testing.T) {
	mem := memory.New(t.TempDir())
	mgr := tmux.New("jindo", []string{"claude", "codex", "agy"})
	mgr.Tmux = func(args ...string) (string, int, error) { return "", 0, nil }

	o := orchestrator.New(mem, mgr)
	var gotArgv []string
	o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			return nil, err
		}
		ad.Exec = func(argv []string) (string, error) {
			gotArgv = argv
			return `{"status":"ok","result":"did X","summary":"s"}`, nil
		}
		return ad, nil
	}
	s := NewServer(o)

	const guidance = "Follow idiomatic Python: type hints, docstrings, snake_case."
	resp := call(t, s, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"write a function","agent":"claude","guidance":"`+guidance+`"}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}

	idx := -1
	for i, a := range gotArgv {
		if a == "--append-system-prompt" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(gotArgv) {
		t.Fatalf("claude argv missing --append-system-prompt value: %v", gotArgv)
	}
	if !strings.Contains(gotArgv[idx+1], guidance) {
		t.Fatalf("system prompt missing guidance %q: %q", guidance, gotArgv[idx+1])
	}
}

// TestToolsCallDispatchGuidanceSchema verifies tools/list advertises the
// optional "guidance" string argument on both the dispatch and dispatch_async
// input schemas.
func TestToolsCallDispatchGuidanceSchema(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":43,"method":"tools/list"}`)
	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)

	seen := map[string]bool{}
	for _, tr := range toolsRaw {
		td, _ := tr.(map[string]any)
		name, _ := td["name"].(string)
		if name != "dispatch" && name != "dispatch_async" {
			continue
		}
		schema, _ := td["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		guidance, ok := props["guidance"].(map[string]any)
		if !ok {
			t.Fatalf("%s inputSchema missing guidance property: %v", name, schema)
		}
		if guidance["type"] != "string" {
			t.Fatalf("%s guidance property type = %v, want string", name, guidance["type"])
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if r == "guidance" {
				t.Fatalf("%s: guidance must not be required: %v", name, required)
			}
		}
		seen[name] = true
	}
	if !seen["dispatch"] || !seen["dispatch_async"] {
		t.Fatalf("guidance property not found on both tools; saw %v", seen)
	}
}

// flaggedCalibrateLog is a dispatch.log whose "standard" tier has 2 dispatches
// with a 1/2 non-ok rate (above failureRateThreshold, >= minSample), so
// DeriveOverrides produces a non-empty threshold override — letting the apply
// path exercise a real file write.
const flaggedCalibrateLog = `{"timestamp":"t1","key":"k1","task":"a","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"ok"}
{"timestamp":"t2","key":"k2","task":"b","agent":"claude","model":"m1","difficulty":"standard","rationale":{"matched":{"security":3},"total":3.0,"threshold":"standard","threshold_value":1.0,"tier":"standard"},"status":"error"}
`

// callCalibrateResult runs the calibrate tool with the given arguments through
// the transport and returns the decoded result payload (the JSON in the single
// text content block).
func callCalibrateResult(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": "calibrate", "arguments": args})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params":  json.RawMessage(params),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp := call(t, s, string(req))
	if resp.Error != nil {
		t.Fatalf("calibrate returned error: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(contentText(t, resp)), &result); err != nil {
		t.Fatalf("calibrate content not JSON: %v", err)
	}
	return result
}

// TestToolsCallCalibrateApplyFalseWritesNothing verifies the backward-compatible
// path: without apply, the calibrate tool returns the report and writes no
// overrides file, and its result carries none of the apply-only keys.
func TestToolsCallCalibrateApplyFalseWritesNothing(t *testing.T) {
	s, _ := newTestServer(t, "canned")

	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(logPath, []byte(flaggedCalibrateLog), 0o644); err != nil {
		t.Fatalf("write dispatch.log: %v", err)
	}
	overridesPath := filepath.Join(dir, "routing_overrides.json")

	result := callCalibrateResult(t, s, map[string]any{
		"path":           logPath,
		"apply":          false,
		"overrides_path": overridesPath,
	})

	if _, ok := result["report"].(string); !ok {
		t.Fatalf("result missing report string: %v", result)
	}
	for _, k := range []string{"applied", "overrides_path", "overrides", "apply_message"} {
		if _, ok := result[k]; ok {
			t.Errorf("apply=false result must not carry %q: %v", k, result)
		}
	}
	if _, err := os.Stat(overridesPath); !os.IsNotExist(err) {
		t.Fatalf("apply=false must not write overrides file; stat err=%v", err)
	}
}

// TestToolsCallCalibrateApplyTrueWritesAppliableOverrides verifies apply=true
// writes an overrides file that routing.ApplyOverrides accepts, and reports the
// path back to the caller.
func TestToolsCallCalibrateApplyTrueWritesAppliableOverrides(t *testing.T) {
	s, _ := newTestServer(t, "canned")

	// Restore live routing thresholds so applying the derived overrides here
	// does not leak mutated global state into other tests in this binary.
	orig := routing.Thresholds()
	t.Cleanup(func() {
		b, err := json.Marshal(map[string]any{"thresholds": orig})
		if err != nil {
			t.Fatalf("restore marshal: %v", err)
		}
		p := filepath.Join(t.TempDir(), "restore.json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("restore write: %v", err)
		}
		if err := routing.ApplyOverrides(p); err != nil {
			t.Fatalf("restore ApplyOverrides: %v", err)
		}
	})

	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatch.log")
	if err := os.WriteFile(logPath, []byte(flaggedCalibrateLog), 0o644); err != nil {
		t.Fatalf("write dispatch.log: %v", err)
	}
	overridesPath := filepath.Join(dir, "routing_overrides.json")

	result := callCalibrateResult(t, s, map[string]any{
		"path":           logPath,
		"apply":          true,
		"overrides_path": overridesPath,
	})

	if applied, _ := result["applied"].(bool); !applied {
		t.Fatalf("apply=true expected applied=true, got: %v", result)
	}
	if got, _ := result["overrides_path"].(string); got != overridesPath {
		t.Errorf("result overrides_path = %q, want %q", got, overridesPath)
	}
	if _, err := os.Stat(overridesPath); err != nil {
		t.Fatalf("apply=true must write overrides file: %v", err)
	}
	if err := routing.ApplyOverrides(overridesPath); err != nil {
		t.Fatalf("routing.ApplyOverrides rejected the written file: %v", err)
	}
}

// TestToolsCallDispatchVerifyInvalidIsInvalidParams verifies the MCP boundary
// refuses an unsafe/invalid verify list with a JSON-RPC invalid-params error
// (codeInvalidParams) BEFORE running any dispatch — for both the sync dispatch
// and the async dispatch_async submit path.
func TestToolsCallDispatchVerifyInvalidIsInvalidParams(t *testing.T) {
	s, _ := newTestServer(t, "canned")

	// A non-allowlisted / shell-metacharacter command must be rejected.
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":50,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"do it","verify":["rm -rf /"]}}}`)
	if resp.Error == nil {
		t.Fatalf("invalid verify: expected an error response, got result")
	}
	if resp.Error.Code != codeInvalidParams {
		t.Fatalf("invalid verify: error code = %d, want %d (invalid params)", resp.Error.Code, codeInvalidParams)
	}

	// The async path rejects at submit time too, not by returning a running job.
	respAsync := call(t, s,
		`{"jsonrpc":"2.0","id":51,"method":"tools/call","params":{"name":"dispatch_async","arguments":{"task":"do it","verify":["go test | tee x"]}}}`)
	if respAsync.Error == nil {
		t.Fatalf("invalid verify (async): expected an error response, got result")
	}
	if respAsync.Error.Code != codeInvalidParams {
		t.Fatalf("invalid verify (async): error code = %d, want %d", respAsync.Error.Code, codeInvalidParams)
	}
}

// TestToolsCallDispatchVerifyPassingInPayload verifies a valid passing verify
// list runs and its outcome is surfaced in the dispatch payload as a "verify"
// object with passed=true.
func TestToolsCallDispatchVerifyPassingInPayload(t *testing.T) {
	canned := `{"status":"ok","result":"did the thing","summary":"s","memory_updates":[]}`
	s, _ := newTestServer(t, canned)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":52,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","verify":["go version"]}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch with passing verify returned error: %+v", resp.Error)
	}
	var payload struct {
		Status string `json:"status"`
		Verify *struct {
			Passed   bool     `json:"passed"`
			Commands []string `json:"commands"`
		} `json:"verify"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &payload); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if payload.Verify == nil {
		t.Fatalf("payload missing verify object: %s", contentText(t, resp))
	}
	if !payload.Verify.Passed {
		t.Fatalf("payload verify.passed = false, want true: %s", contentText(t, resp))
	}
	if payload.Status != "ok" {
		t.Fatalf("status = %q, want ok (passing verify does not change status)", payload.Status)
	}
}

// TestToolsCallDispatchFilesInPayload verifies that when an IN-PLACE dispatch
// actually changes a file on disk (in a git repo), the resulting payload carries
// a "files" manifest entry for it. The fake adapter's Exec writes a file into the
// process cwd (which the test temporarily points at a scratch git repo) before
// returning the canned author response, simulating what a real CLI dispatch
// would do. isolate:false is passed explicitly: isolation now defaults on, and
// an isolated run would snapshot the ephemeral worktree (where the fake never
// writes) rather than the base repo, so this legacy in-place changed-files
// contract is only exercised on the DispatchModel path.
func TestToolsCallDispatchFilesInPayload(t *testing.T) {
	repo := t.TempDir()
	runGitMcp(t, repo, "init")
	runGitMcp(t, repo, "config", "user.email", "test@test.com")
	runGitMcp(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitMcp(t, repo, "add", "-A")
	runGitMcp(t, repo, "commit", "-m", "init")

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	canned := `{"status":"ok","result":"did the thing","summary":"s","memory_updates":[]}`
	s, _ := newTestServer(t, canned)
	s.o.GetAdapter = func(name string) (agent.Adapter, error) {
		ad, err := agent.GetAdapter(name)
		if err != nil {
			return nil, err
		}
		ad.Exec = func(argv []string) (string, error) {
			if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("added"), 0o644); err != nil {
				return "", err
			}
			return canned, nil
		}
		return ad, nil
	}

	resp := call(t, s,
		`{"jsonrpc":"2.0","id":53,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a file","isolate":false}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch returned error: %+v", resp.Error)
	}
	var payload struct {
		Files []struct {
			Path   string `json:"Path"`
			Status string `json:"Status"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &payload); err != nil {
		t.Fatalf("dispatch content not JSON: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("payload.files = %v, want exactly one entry for new.txt: %s", payload.Files, contentText(t, resp))
	}
	if payload.Files[0].Path != "new.txt" || payload.Files[0].Status != "untracked" {
		t.Fatalf("payload.files[0] = %+v, want {new.txt untracked}", payload.Files[0])
	}
}

// runGitMcp runs a git command in dir, failing the test on error.
func runGitMcp(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestToolsCallDispatchVerifySchema verifies tools/list advertises the optional
// "verify" array argument on BOTH the dispatch and dispatch_async input schemas.
func TestToolsCallDispatchVerifySchema(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":53,"method":"tools/list"}`)
	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)

	seen := map[string]bool{}
	for _, tr := range toolsRaw {
		td, _ := tr.(map[string]any)
		name, _ := td["name"].(string)
		if name != "dispatch" && name != "dispatch_async" {
			continue
		}
		schema, _ := td["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		verify, ok := props["verify"].(map[string]any)
		if !ok {
			t.Fatalf("%s inputSchema missing verify property: %v", name, schema)
		}
		if verify["type"] != "array" {
			t.Fatalf("%s verify property type = %v, want array", name, verify["type"])
		}
		items, ok := verify["items"].(map[string]any)
		if !ok || items["type"] != "string" {
			t.Fatalf("%s verify items = %v, want {type:string}", name, verify["items"])
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if r == "verify" {
				t.Fatalf("%s: verify must not be required: %v", name, required)
			}
		}
		seen[name] = true
	}
	if !seen["dispatch"] || !seen["dispatch_async"] {
		t.Fatalf("verify property not found on both tools; saw %v", seen)
	}
}

// cannedPlan is a planner stdout the fake adapter returns for the plan tool:
// prose then a valid plan JSON block with one step.
const cannedPlan = `Decomposed the goal.

{"steps":[{"id":"s1","title":"do the thing","difficulty":"standard","suggested_model":"claude-sonnet-5","suggested_verify":["go test ./..."],"depends_on":[]}],"summary":"one-step plan"}`

func TestToolsCallPlan(t *testing.T) {
	s, mem := newTestServer(t, cannedPlan)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"plan","arguments":{"goal":"build the thing"}}}`)

	if resp.Error != nil {
		t.Fatalf("plan returned error: %+v", resp.Error)
	}
	text := contentText(t, resp)

	var out struct {
		Agent   string `json:"agent"`
		Model   string `json:"model"`
		Summary string `json:"summary"`
		Key     string `json:"key"`
		Steps   []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("plan content not JSON: %v (text=%s)", err, text)
	}
	if len(out.Steps) != 1 || out.Steps[0].ID != "s1" {
		t.Fatalf("plan steps = %+v, want one step id=s1", out.Steps)
	}
	if out.Summary != "one-step plan" {
		t.Errorf("plan summary = %q, want %q", out.Summary, "one-step plan")
	}
	// The plan was persisted under the reported key.
	if out.Key == "" {
		t.Fatalf("plan key empty; not persisted")
	}
	if _, ok := mem.Read(out.Key); !ok {
		t.Fatalf("plan not persisted under key %q", out.Key)
	}
}

func TestToolsCallPlanEmptyGoal(t *testing.T) {
	s, _ := newTestServer(t, cannedPlan)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"plan","arguments":{"goal":""}}}`)

	if resp.Error == nil {
		t.Fatalf("plan with empty goal returned no error, want invalid-params")
	}
	if resp.Error.Code != codeInvalidParams {
		t.Fatalf("empty-goal error code = %d, want %d", resp.Error.Code, codeInvalidParams)
	}
}

// TestToolsCallDispatchEffortRoutesThrough verifies the dispatch tool accepts a
// valid effort value and routes through without error, returning the canned
// result (the effort reaches the author's argv; the wiring itself is asserted
// in the orchestrator package).
func TestToolsCallDispatchEffortRoutesThrough(t *testing.T) {
	s, _ := newTestServer(t, "canned-effort")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":50,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","agent":"claude","effort":"high"}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch with effort returned error: %+v", resp.Error)
	}
	if text := contentText(t, resp); !strings.Contains(text, "canned-effort") {
		t.Fatalf("dispatch content missing canned result: %s", text)
	}
}

// TestToolsCallDispatchInvalidEffort verifies a non-empty effort outside the
// allowed set is rejected as invalid-params BEFORE any dispatch work runs.
func TestToolsCallDispatchInvalidEffort(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":51,"method":"tools/call","params":{"name":"dispatch","arguments":{"task":"add a comment","effort":"bogus"}}}`)
	if resp.Error == nil {
		t.Fatalf("dispatch with invalid effort: expected error, got %+v", resp)
	}
	if resp.Error.Code != codeInvalidParams {
		t.Fatalf("invalid effort error code = %d, want %d", resp.Error.Code, codeInvalidParams)
	}
}

// TestToolsCallDispatchEffortSchema verifies tools/list advertises the optional
// "effort" string argument on both the dispatch and dispatch_async schemas, and
// that it is not required.
func TestToolsCallDispatchEffortSchema(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s, `{"jsonrpc":"2.0","id":52,"method":"tools/list"}`)
	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)

	seen := map[string]bool{}
	for _, tr := range toolsRaw {
		td, _ := tr.(map[string]any)
		name, _ := td["name"].(string)
		if name != "dispatch" && name != "dispatch_async" {
			continue
		}
		schema, _ := td["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		effort, ok := props["effort"].(map[string]any)
		if !ok {
			t.Fatalf("%s inputSchema missing effort property: %v", name, schema)
		}
		if effort["type"] != "string" {
			t.Fatalf("%s effort property type = %v, want string", name, effort["type"])
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if r == "effort" {
				t.Fatalf("%s: effort must not be required: %v", name, required)
			}
		}
		seen[name] = true
	}
	if !seen["dispatch"] || !seen["dispatch_async"] {
		t.Fatalf("effort property not found on both tools; saw %v", seen)
	}
}

// TestToolsCallDispatchMulti verifies the dispatch_multi tool fans a task out to
// each requested model and returns a candidates array (one per model, in order)
// carrying each candidate's agent/model/result/status.
func TestToolsCallDispatchMulti(t *testing.T) {
	canned := `{"status":"ok","result":"candidate answer","summary":"s","memory_updates":[]}`
	s, _ := newTestServer(t, canned)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":60,"method":"tools/call","params":{"name":"dispatch_multi","arguments":{"task":"solve this","models":["claude-opus-4-8","gpt-5.5"]}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch_multi returned error: %+v", resp.Error)
	}
	var payload struct {
		Candidates []struct {
			Agent  string `json:"agent"`
			Model  string `json:"model"`
			Result string `json:"result"`
			Status string `json:"status"`
		} `json:"candidates"`
		Synthesis *struct {
			Model string `json:"model"`
		} `json:"synthesis"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &payload); err != nil {
		t.Fatalf("dispatch_multi content not JSON: %v", err)
	}
	if len(payload.Candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(payload.Candidates))
	}
	if payload.Candidates[0].Model != "claude-opus-4-8" || payload.Candidates[0].Agent != "claude" {
		t.Fatalf("candidate[0] = %+v, want model claude-opus-4-8 agent claude", payload.Candidates[0])
	}
	if payload.Candidates[1].Model != "gpt-5.5" || payload.Candidates[1].Agent != "codex" {
		t.Fatalf("candidate[1] = %+v, want model gpt-5.5 agent codex", payload.Candidates[1])
	}
	if payload.Candidates[0].Result != "candidate answer" || payload.Candidates[0].Status != "ok" {
		t.Fatalf("candidate[0] result/status = %q/%q, want candidate answer/ok", payload.Candidates[0].Result, payload.Candidates[0].Status)
	}
	// synthesis omitted without synthesis="judge".
	if payload.Synthesis != nil {
		t.Fatalf("synthesis = %+v, want omitted (no judge requested)", payload.Synthesis)
	}
}

// TestToolsCallDispatchMultiMissingModels verifies task without models (and an
// empty models list) is rejected at the MCP boundary as invalid-params.
func TestToolsCallDispatchMultiMissingModels(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	for _, args := range []string{
		`{"task":"solve this"}`,
		`{"task":"solve this","models":[]}`,
	} {
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":61,"method":"tools/call","params":{"name":"dispatch_multi","arguments":`+args+`}}`)
		if resp.Error == nil {
			t.Fatalf("dispatch_multi(%s) error = nil, want invalid-params", args)
		}
		if resp.Error.Code != codeInvalidParams {
			t.Fatalf("dispatch_multi(%s) error code = %d, want %d", args, resp.Error.Code, codeInvalidParams)
		}
	}
}

// TestToolsCallDispatchMultiAsyncReturnsJobId verifies dispatch_multi_async
// returns a job_id and status "running" immediately, without waiting for the
// background fan-out to finish.
func TestToolsCallDispatchMultiAsyncReturnsJobId(t *testing.T) {
	canned := `{"status":"ok","result":"candidate answer","summary":"s","memory_updates":[]}`
	s, _ := newTestServer(t, canned)
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":62,"method":"tools/call","params":{"name":"dispatch_multi_async","arguments":{"task":"solve this","models":["claude-opus-4-8","gpt-5.5"]}}}`)
	if resp.Error != nil {
		t.Fatalf("dispatch_multi_async returned error: %+v", resp.Error)
	}
	var res struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, resp)), &res); err != nil {
		t.Fatalf("dispatch_multi_async content not JSON: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("dispatch_multi_async missing job_id: %s", contentText(t, resp))
	}
	if res.Status != "running" {
		t.Fatalf("dispatch_multi_async status = %q, want running", res.Status)
	}

	// Drain the background job before returning: t.TempDir()'s cleanup
	// races the goroutine's best-effort persist write otherwise (see
	// internal/jobs.Manager.Submit). Long-polling job_status until
	// terminal makes the wait deterministic without weakening the
	// immediate-running assertion above.
	statusReq := `{"jsonrpc":"2.0","id":63,"method":"tools/call","params":{"name":"job_status","arguments":{"job_id":"` + res.JobID + `","wait_sec":5}}}`
	statusResp := call(t, s, statusReq)
	if statusResp.Error != nil {
		t.Fatalf("job_status returned error: %+v", statusResp.Error)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(contentText(t, statusResp)), &status); err != nil {
		t.Fatalf("job_status content not JSON: %v", err)
	}
	if status.Status != "done" {
		t.Fatalf("job_status status = %q, want done", status.Status)
	}
}

// TestToolsCallDispatchMultiAsyncMissingModels verifies task without models
// (and an empty models list) is rejected at submit time as invalid-params,
// same as the sync dispatch_multi tool.
func TestToolsCallDispatchMultiAsyncMissingModels(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	for _, args := range []string{
		`{"task":"solve this"}`,
		`{"task":"solve this","models":[]}`,
	} {
		resp := call(t, s,
			`{"jsonrpc":"2.0","id":64,"method":"tools/call","params":{"name":"dispatch_multi_async","arguments":`+args+`}}`)
		if resp.Error == nil {
			t.Fatalf("dispatch_multi_async(%s) error = nil, want invalid-params", args)
		}
		if resp.Error.Code != codeInvalidParams {
			t.Fatalf("dispatch_multi_async(%s) error code = %d, want %d", args, resp.Error.Code, codeInvalidParams)
		}
	}
}

// planGatePayload mirrors orchestrator.GateResult for decoding the plan_gate
// tool's JSON content.
type planGatePayload struct {
	StepsRemaining int  `json:"steps_remaining"`
	Verify         struct {
		Passed   bool     `json:"passed"`
		Commands []string `json:"commands"`
	} `json:"verify"`
	GoalMet       bool   `json:"goal_met"`
	GoalMetReason string `json:"goal_met_reason"`
	CanStop       bool   `json:"can_stop"`
}

// TestToolsCallPlanGateNoActivePlan verifies plan_gate is invalid-params when
// no plan has been established, mirroring plan_status's "no active plan" case.
func TestToolsCallPlanGateNoActivePlan(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	resp := call(t, s,
		`{"jsonrpc":"2.0","id":70,"method":"tools/call","params":{"name":"plan_gate","arguments":{}}}`)
	if resp.Error == nil {
		t.Fatalf("plan_gate with no active plan: error = nil, want invalid-params")
	}
	if resp.Error.Code != codeInvalidParams {
		t.Fatalf("plan_gate with no active plan: error code = %d, want %d", resp.Error.Code, codeInvalidParams)
	}
}

// TestToolsCallPlanGateStepsRemainingBlocks seeds a plan with one still-pending
// step and a judge that affirms goal_met, proving can_stop stays false while
// steps remain even though verify and the judge both agree.
func TestToolsCallPlanGateStepsRemainingBlocks(t *testing.T) {
	canned := `{"goal_met": true, "reason": "looks done"}`
	s, _ := newTestServer(t, canned)
	if err := s.plan.SaveWith("build the thing", "", nil, []plan.Step{
		{ID: "s1", Title: "do it", Status: plan.StatusPending},
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	resp := call(t, s,
		`{"jsonrpc":"2.0","id":71,"method":"tools/call","params":{"name":"plan_gate","arguments":{"workdir":"`+t.TempDir()+`"}}}`)
	if resp.Error != nil {
		t.Fatalf("plan_gate returned error: %+v", resp.Error)
	}
	var out planGatePayload
	if err := json.Unmarshal([]byte(contentText(t, resp)), &out); err != nil {
		t.Fatalf("plan_gate content not JSON: %v", err)
	}
	if out.StepsRemaining != 1 {
		t.Errorf("steps_remaining = %d, want 1", out.StepsRemaining)
	}
	if !out.Verify.Passed {
		t.Errorf("verify.passed = false, want true (empty verify_cmds is trivially passed)")
	}
	if !out.GoalMet {
		t.Errorf("goal_met = false, want true (judge affirmed it)")
	}
	if out.CanStop {
		t.Errorf("can_stop = true, want false (a step still pending must block the gate)")
	}
}

// TestToolsCallPlanGateCanStop seeds a fully-done plan with a real passing
// integration verify command and a judge that affirms goal_met, proving the
// gate reports can_stop=true only once every axis agrees.
func TestToolsCallPlanGateCanStop(t *testing.T) {
	canned := `{"goal_met": true, "reason": "all done"}`
	s, _ := newTestServer(t, canned)
	if err := s.plan.SaveWith("build the thing", "the spec", []string{"go version"}, []plan.Step{
		{ID: "s1", Title: "do it", Status: plan.StatusDone},
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	resp := call(t, s,
		`{"jsonrpc":"2.0","id":72,"method":"tools/call","params":{"name":"plan_gate","arguments":{"workdir":"`+t.TempDir()+`"}}}`)
	if resp.Error != nil {
		t.Fatalf("plan_gate returned error: %+v", resp.Error)
	}
	var out planGatePayload
	if err := json.Unmarshal([]byte(contentText(t, resp)), &out); err != nil {
		t.Fatalf("plan_gate content not JSON: %v", err)
	}
	if out.StepsRemaining != 0 {
		t.Errorf("steps_remaining = %d, want 0", out.StepsRemaining)
	}
	if !out.Verify.Passed {
		t.Errorf("verify.passed = false, want true (\"go version\" succeeds)")
	}
	if !out.GoalMet || out.GoalMetReason != "all done" {
		t.Errorf("goal_met/reason = %v/%q, want true/\"all done\"", out.GoalMet, out.GoalMetReason)
	}
	if !out.CanStop {
		t.Errorf("can_stop = false, want true (no steps remain, verify passed, goal met)")
	}
}

// TestToolsCallPlanGateConservativeOnUnparseableJudge proves the gate never
// reports goal_met/can_stop=true when the judge's output carries no parseable
// verdict, even with zero steps remaining and verify passing.
func TestToolsCallPlanGateConservativeOnUnparseableJudge(t *testing.T) {
	s, _ := newTestServer(t, "not a JSON verdict at all")
	if err := s.plan.SaveWith("build the thing", "", nil, []plan.Step{
		{ID: "s1", Title: "do it", Status: plan.StatusDone},
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	resp := call(t, s,
		`{"jsonrpc":"2.0","id":73,"method":"tools/call","params":{"name":"plan_gate","arguments":{"workdir":"`+t.TempDir()+`"}}}`)
	if resp.Error != nil {
		t.Fatalf("plan_gate returned error: %+v", resp.Error)
	}
	var out planGatePayload
	if err := json.Unmarshal([]byte(contentText(t, resp)), &out); err != nil {
		t.Fatalf("plan_gate content not JSON: %v", err)
	}
	if out.GoalMet {
		t.Errorf("goal_met = true, want false when the judge produced no parseable verdict")
	}
	if out.CanStop {
		t.Errorf("can_stop = true, want false when the judge produced no parseable verdict")
	}
	if out.GoalMetReason == "" {
		t.Errorf("goal_met_reason empty, want an explanation of the unparseable judge output")
	}
}

// TestToolsCallPlanGateInvalidVerifyIsInternalError proves a plan whose
// persisted verify_cmds is unsafe/non-allowlisted refuses the gate as a
// config error (surfaced as an internal error by callPlanGate) rather than
// running the judge on a poisoned state.
func TestToolsCallPlanGateInvalidVerifyIsInternalError(t *testing.T) {
	s, _ := newTestServer(t, "canned")
	if err := s.plan.SaveWith("build the thing", "", []string{"rm -rf /"}, []plan.Step{
		{ID: "s1", Title: "do it", Status: plan.StatusDone},
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	resp := call(t, s,
		`{"jsonrpc":"2.0","id":74,"method":"tools/call","params":{"name":"plan_gate","arguments":{"workdir":"`+t.TempDir()+`"}}}`)
	if resp.Error == nil {
		t.Fatalf("plan_gate with unsafe verify_cmds: error = nil, want an error")
	}
	if resp.Error.Code != codeInternalError {
		t.Fatalf("plan_gate with unsafe verify_cmds: error code = %d, want %d", resp.Error.Code, codeInternalError)
	}
}
