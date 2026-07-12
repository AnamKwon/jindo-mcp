#!/usr/bin/env python3
"""jindo benchmark harness (pilot).

Answers the review's central question — "is JINDO actually better, and which
agent should route which work?" — with VERIFIED outcomes, not opinions.

For every task x config it: sets up a fresh RED git repo from the task fixture,
dispatches through the jindo-mcp binary (isolate + objective verify), merges the
returned branch on success, and records whether `go test` actually passes, how
long it took, and how many agent calls it cost. It then prints the KPI table the
review asked for and writes:
  - bench/report.md            (human KPI report)
  - bench/routing_proposal.json (best agent per task_type by verified success —
                                 DATA for the routing owner to consume; this
                                 harness never edits internal/routing itself)

Usage:  python3 bench/run.py            # pilot: the built-in task suite
Env:    JINDO_BIN (default ./jindo-mcp), JINDO_BENCH_CONFIGS (comma list)

NOTE (pilot honesty): agy is omitted by default because its CLI daily quota was
exhausted during development; add it to CONFIGS once the quota resets. This is a
PILOT (small suite) that proves the harness and yields directional data — not a
statistically strong benchmark. Scale the suite for real conclusions.
"""
import json, os, subprocess, time, sys, tempfile, shutil

REPO = os.environ.get("JINDO_REPO", "/Users/anamkwon/coding-agent-collaboration")
BIN = os.environ.get("JINDO_BIN", os.path.join(REPO, "jindo-mcp"))
WORKROOT = os.environ.get("JINDO_BENCH_WORK",
    "/private/tmp/claude-501/-Users-anamkwon-coding-agent-collaboration/fa167fa5-2224-4e01-8cfe-2477b81d8b26/scratchpad/benchwork")

# ---- Task suite (hermetic, verify-gated, tagged) --------------------------
# Each task: id, task_type, difficulty, language, files{path:content} (RED
# fixture — compiles-but-fails or fails-to-build until solved), prompt, verify.
GO_MOD = "module bench\n\ngo 1.23\n"

TASKS = [
  {
    "id": "impl_dedup", "task_type": "implementation", "difficulty": "standard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/dedup_test.go": (
        "package p\nimport (\"reflect\";\"testing\")\n"
        "func TestDedup(t *testing.T){\n"
        " if got:=Dedup([]int{1,1,2,3,3,3,2}); !reflect.DeepEqual(got,[]int{1,2,3,2}){t.Fatalf(\"got %v\",got)}\n"
        " if got:=Dedup([]string{}); len(got)!=0 {t.Fatalf(\"empty: %v\",got)}\n"
        "}\n"),
    },
    "prompt": ("Implement `func Dedup[T comparable](in []T) []T` in ./p/dedup.go (package p): "
               "remove CONSECUTIVE duplicate elements (like uniq), preserving order. Only create ./p/dedup.go."),
    "verify": ["go test ./p/..."],
  },
  {
    "id": "debug_off_by_one", "task_type": "debugging", "difficulty": "standard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/sum.go": ("package p\n// SumTo returns 1+2+...+n. It has a bug.\n"
                   "func SumTo(n int) int { s:=0; for i:=1;i<n;i++ { s+=i }; return s }\n"),
      "p/sum_test.go": ("package p\nimport \"testing\"\n"
                        "func TestSumTo(t *testing.T){ if SumTo(5)!=15 {t.Fatalf(\"got %d want 15\",SumTo(5))}; if SumTo(1)!=1{t.Fatalf(\"got %d want 1\",SumTo(1))} }\n"),
    },
    "prompt": ("The test ./p/sum_test.go fails: SumTo is off by one (loop bound). Fix ./p/sum.go so `go test` passes. "
               "Edit only ./p/sum.go."),
    "verify": ["go test ./p/..."],
  },
  {
    "id": "test_gen", "task_type": "test_generation", "difficulty": "trivial", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/rev.go": ("package p\nfunc Reverse(s string) string { r:=[]rune(s); for i,j:=0,len(r)-1;i<j;i,j=i+1,j-1 {r[i],r[j]=r[j],r[i]}; return string(r) }\n"),
      "p/keep.go": ("package p\n// keep the package compiling before the test is added\n"),
    },
    "prompt": ("Write a table-driven Go test ./p/rev_test.go (package p) for the existing Reverse(s string) string, "
               "covering empty, ascii, and a multibyte string. Only create ./p/rev_test.go; do not modify rev.go."),
    "verify": ["go test ./p/..."],
  },
  {
    "id": "impl_korean", "task_type": "implementation", "difficulty": "standard", "language": "ko",
    "files": {
      "go.mod": GO_MOD,
      "p/atomic_test.go": ("package p\nimport \"testing\"\n"
        "func TestCounter(t *testing.T){ c:=NewCounter(); for i:=0;i<100;i++{ go c.Inc() }; /*not concurrency-strict*/ }\n"
        "func TestCounterBasic(t *testing.T){ c:=NewCounter(); c.Inc(); c.Inc(); if c.Value()!=2 {t.Fatalf(\"got %d\",c.Value())} }\n"),
    },
    "prompt": ("동시성 안전한 카운터를 구현해줘: ./p/counter.go (package p)에 `func NewCounter() *Counter`, "
               "메서드 `Inc()`와 `Value() int`를 만들어. 여러 goroutine이 Inc()를 동시에 호출해도 경쟁 상태(race)가 "
               "없어야 하고, sync/atomic 또는 뮤텍스를 써. ./p/counter.go 만 생성해."),
    "verify": ["go test ./p/...", "go vet ./p/..."],
  },
]

# ---- Configs: how to route each task --------------------------------------
# model="" => JINDO auto-routing (no pin). review toggles cross-model review.
DEFAULT_CONFIGS = [
  {"name": "claude_solo",  "model": "claude-sonnet-5",       "review": False},
  {"name": "codex_solo",   "model": "gpt-5.3-codex-spark",   "review": False},
  {"name": "jindo_auto",   "model": "",                       "review": False},
  {"name": "jindo_review", "model": "",                       "review": True},
]

def sh(*a, cwd=None):
    return subprocess.run(a, cwd=cwd, capture_output=True, text=True)

def setup_repo(task):
    d = tempfile.mkdtemp(prefix=f"{task['id']}-", dir=WORKROOT)
    for rel, content in task["files"].items():
        p = os.path.join(d, rel); os.makedirs(os.path.dirname(p), exist_ok=True)
        open(p, "w").write(content)
    sh("git", "init", "-q", cwd=d); sh("git", "config", "user.email", "b@b", cwd=d); sh("git", "config", "user.name", "b", cwd=d)
    sh("git", "add", "-A", cwd=d); sh("git", "commit", "-qm", "RED", cwd=d)
    return d

# ---- JSON-RPC client over the jindo-mcp binary ----------------------------
class Client:
    def __init__(self):
        self.p = subprocess.Popen([BIN], cwd=REPO, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.id = 0
        self._rpc("initialize", {})
    def _rpc(self, method, params):
        self.id += 1; rid = self.id
        self.p.stdin.write(json.dumps({"jsonrpc":"2.0","id":rid,"method":method,"params":params})+"\n"); self.p.stdin.flush()
        while True:
            ln = self.p.stdout.readline()
            if not ln: raise RuntimeError("server closed")
            ln = ln.strip()
            if not ln: continue
            o = json.loads(ln)
            if o.get("id") == rid:
                if o.get("error"): raise RuntimeError(o["error"])
                return o.get("result")
    def tool(self, name, args):
        return json.loads(self._rpc("tools/call", {"name": name, "arguments": args})["content"][0]["text"])
    def dispatch_async(self, args):
        j = self.tool("dispatch_async", args); jid = j["job_id"]
        while True:
            s = self.tool("job_status", {"job_id": jid, "wait_sec": 25})
            if s.get("status") in ("done", "error"): return s.get("result", s)
    def close(self):
        try: self.p.stdin.close(); self.p.wait(timeout=10)
        except Exception: pass

def run_case(cli, task, cfg):
    d = setup_repo(task)
    args = {"task": task["prompt"], "verify": task["verify"], "workdir": d,
            "review": cfg["review"], "isolate": True, "effort": "medium"}
    if cfg["model"]: args["model"] = cfg["model"]
    t0 = time.time()
    try:
        r = cli.dispatch_async(args)
    except Exception as e:
        return {"ok": False, "err": str(e), "secs": time.time()-t0, "agent": "?", "calls": 0}
    secs = time.time() - t0
    iso = r.get("isolation") or {}
    agent = r.get("agent", "?")
    calls = 1 + len(r.get("reviews") or [])
    # merge the isolate branch, then the ground truth: does `go test` pass?
    if iso.get("committed") and iso.get("branch"):
        sh("git", "merge", "--no-ff", iso["branch"], "-m", "merge", cwd=d)
    ver = sh("go", "test", "./p/...", cwd=d)
    passed = ver.returncode == 0
    shutil.rmtree(d, ignore_errors=True)
    return {"ok": passed, "secs": round(secs,1), "agent": agent, "calls": calls,
            "status": r.get("status"), "verify_reported": (r.get("verify") or {}).get("passed")}

def main():
    os.makedirs(WORKROOT, exist_ok=True)
    names = os.environ.get("JINDO_BENCH_CONFIGS")
    configs = [c for c in DEFAULT_CONFIGS if (not names or c["name"] in names.split(","))]
    cli = Client()
    results = []
    for task in TASKS:
        for cfg in configs:
            print(f"[run] {task['id']:16} x {cfg['name']:13} ...", flush=True)
            res = run_case(cli, task, cfg)
            print(f"      -> verified={res['ok']} agent={res.get('agent')} secs={res.get('secs')} calls={res.get('calls')}", flush=True)
            results.append({"task": task["id"], "task_type": task["task_type"], "difficulty": task["difficulty"],
                            "language": task["language"], "config": cfg["name"], **res})
    cli.close()
    open(os.path.join(REPO, "bench", "results.json"), "w").write(json.dumps(results, indent=2, ensure_ascii=False))
    report(results)

def report(results):
    configs = []
    for r in results:
        if r["config"] not in configs: configs.append(r["config"])
    # KPI per config
    lines = ["# jindo benchmark — pilot report", "",
             "PILOT: small suite, claude+codex only (agy quota-exhausted). Directional, not conclusive.", "",
             "## KPI by config", "", "| config | verified pass | avg secs | avg calls |",
             "|---|---|---|---|"]
    for c in configs:
        rs = [r for r in results if r["config"] == c]
        pv = sum(1 for r in rs if r["ok"]); n = len(rs)
        avgs = round(sum(r["secs"] for r in rs)/n, 1) if n else 0
        avgc = round(sum(r["calls"] for r in rs)/n, 1) if n else 0
        lines.append(f"| {c} | {pv}/{n} ({round(100*pv/n)}%) | {avgs} | {avgc} |")
    # task_type x agent verified matrix (solo configs only, where model is pinned)
    lines += ["", "## verified success by task_type × solo agent", "", "| task_type | claude | codex |", "|---|---|---|"]
    ttypes = []
    for r in results:
        if r["task_type"] not in ttypes: ttypes.append(r["task_type"])
    proposal = {}
    for tt in ttypes:
        cell = {}
        for agentcfg, label in (("claude_solo","claude"),("codex_solo","codex")):
            rs = [r for r in results if r["task_type"]==tt and r["config"]==agentcfg]
            pv = sum(1 for r in rs if r["ok"]); n=len(rs)
            cell[label] = (pv, n, (sum(r["secs"] for r in rs)/n if n else 9e9))
        lines.append(f"| {tt} | {cell['claude'][0]}/{cell['claude'][1]} | {cell['codex'][0]}/{cell['codex'][1]} |")
        # proposal: highest pass rate, tiebreak faster
        best = max(cell.items(), key=lambda kv: (kv[1][0]/kv[1][1] if kv[1][1] else 0, -kv[1][2]))
        proposal[tt] = {"best_agent": best[0], "verified": f"{best[1][0]}/{best[1][1]}", "avg_secs": round(best[1][2],1)}
    lines += ["", "## routing proposal (data for the routing owner; NOT auto-applied)", "",
              "```json", json.dumps(proposal, indent=2, ensure_ascii=False), "```",
              "", "Consume via internal/routing (owner) — this harness never edits the router."]
    md = "\n".join(lines)
    open(os.path.join(REPO, "bench", "report.md"), "w").write(md+"\n")
    open(os.path.join(REPO, "bench", "routing_proposal.json"), "w").write(json.dumps(proposal, indent=2, ensure_ascii=False)+"\n")
    print("\n"+md, flush=True)
    print("\n== BENCH DONE ==", flush=True)

if __name__ == "__main__":
    main()
