"""Cross-language code-generation fixtures with deterministic hidden oracles."""

TASKS = [
    {
        "id": "javascript_bounded_keyed_scheduler",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "javascript",
        "task_type": "bounded_keyed_scheduler",
        "public_files": {
            "package.json": '{"type":"module","scripts":{"test":"node --test"}}\n',
            "scheduler.js": '''export class KeyedScheduler {
  constructor(limit) { this.limit = limit; }
  run(key, fn, signal) { return Promise.resolve().then(fn); }
}
''',
            "scheduler.test.js": '''import test from "node:test";
import assert from "node:assert/strict";
import { KeyedScheduler } from "./scheduler.js";

test("returns task result", async () => {
  const scheduler = new KeyedScheduler(1);
  assert.equal(await scheduler.run("a", async () => 7), 7);
});
''',
        },
        "hidden_files": {
            "hidden.test.js": '''import test from "node:test";
import assert from "node:assert/strict";
import { KeyedScheduler } from "./scheduler.js";

const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return {promise, resolve}; };

test("enforces global bound, per-key FIFO, and cross-key concurrency", async () => {
  const s = new KeyedScheduler(2); let active = 0; let peak = 0; const order = [];
  const a = deferred(), b = deferred();
  const task = (name, gate) => async () => { active++; peak=Math.max(peak,active); order.push("start:"+name); await gate.promise; order.push("end:"+name); active--; return name; };
  const a1=s.run("a",task("a1",a)), a2=s.run("a",task("a2",b)), b1=s.run("b",task("b1",b)), c1=s.run("c",task("c1",b));
  await new Promise(r=>setImmediate(r)); assert.equal(peak,2); assert.deepEqual(order.slice(0,2),["start:a1","start:b1"]);
  b.resolve(); await new Promise(r=>setImmediate(r)); assert.equal(order.includes("start:a2"),false);
  a.resolve(); assert.deepEqual(await Promise.all([a1,a2,b1,c1]),["a1","a2","b1","c1"]); assert.ok(peak<=2);
});

test("queued abort rejects without invocation and does not block the key", async () => {
  const s=new KeyedScheduler(1), gate=deferred(); let calls=0;
  const first=s.run("x",async()=>{await gate.promise; return 1;});
  const controller=new AbortController();
  const aborted=s.run("x",async()=>{calls++; return 2;},controller.signal); controller.abort();
  await assert.rejects(aborted,e=>e.name==="AbortError"); gate.resolve(); assert.equal(await first,1);
  assert.equal(await s.run("x",async()=>3),3); assert.equal(calls,0);
});

test("invalid calls have no side effects and rejection releases capacity", async () => {
  assert.throws(()=>new KeyedScheduler(0),RangeError); const s=new KeyedScheduler(1); let calls=0;
  const c=new AbortController(); c.abort();
  await assert.rejects(s.run("a",async()=>{calls++;},c.signal),e=>e.name==="AbortError"); assert.equal(calls,0);
  await assert.rejects(s.run("a",async()=>{throw new Error("boom")}),/boom/);
  assert.equal(await s.run("b",async()=>9),9);
});
''',
        },
        "reference_files": {
            "scheduler.js": '''const abortError = () => { const error = new Error("aborted"); error.name = "AbortError"; return error; };

export class KeyedScheduler {
  constructor(limit) {
    if (!Number.isInteger(limit) || limit <= 0) throw new RangeError("limit must be positive");
    this.limit = limit; this.active = 0; this.busy = new Set(); this.pending = [];
  }
  run(key, fn, signal) {
    if (typeof fn !== "function") return Promise.reject(new TypeError("fn must be a function"));
    if (signal?.aborted) return Promise.reject(abortError());
    return new Promise((resolve, reject) => {
      const job = {key, fn, resolve, reject, cancelled:false};
      if (signal) {
        job.onAbort = () => { if (!job.started && !job.cancelled) { job.cancelled=true; reject(abortError()); this.#pump(); } };
        signal.addEventListener("abort", job.onAbort, {once:true}); job.signal=signal;
      }
      this.pending.push(job); this.#pump();
    });
  }
  #pump() {
    while (this.active < this.limit) {
      const index = this.pending.findIndex(job => !job.cancelled && !this.busy.has(job.key));
      if (index < 0) break;
      const [job] = this.pending.splice(index,1); job.started=true;
      if (job.signal) job.signal.removeEventListener("abort",job.onAbort);
      this.active++; this.busy.add(job.key);
      Promise.resolve().then(job.fn).then(job.resolve,job.reject).finally(()=>{ this.active--; this.busy.delete(job.key); this.#pump(); });
    }
    this.pending = this.pending.filter(job => !job.cancelled);
  }
}
''',
        },
        "prompt": "Repair KeyedScheduler without changing its exported class, constructor, or run(key, fn, signal) API. limit must be a positive integer. Across all keys at most limit functions may run. Functions for the same key start strictly FIFO and never overlap, while different keys may run concurrently. A signal already aborted, or aborted while its job is queued, rejects that job with an error whose name is AbortError and never invokes fn; it must not block later work. Once fn starts, its settlement owns the slot even if the caller's signal later aborts. Synchronous throws and async rejection propagate and always release capacity. Invalid calls must not invoke user code. Use no dependencies.",
        "verify": [["node", "--test"]],
    },
    {
        "id": "java_deterministic_dependency_planner",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "java",
        "task_type": "deterministic_dependency_planner",
        "public_files": {
            "Planner.java": '''import java.util.*;

public final class Planner {
    public static List<List<String>> plan(Map<String, Set<String>> dependencies) {
        return List.of(new ArrayList<>(dependencies.keySet()));
    }
}
''',
            "PlannerTest.java": '''import java.util.*;
public final class PlannerTest {
  public static void main(String[] args) {
    var graph = new HashMap<String,Set<String>>(); graph.put("build",Set.of("compile")); graph.put("compile",Set.of());
    if (!Planner.plan(graph).equals(List.of(List.of("compile"),List.of("build")))) throw new AssertionError();
  }
}
''',
        },
        "hidden_files": {
            "HiddenTest.java": '''import java.util.*;
public final class HiddenTest {
  static void eq(Object a,Object b){if(!Objects.equals(a,b))throw new AssertionError(a+" != "+b);}
  static void bad(Map<String,Set<String>> g){try{Planner.plan(g);throw new AssertionError("accepted invalid graph");}catch(IllegalArgumentException expected){}}
  public static void main(String[] args){
    var g=new LinkedHashMap<String,Set<String>>(); g.put("deploy",Set.of("package","test")); g.put("test",Set.of("compile")); g.put("package",Set.of("compile")); g.put("compile",Set.of()); g.put("docs",Set.of());
    var before=new LinkedHashMap<>(g); eq(Planner.plan(g),List.of(List.of("compile","docs"),List.of("package","test"),List.of("deploy"))); eq(g,before);
    var shuffled=new HashMap<String,Set<String>>(); shuffled.put("docs",Set.of()); shuffled.put("package",Set.of("compile")); shuffled.put("deploy",Set.of("test","package")); shuffled.put("compile",Set.of()); shuffled.put("test",Set.of("compile")); eq(Planner.plan(shuffled),Planner.plan(g));
    bad(Map.of("a",Set.of("missing"))); bad(Map.of("a",Set.of("a"))); bad(Map.of("a",Set.of("b"),"b",Set.of("c"),"c",Set.of("a")));
    eq(Planner.plan(Map.of()),List.of());
  }
}
''',
        },
        "reference_files": {
            "Planner.java": '''import java.util.*;

public final class Planner {
    public static List<List<String>> plan(Map<String, Set<String>> dependencies) {
        Objects.requireNonNull(dependencies, "dependencies");
        var indegree = new HashMap<String,Integer>();
        var outgoing = new HashMap<String,List<String>>();
        for (var node : dependencies.keySet()) { if (node == null) throw new IllegalArgumentException(); indegree.put(node,0); outgoing.put(node,new ArrayList<>()); }
        for (var entry : dependencies.entrySet()) {
            if (entry.getValue() == null) throw new IllegalArgumentException();
            for (var dependency : entry.getValue()) {
                if (!dependencies.containsKey(dependency) || dependency.equals(entry.getKey())) throw new IllegalArgumentException();
                indegree.put(entry.getKey(),indegree.get(entry.getKey())+1); outgoing.get(dependency).add(entry.getKey());
            }
        }
        var ready = new TreeSet<String>(); indegree.forEach((node,n)->{if(n==0)ready.add(node);});
        var result = new ArrayList<List<String>>(); int emitted=0;
        while (!ready.isEmpty()) {
            var wave = new ArrayList<>(ready); ready.clear(); result.add(List.copyOf(wave)); emitted += wave.size();
            for (var node : wave) for (var dependent : outgoing.get(node)) { int n=indegree.merge(dependent,-1,Integer::sum); if(n==0)ready.add(dependent); }
        }
        if (emitted != dependencies.size()) throw new IllegalArgumentException("cycle");
        return List.copyOf(result);
    }
}
''',
        },
        "prompt": "Implement Planner.plan without changing its public signature. The map key is a job and its set contains jobs that must finish first. Return maximal execution waves: every currently-ready job belongs to the same wave, waves respect all dependencies, and names inside every wave are lexicographically sorted. Output must be deterministic for HashMap/HashSet inputs, deeply independent of the input, and the input must not be mutated. Empty input returns an empty list. Null structures, missing dependencies, self-dependencies, and any direct or indirect cycle throw IllegalArgumentException. Use only the Java standard library.",
        "verify": [["javac", "-d", "out", "Planner.java", "PlannerTest.java", "HiddenTest.java"], ["java", "-cp", "out", "PlannerTest"], ["java", "-cp", "out", "HiddenTest"]],
    },
    {
        "id": "sql_bitemporal_ledger_report",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "sql",
        "task_type": "bitemporal_ledger_report",
        "public_files": {
            "query.sql": '''SELECT account_id, 0 AS balance, NULL AS last_effective_at FROM accounts ORDER BY account_id;
''',
            "README.md": '''Write query.sql for SQLite. Tables are accounts(account_id), report_params(as_of, generated_at), and ledger_events(event_id, account_id, effective_at, revision, ingested_at, delta, is_deleted).\n''',
        },
        "hidden_files": {
            "verify_query.py": '''import sqlite3
from pathlib import Path

db=sqlite3.connect(":memory:")
db.executescript("""
create table accounts(account_id text primary key);
create table report_params(as_of text not null, generated_at text not null);
create table ledger_events(event_id text, account_id text, effective_at text, revision integer, ingested_at text, delta integer, is_deleted integer);
insert into accounts values ('a'),('b'),('c'); insert into report_params values ('2026-01-31','2026-02-02');
insert into ledger_events values
 ('e1','a','2026-01-01',1,'2026-01-01',10,0),
 ('e1','a','2026-01-01',2,'2026-02-01',15,0),
 ('e1','a','2026-01-01',3,'2026-02-03',99,0),
 ('e2','a','2026-01-05',1,'2026-01-06',7,0),
 ('e2','a','2026-01-05',2,'2026-02-02',7,1),
 ('e3','b','2026-01-20',1,'2026-01-21',4,0),
 ('e4','b','2026-02-05',1,'2026-01-22',100,0),
 ('e5','b','2026-01-25',1,'2026-02-05',100,0);
""")
sql=Path("query.sql").read_text(); rows=db.execute(sql).fetchall()
assert rows == [('a',15,'2026-01-01'),('b',4,'2026-01-20'),('c',0,None)], rows
db.executescript("delete from report_params; insert into report_params values ('2026-01-04','2026-01-10');")
rows=db.execute(sql).fetchall(); assert rows == [('a',10,'2026-01-01'),('b',0,None),('c',0,None)], rows
print("ok")
''',
        },
        "reference_files": {
            "query.sql": '''WITH eligible AS (
  SELECT e.*, ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY revision DESC, ingested_at DESC) AS choice
  FROM ledger_events AS e CROSS JOIN report_params AS p
  WHERE e.effective_at <= p.as_of AND e.ingested_at <= p.generated_at
), current_events AS (
  SELECT account_id, effective_at, delta FROM eligible WHERE choice = 1 AND is_deleted = 0
), totals AS (
  SELECT account_id, SUM(delta) AS balance, MAX(effective_at) AS last_effective_at FROM current_events GROUP BY account_id
)
SELECT a.account_id, COALESCE(t.balance,0) AS balance, t.last_effective_at
FROM accounts AS a LEFT JOIN totals AS t ON t.account_id = a.account_id
ORDER BY a.account_id;
''',
        },
        "prompt": "Replace query.sql with one SQLite SELECT statement. report_params contains exactly one (as_of, generated_at) row. For each event_id, first discard versions whose effective_at is after as_of or whose ingested_at is after generated_at, then choose the remaining version with greatest revision (greatest ingested_at breaks a revision tie). A chosen is_deleted=1 version removes that logical event. Sum delta for chosen live events per account. Return every account exactly once as account_id, balance, last_effective_at; accounts without live events have integer balance 0 and NULL last_effective_at. last_effective_at is the maximum effective_at among contributing events. Sort by account_id. Do not alter schema or data.",
        "verify": [["python3", "verify_query.py"]],
    },
    {
        "id": "cpp_raii_reentrant_observer_registry",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "cpp",
        "task_type": "raii_reentrant_observer_registry",
        "public_files": {
            "registry.hpp": '''#pragma once
#include <functional>
#include <vector>

class Registry {
public:
    class Subscription {
    public:
        Subscription() = default;
        ~Subscription() = default;
    };
    Subscription subscribe(std::function<void(int)> callback) {
        callbacks_.push_back(std::move(callback)); return {};
    }
    void publish(int value) {
        for (auto& callback : callbacks_) callback(value);
    }
private:
    std::vector<std::function<void(int)>> callbacks_;
};
''',
            "registry_test.cpp": '''#include "registry.hpp"
#include <cassert>
int main(){ Registry r; int seen=0; auto s=r.subscribe([&](int n){seen+=n;}); r.publish(3); assert(seen==3); }
''',
        },
        "hidden_files": {
            "hidden_test.cpp": '''#include "registry.hpp"
#include <atomic>
#include <cassert>
#include <future>
#include <stdexcept>
#include <string>
#include <thread>
#include <chrono>
#include <vector>

int main(){
  Registry r; std::vector<std::string> log; Registry::Subscription first, added;
  first=r.subscribe([&](int){log.push_back("first"); first=Registry::Subscription{}; added=r.subscribe([&](int){log.push_back("added");});});
  auto second=r.subscribe([&](int){log.push_back("second");});
  r.publish(1); assert((log==std::vector<std::string>{"first","second"}));
  log.clear(); r.publish(2); assert((log==std::vector<std::string>{"second","added"}));

  int after_throw=0; auto bad=r.subscribe([](int){throw std::runtime_error("boom");}); auto tail=r.subscribe([&](int){after_throw++;});
  try { r.publish(3); assert(false); } catch(const std::runtime_error& e) { assert(std::string(e.what())=="boom"); }
  assert(after_throw==1);

  Registry::Subscription moved=std::move(tail); tail=Registry::Subscription{}; after_throw=0;
  try { r.publish(4); } catch(...) {} assert(after_throw==1); moved=Registry::Subscription{};
  try { r.publish(5); } catch(...) {} assert(after_throw==1);

  std::promise<void> entered, release; auto gate=release.get_future().share();
  auto blocker=r.subscribe([&](int){entered.set_value(); gate.wait();});
  std::thread publishing([&]{try{r.publish(6);}catch(...){}}); entered.get_future().wait();
  auto future=std::async(std::launch::async,[&]{return r.subscribe([](int){});});
  assert(future.wait_for(std::chrono::milliseconds(200))==std::future_status::ready);
  auto concurrent=std::move(future.get()); release.set_value(); publishing.join();

  Registry::Subscription survivor; { Registry temporary; survivor=temporary.subscribe([](int){}); }
  survivor=Registry::Subscription{};
}
''',
        },
        "reference_files": {
            "registry.hpp": '''#pragma once
#include <cstdint>
#include <exception>
#include <functional>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <utility>
#include <vector>

class Registry {
    struct Entry { std::uint64_t id; std::function<void(int)> callback; };
    struct State { std::mutex mutex; std::uint64_t next=1; std::vector<Entry> entries; };
public:
    class Subscription {
    public:
        Subscription() = default;
        Subscription(const Subscription&) = delete;
        Subscription& operator=(const Subscription&) = delete;
        Subscription(Subscription&& other) noexcept { move_from(other); }
        Subscription& operator=(Subscription&& other) noexcept { if(this!=&other){reset();move_from(other);} return *this; }
        ~Subscription(){reset();}
    private:
        friend class Registry;
        Subscription(std::weak_ptr<State> state,std::uint64_t id):state_(std::move(state)),id_(id){}
        void move_from(Subscription& other) noexcept { state_=std::move(other.state_);id_=std::exchange(other.id_,0); }
        void reset() noexcept {
            if(id_==0)return; if(auto state=state_.lock()){std::lock_guard lock(state->mutex);std::erase_if(state->entries,[&](const Entry& e){return e.id==id_;});} id_=0;state_.reset();
        }
        std::weak_ptr<State> state_; std::uint64_t id_=0;
    };
    Registry():state_(std::make_shared<State>()){}
    Subscription subscribe(std::function<void(int)> callback) {
        if(!callback)throw std::invalid_argument("callback"); std::lock_guard lock(state_->mutex);
        auto id=state_->next++; state_->entries.push_back(Entry{id,std::move(callback)}); return Subscription(state_,id);
    }
    void publish(int value) {
        std::vector<std::function<void(int)>> snapshot; {std::lock_guard lock(state_->mutex);for(auto& e:state_->entries)snapshot.push_back(e.callback);}
        std::exception_ptr first; for(auto& callback:snapshot){try{callback(value);}catch(...){if(!first)first=std::current_exception();}}
        if(first)std::rethrow_exception(first);
    }
private: std::shared_ptr<State> state_;
};
''',
        },
        "prompt": "Repair registry.hpp without changing the public Registry, Subscription, subscribe, or publish signatures. A live Subscription owns exactly one registration; it is move-only, move assignment first releases its old registration, and destruction unregisters safely even after Registry destruction. publish snapshots registrations present at its start and invokes them exactly once in registration order. Subscribe/unsubscribe during a callback affects only later publishes, so callbacks may unsubscribe themselves or add callbacks reentrantly. Never hold an internal lock while invoking a callback. If callbacks throw, invoke every callback in the snapshot and then rethrow the first exception. Concurrent subscribe, unsubscribe, publish, and subscription moves must not race or deadlock. Reject an empty std::function. Use only the C++20 standard library.",
        "verify": [["g++", "-std=c++20", "-pthread", "registry_test.cpp", "-o", "registry_test"], ["./registry_test"], ["g++", "-std=c++20", "-pthread", "hidden_test.cpp", "-o", "hidden_test"], ["./hidden_test"]],
    },
]

# Paired prompt-language transfer fixtures keep repository bytes and hidden
# oracles identical; only the natural-language instruction changes.
import copy


def _paired_variant(task_id, variant_id, prompt):
    base = next(task for task in TASKS if task["id"] == task_id)
    variant = copy.deepcopy(base)
    variant["id"] = variant_id
    variant["prompt"] = prompt
    variant["prompt_language"] = "korean"
    return variant


TASKS += [
    _paired_variant(
        "java_deterministic_dependency_planner",
        "java_deterministic_dependency_planner_ko",
        "Planner.plan의 공개 시그니처를 바꾸지 말고 구현하라. 맵의 키는 작업이고 값 집합은 먼저 끝나야 하는 작업들이다. 현재 실행 가능한 모든 작업을 하나의 최대 실행 웨이브로 묶어 반환하고, 모든 의존성을 지키며 각 웨이브 내부 이름은 사전순이어야 한다. HashMap/HashSet 입력에서도 결과가 결정적이어야 하고, 출력은 입력과 깊게 독립적이며 입력을 변경하면 안 된다. 빈 입력은 빈 리스트를 반환한다. null 구조, 존재하지 않는 의존성, 자기 의존성, 직접 또는 간접 순환은 IllegalArgumentException을 던진다. Java 표준 라이브러리만 사용하라.",
    ),
    _paired_variant(
        "sql_bitemporal_ledger_report",
        "sql_bitemporal_ledger_report_ko",
        "query.sql을 SQLite SELECT 문 하나로 교체하라. report_params에는 (as_of, generated_at) 행이 정확히 하나 있다. event_id별로 effective_at이 as_of보다 늦거나 ingested_at이 generated_at보다 늦은 버전을 먼저 제외하고, 남은 것 중 revision이 가장 큰 버전을 고른다. revision이 같으면 ingested_at이 큰 것을 고른다. 선택된 is_deleted=1 버전은 해당 논리 이벤트를 제거한다. 계정별 선택된 활성 이벤트의 delta를 합산한다. 모든 계정을 account_id, balance, last_effective_at으로 정확히 한 번 반환하고, 활성 이벤트가 없는 계정은 정수 balance 0과 NULL last_effective_at을 가져야 한다. last_effective_at은 기여 이벤트의 최대 effective_at이며 account_id 순으로 정렬한다. 스키마나 데이터를 변경하지 말라.",
    ),
]
