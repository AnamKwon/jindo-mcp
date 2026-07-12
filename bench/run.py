#!/usr/bin/env python3
"""jindo benchmark harness.

Answers, with VERIFIED outcomes: (1) is JINDO actually better, and (2) which
agent should route which work? For every task x config it sets up a fresh RED git
repo from the fixture, dispatches through the jindo-mcp binary (isolate + verify),
merges the returned branch on success, then runs the task's OWN verify commands as
ground truth (incl. `-race` for concurrency tasks). It writes bench/report.md,
bench/routing_proposal.json, bench/results.json.

This revision runs a HARDER suite CODEX-ONLY (claude/agy tokens exhausted). With a
single agent, cross-agent routing can't be compared; instead it measures the
core product claim under one agent: does JINDO's objective verify gate +
self-heal (auto-revision on verify failure) lift codex's verified success on hard
tasks vs raw codex?
  - codex_raw    : dispatch with NO verify -> author once, no gate, no retry.
  - codex_verify : dispatch WITH verify    -> objective gate + auto-revision.
The LIFT (codex_verify pass-rate minus codex_raw pass-rate) is JINDO's value here.

Usage:  python3 bench/run.py
Env:    JINDO_BIN, JINDO_BENCH_CONFIGS (comma list), JINDO_BENCH_MODEL (default gpt-5.5)
"""
import json, os, subprocess, time, tempfile, shutil

REPO = os.environ.get("JINDO_REPO", "/Users/anamkwon/coding-agent-collaboration")
BIN = os.environ.get("JINDO_BIN", os.path.join(REPO, "jindo-mcp"))
WORKROOT = os.environ.get("JINDO_BENCH_WORK",
    "/private/tmp/claude-501/-Users-anamkwon-coding-agent-collaboration/fa167fa5-2224-4e01-8cfe-2477b81d8b26/scratchpad/benchwork")
CODEX_MODEL = os.environ.get("JINDO_BENCH_MODEL", "gpt-5.5")  # codex hard-tier

GO_MOD = "module bench\n\ngo 1.23\n"

# ---- HARD suite: hermetic, verify-gated, thorough tests (first attempts can fail) ----
TASKS = [
  {
    "id": "lru_ttl", "task_type": "implementation", "difficulty": "hard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/lru_test.go": (
        "package p\nimport (\"sync\";\"testing\";\"time\")\n"
        "func TestLRUBasic(t *testing.T){ c:=New(2, time.Minute); c.Put(\"a\",1); c.Put(\"b\",2)\n"
        " if v,ok:=c.Get(\"a\"); !ok||v!=1 {t.Fatalf(\"a=%v,%v\",v,ok)}\n"
        " c.Put(\"c\",3) // evicts LRU which is b (a was just used)\n"
        " if _,ok:=c.Get(\"b\"); ok {t.Fatalf(\"b should be evicted\")}\n"
        " if v,ok:=c.Get(\"c\"); !ok||v!=3 {t.Fatalf(\"c=%v,%v\",v,ok)} }\n"
        "func TestLRUTTL(t *testing.T){ c:=New(10, 30*time.Millisecond); c.Put(\"x\",9)\n"
        " if _,ok:=c.Get(\"x\"); !ok {t.Fatalf(\"x should be present\")}\n"
        " time.Sleep(60*time.Millisecond)\n"
        " if _,ok:=c.Get(\"x\"); ok {t.Fatalf(\"x should have expired\")} }\n"
        "func TestLRURace(t *testing.T){ c:=New(50, time.Minute); var wg sync.WaitGroup\n"
        " for i:=0;i<50;i++{ wg.Add(1); go func(i int){ defer wg.Done(); c.Put(string(rune('A'+i%26)), i); c.Get(\"A\") }(i) }\n"
        " wg.Wait() }\n"),
    },
    "prompt": ("Implement a concurrency-safe LRU cache with TTL in ./p/lru.go (package p): "
               "`func New(capacity int, ttl time.Duration) *Cache`, methods `Put(key string, val int)`, "
               "`Get(key string) (int, bool)`. Get returns false for missing OR expired entries. Put evicts "
               "the least-recently-used entry when over capacity (Get counts as a use). It MUST be safe under "
               "concurrent Put/Get (the tests run with -race). Only create ./p/lru.go."),
    "verify": ["go test ./p/... -race"],
  },
  {
    "id": "expr_eval", "task_type": "implementation", "difficulty": "hard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/eval_test.go": (
        "package p\nimport \"testing\"\n"
        "func TestEval(t *testing.T){\n"
        " cases:=[]struct{in string;want float64;err bool}{\n"
        "  {\"1+2*3\",7,false},{\"(1+2)*3\",9,false},{\"2*(3+4)-5\",9,false},\n"
        "  {\"-3+4\",1,false},{\" 10 / 4 \",2.5,false},{\"2*-3\",-6,false},\n"
        "  {\"1/0\",0,true},{\"1+\",0,true},{\"(1+2\",0,true},{\"\",0,true} }\n"
        " for _,c:=range cases{ got,err:=Eval(c.in)\n"
        "  if c.err { if err==nil {t.Fatalf(\"%q: expected error\",c.in)}; continue }\n"
        "  if err!=nil {t.Fatalf(\"%q: unexpected err %v\",c.in,err)}\n"
        "  if got!=c.want {t.Fatalf(\"%q: got %v want %v\",c.in,got,c.want)} } }\n"),
    },
    "prompt": ("Implement `func Eval(expr string) (float64, error)` in ./p/eval.go (package p): a correct "
               "arithmetic expression evaluator supporting + - * / , parentheses, unary minus, decimals and "
               "whitespace, with standard precedence. Return an error for division by zero, empty input, and "
               "malformed expressions (dangling operator, unbalanced parens). Only create ./p/eval.go."),
    "verify": ["go test ./p/..."],
  },
  {
    "id": "merge_intervals", "task_type": "implementation", "difficulty": "standard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/iv_test.go": (
        "package p\nimport (\"reflect\";\"testing\")\n"
        "func TestMerge(t *testing.T){\n"
        " cases:=[]struct{in,want [][2]int}{\n"
        "  {[][2]int{{1,3},{2,6},{8,10},{15,18}},[][2]int{{1,6},{8,10},{15,18}}},\n"
        "  {[][2]int{{1,4},{4,5}},[][2]int{{1,5}}},\n"
        "  {[][2]int{{5,6},{1,2},{3,4}},[][2]int{{1,2},{3,4},{5,6}}},\n"
        "  {[][2]int{{1,10},{2,3},{4,5}},[][2]int{{1,10}}},\n"
        "  {[][2]int{},[][2]int{}} }\n"
        " for _,c:=range cases{ if got:=Merge(c.in); !reflect.DeepEqual(got,c.want){t.Fatalf(\"in %v got %v want %v\",c.in,got,c.want)} } }\n"),
    },
    "prompt": ("Implement `func Merge(intervals [][2]int) [][2]int` in ./p/iv.go (package p): merge all "
               "overlapping or touching intervals and return them sorted by start. Input may be unsorted; do "
               "not mutate the input; empty input returns an empty (non-nil ok) slice. Only create ./p/iv.go."),
    "verify": ["go test ./p/..."],
  },
  {
    "id": "fix_race", "task_type": "debugging", "difficulty": "hard", "language": "en",
    "files": {
      "go.mod": GO_MOD,
      "p/counter.go": ("package p\n// Tally counts occurrences concurrently. It has a data race bug.\n"
                       "type Tally struct{ m map[string]int }\n"
                       "func NewTally() *Tally { return &Tally{m: map[string]int{}} }\n"
                       "func (t *Tally) Add(k string){ t.m[k]++ }\n"
                       "func (t *Tally) Get(k string) int { return t.m[k] }\n"),
      "p/counter_test.go": ("package p\nimport (\"sync\";\"testing\")\n"
        "func TestTallyRace(t *testing.T){ tl:=NewTally(); var wg sync.WaitGroup\n"
        " for i:=0;i<200;i++{ wg.Add(1); go func(){ defer wg.Done(); tl.Add(\"k\") }() }\n"
        " wg.Wait(); if tl.Get(\"k\")!=200 {t.Fatalf(\"got %d want 200\",tl.Get(\"k\"))} }\n"),
    },
    "prompt": ("./p/counter_test.go fails under the race detector: Tally.Add/Get race on the map. Fix ./p/counter.go "
               "so it is concurrency-safe (mutex or sharding) and `go test -race` passes, keeping the same API "
               "(NewTally, Add, Get). Edit only ./p/counter.go."),
    "verify": ["go test ./p/... -race"],
  },
  {
    "id": "topo_sort", "task_type": "implementation", "difficulty": "hard", "language": "ko",
    "files": {
      "go.mod": GO_MOD,
      "p/topo_test.go": (
        "package p\nimport \"testing\"\n"
        "func idx(order []string, s string) int { for i,v:=range order { if v==s {return i} }; return -1 }\n"
        "func TestTopo(t *testing.T){ deps:=map[string][]string{\"a\":{\"b\",\"c\"},\"b\":{\"d\"},\"c\":{\"d\"},\"d\":{}}\n"
        " order,err:=TopoSort(deps); if err!=nil {t.Fatalf(\"err %v\",err)}\n"
        " if len(order)!=4 {t.Fatalf(\"len %d\",len(order))}\n"
        " if idx(order,\"d\")>idx(order,\"b\")||idx(order,\"b\")>idx(order,\"a\") {t.Fatalf(\"bad order %v\",order)} }\n"
        "func TestTopoCycle(t *testing.T){ if _,err:=TopoSort(map[string][]string{\"x\":{\"y\"},\"y\":{\"x\"}}); err==nil {t.Fatalf(\"expected cycle error\")} }\n"),
    },
    "prompt": ("./p/topo.go (package p)에 `func TopoSort(deps map[string][]string) ([]string, error)`를 구현해줘. "
               "deps[n]은 n이 의존하는 노드들이야(그 노드들이 n보다 먼저 와야 함). 위상정렬 결과를 반환하되, "
               "순환(cycle)이 있으면 error를 반환해. ./p/topo.go 만 생성해."),
    "verify": ["go test ./p/...", "go vet ./p/..."],
  },
]

# ---- Configs: CODEX-ONLY (claude/agy tokens exhausted) ---------------------
# codex_raw: no verify (author once). codex_verify: verify gate + auto-revision.
DEFAULT_CONFIGS = [
  {"name": "codex_raw",    "model": CODEX_MODEL, "review": False, "verify": False},
  {"name": "codex_verify", "model": CODEX_MODEL, "review": False, "verify": True},
]

def sh(*a, cwd=None): return subprocess.run(a, cwd=cwd, capture_output=True, text=True)

def setup_repo(task):
    d = tempfile.mkdtemp(prefix=f"{task['id']}-", dir=WORKROOT)
    for rel, content in task["files"].items():
        p = os.path.join(d, rel); os.makedirs(os.path.dirname(p), exist_ok=True); open(p,"w").write(content)
    sh("git","init","-q",cwd=d); sh("git","config","user.email","b@b",cwd=d); sh("git","config","user.name","b",cwd=d)
    sh("git","add","-A",cwd=d); sh("git","commit","-qm","RED",cwd=d)
    return d

def run_verify(d, cmds):
    for c in cmds:
        r = sh(*c.split(), cwd=d)
        if r.returncode != 0: return False
    return True

class Client:
    def __init__(self):
        self.p=subprocess.Popen([BIN],cwd=REPO,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True,bufsize=1); self.id=0
        self._rpc("initialize",{})
    def _rpc(self,method,params):
        self.id+=1;rid=self.id
        self.p.stdin.write(json.dumps({"jsonrpc":"2.0","id":rid,"method":method,"params":params})+"\n");self.p.stdin.flush()
        while True:
            ln=self.p.stdout.readline()
            if not ln: raise RuntimeError("closed")
            ln=ln.strip()
            if not ln: continue
            o=json.loads(ln)
            if o.get("id")==rid:
                if o.get("error"): raise RuntimeError(o["error"])
                return o.get("result")
    def tool(self,name,args): return json.loads(self._rpc("tools/call",{"name":name,"arguments":args})["content"][0]["text"])
    def dispatch_async(self,args):
        j=self.tool("dispatch_async",args);jid=j["job_id"]
        while True:
            s=self.tool("job_status",{"job_id":jid,"wait_sec":25})
            if s.get("status") in ("done","error"): return s.get("result",s)
    def close(self):
        try: self.p.stdin.close(); self.p.wait(timeout=10)
        except Exception: pass

def run_case(cli, task, cfg):
    d = setup_repo(task)
    args = {"task": task["prompt"], "workdir": d, "model": cfg["model"],
            "review": cfg["review"], "isolate": True, "effort": "high"}
    if cfg["verify"]: args["verify"] = task["verify"]
    t0=time.time()
    try: r=cli.dispatch_async(args)
    except Exception as e:
        shutil.rmtree(d,ignore_errors=True)
        return {"ok":False,"err":str(e),"secs":round(time.time()-t0,1),"agent":"?","revisions":0}
    secs=round(time.time()-t0,1)
    iso=r.get("isolation") or {}
    if iso.get("committed") and iso.get("branch"):
        sh("git","merge","--no-ff",iso["branch"],"-m","merge",cwd=d)
    passed = run_verify(d, task["verify"])   # ground truth = task's own verify (incl -race)
    revisions = (r.get("verify") or {}).get("revisions") or r.get("verify_revisions") or 0
    shutil.rmtree(d,ignore_errors=True)
    return {"ok":passed,"secs":secs,"agent":r.get("agent","?"),
            "status":r.get("status"),"verify_reported":(r.get("verify") or {}).get("passed"),
            "revisions":revisions}

def main():
    os.makedirs(WORKROOT,exist_ok=True)
    names=os.environ.get("JINDO_BENCH_CONFIGS")
    configs=[c for c in DEFAULT_CONFIGS if (not names or c["name"] in names.split(","))]
    cli=Client(); results=[]
    for task in TASKS:
        for cfg in configs:
            print(f"[run] {task['id']:16} x {cfg['name']:13} ...",flush=True)
            res=run_case(cli,task,cfg)
            print(f"      -> verified={res['ok']} secs={res['secs']} revisions={res.get('revisions')} status={res.get('status')}",flush=True)
            results.append({"task":task["id"],"task_type":task["task_type"],"difficulty":task["difficulty"],
                            "language":task["language"],"config":cfg["name"],**res})
    cli.close()
    open(os.path.join(REPO,"bench","results.json"),"w").write(json.dumps(results,indent=2,ensure_ascii=False))
    report(results)

def report(results):
    configs=[]
    for r in results:
        if r["config"] not in configs: configs.append(r["config"])
    lines=["# jindo benchmark — hard suite (codex-only)","",
           "CODEX-ONLY run (claude/agy tokens exhausted): measures whether JINDO's objective",
           "verify gate + self-heal (auto-revision) lifts a single agent's verified success on",
           "HARD tasks vs raw codex. Not a cross-agent routing comparison.","",
           "## KPI by config","","| config | verified pass | avg secs | avg revisions |","|---|---|---|---|"]
    rate={}
    for c in configs:
        rs=[r for r in results if r["config"]==c]; pv=sum(1 for r in rs if r["ok"]); n=len(rs)
        rate[c]=pv/n if n else 0
        lines.append(f"| {c} | {pv}/{n} ({round(100*pv/n) if n else 0}%) | {round(sum(r['secs'] for r in rs)/n,1) if n else 0} | {round(sum(r.get('revisions',0) for r in rs)/n,1) if n else 0} |")
    # per-task pass table
    tasks=[]
    for r in results:
        if r["task"] not in tasks: tasks.append(r["task"])
    lines+=["","## per-task verified pass","","| task | "+" | ".join(configs)+" |","|"+"---|"*(len(configs)+1)]
    for tk in tasks:
        row=[tk]
        for c in configs:
            m=[r for r in results if r["task"]==tk and r["config"]==c]
            row.append("✓" if (m and m[0]["ok"]) else "✗")
        lines.append("| "+" | ".join(row)+" |")
    if "codex_raw" in rate and "codex_verify" in rate:
        lift=round(100*(rate["codex_verify"]-rate["codex_raw"]))
        lines+=["",f"## JINDO lift (verify+self-heal vs raw): {lift:+d} percentage points",
                "Positive => the objective gate + auto-revision recovered failures a single raw dispatch missed."]
    md="\n".join(lines)
    open(os.path.join(REPO,"bench","report_hard.md"),"w").write(md+"\n")
    print("\n"+md,flush=True); print("\n== BENCH DONE ==",flush=True)

if __name__=="__main__": main()
