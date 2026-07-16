"""Hard fixtures spanning software-work purposes beyond greenfield coding."""

TASKS = [
    {
        "id": "go_http_retry_debugging",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "go",
        "task_type": "api_debugging_retry_semantics",
        "public_files": {
            "go.mod": "module retrybench\n\ngo 1.23\n",
            "retry/retry.go": r'''package retry

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Sleeper func(context.Context, time.Duration) error

func Do(ctx context.Context, client *http.Client, request *http.Request, maxAttempts int, sleep Sleeper) (*http.Response, error) {
	if maxAttempts < 1 { return nil, fmt.Errorf("maxAttempts must be positive") }
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		response, err := client.Do(request.WithContext(ctx))
		if err == nil { return response, nil }
		last = err
	}
	return nil, last
}
''',
            "retry/retry_test.go": r'''package retry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReturnsSuccessfulResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" { t.Fatalf("body=%q", body) }
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header), Request: r}, nil
	})}
	req, _ := http.NewRequest(http.MethodPost, "http://example.test", bytes.NewBufferString("payload"))
	response, err := Do(context.Background(), client, req, 2, nil)
	if err != nil || response.StatusCode != 200 { t.Fatalf("response=%v err=%v", response, err) }
	response.Body.Close()
}
''',
        },
        "hidden_files": {
            "retry/hidden_test.go": r'''package retry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type trackedBody struct { io.Reader; closed bool }
func (b *trackedBody) Close() error { b.closed = true; return nil }

func request() *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://example.test/upload", bytes.NewBufferString("payload"))
	r.Header.Set("X-Test", "kept")
	return r
}

func TestRetryableResponsesReplayBodyAndCloseDiscardedResponse(t *testing.T) {
	var attempts int; var bodies []string; var discarded *trackedBody
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++; body, _ := io.ReadAll(r.Body); bodies = append(bodies, string(body))
		if r.Header.Get("X-Test") != "kept" { t.Fatal("header lost") }
		if attempts == 1 {
			discarded = &trackedBody{Reader: strings.NewReader("retry")}
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: discarded, Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	response, err := Do(context.Background(), client, request(), 3, nil)
	if err != nil || response.StatusCode != 200 { t.Fatalf("response=%v err=%v", response, err) }
	defer response.Body.Close()
	if attempts != 2 || !reflect.DeepEqual(bodies, []string{"payload", "payload"}) || discarded == nil || !discarded.closed {
		t.Fatalf("attempts=%d bodies=%v discarded=%+v", attempts, bodies, discarded)
	}
}

func TestStatusPolicyRetryAfterAndFinalOwnership(t *testing.T) {
	statuses := []int{429, 502, 503, 504, 418}; var attempts int; var sleeps []time.Duration
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := statuses[attempts]; attempts++
		header := make(http.Header); if status == 429 { header.Set("Retry-After", "2") }
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("x")), Request: r}, nil
	})}
	sleep := func(ctx context.Context, duration time.Duration) error { sleeps = append(sleeps, duration); return nil }
	response, err := Do(context.Background(), client, request(), 5, sleep)
	if err != nil || response.StatusCode != 418 { t.Fatalf("response=%v err=%v", response, err) }
	response.Body.Close()
	if attempts != 5 || !reflect.DeepEqual(sleeps, []time.Duration{2*time.Second}) { t.Fatalf("attempts=%d sleeps=%v", attempts, sleeps) }
}

func TestTransportErrorsContextAndValidation(t *testing.T) {
	var attempts int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++; if attempts < 3 { return nil, errors.New("temporary") }
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	response, err := Do(context.Background(), client, request(), 3, nil)
	if err != nil || response.StatusCode != 204 || attempts != 3 { t.Fatalf("attempts=%d response=%v err=%v", attempts, response, err) }
	response.Body.Close()

	cancelled, cancel := context.WithCancel(context.Background()); cancel(); attempts = 0
	_, err = Do(cancelled, client, request(), 3, nil)
	if !errors.Is(err, context.Canceled) || attempts != 0 { t.Fatalf("attempts=%d err=%v", attempts, err) }
	if _, err := Do(context.Background(), nil, request(), 1, nil); err == nil { t.Fatal("accepted nil client") }
	if _, err := Do(context.Background(), client, nil, 1, nil); err == nil { t.Fatal("accepted nil request") }
	bad, _ := http.NewRequest(http.MethodPost, "http://example.test", io.NopCloser(strings.NewReader("x")))
	if _, err := Do(context.Background(), client, bad, 2, nil); err == nil { t.Fatal("accepted unreplayable body") }
}

func TestSleepCancellationStopsBeforeNextAttempt(t *testing.T) {
	var attempts int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++; h := make(http.Header); h.Set("Retry-After", "9")
		return &http.Response{StatusCode: 503, Header: h, Body: io.NopCloser(strings.NewReader("retry")), Request: r}, nil
	})}
	sleep := func(ctx context.Context, d time.Duration) error { return context.Canceled }
	_, err := Do(context.Background(), client, request(), 3, sleep)
	if !errors.Is(err, context.Canceled) || attempts != 1 { t.Fatalf("attempts=%d err=%v", attempts, err) }
}
''',
        },
        "reference_files": {
            "retry/retry.go": r'''package retry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Sleeper func(context.Context, time.Duration) error

func retryable(status int) bool { return status == 429 || status == 502 || status == 503 || status == 504 }

func Do(ctx context.Context, client *http.Client, request *http.Request, maxAttempts int, sleep Sleeper) (*http.Response, error) {
	if ctx == nil || client == nil || request == nil || maxAttempts < 1 { return nil, fmt.Errorf("invalid arguments") }
	if request.Body != nil && request.GetBody == nil { return nil, fmt.Errorf("request body is not replayable") }
	if err := ctx.Err(); err != nil { return nil, err }
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		clone := request.Clone(ctx)
		if request.Body != nil { body, err := request.GetBody(); if err != nil { return nil, err }; clone.Body = body }
		response, err := client.Do(clone)
		if err != nil {
			if ctx.Err() != nil { return nil, ctx.Err() }
			last = err
			continue
		}
		if !retryable(response.StatusCode) || attempt+1 == maxAttempts { return response, nil }
		io.Copy(io.Discard, response.Body); response.Body.Close()
		if value := strings.TrimSpace(response.Header.Get("Retry-After")); value != "" {
			seconds, err := strconv.ParseUint(value, 10, 31)
			if err == nil && seconds > 0 && sleep != nil {
				if err := sleep(ctx, time.Duration(seconds)*time.Second); err != nil { return nil, err }
			}
		}
	}
	if ctx.Err() != nil { return nil, ctx.Err() }
	return nil, last
}
''',
        },
        "prompt": "Debug retry.Do without changing its exported Sleeper type or Do signature. Validate nil context/client/request and maxAttempts. A non-nil request body must have GetBody so every attempt receives identical bytes without mutating the original request. Retry transport errors and only HTTP 429, 502, 503, and 504, up to maxAttempts. Drain and close every response that will be retried; return the final response open for the caller, including a final retryable response or any non-retryable status. Preserve method, URL, and headers. Before retrying, call the injected sleeper only for a positive integer Retry-After delta-seconds value; a sleeper error stops the operation. Context cancellation takes precedence and must prevent a new attempt. Use only the standard library.",
        "verify": [["go", "test", "./...", "-count=1"]],
    },
    {
        "id": "python_exact_weighted_allocator",
        "difficulty": "hard",
        "domain": "coding",
        "language": "python",
        "task_type": "numerical_exact_apportionment",
        "public_files": {
            "allocator.py": '''def allocate(total_cents, weights):
    total = float(sum(weights))
    return [round(total_cents * float(weight) / total) for weight in weights]
''',
            "test_allocator.py": '''import unittest
from decimal import Decimal
from allocator import allocate

class AllocatorTest(unittest.TestCase):
    def test_even(self):
        self.assertEqual(allocate(10, [Decimal("1"), Decimal("1")]), [5, 5])

if __name__ == "__main__": unittest.main()
''',
        },
        "hidden_files": {
            "test_hidden.py": '''import unittest
from decimal import Decimal
from allocator import allocate

class HiddenAllocatorTest(unittest.TestCase):
    def test_largest_remainder_and_stable_ties(self):
        self.assertEqual(allocate(10, [Decimal("1"), Decimal("1"), Decimal("1")]), [4,3,3])
        self.assertEqual(allocate(7, [Decimal("0.1"), Decimal("0.2"), Decimal("0.7")]), [1,1,5])
        self.assertEqual(allocate(2, [Decimal("1"), Decimal("1"), Decimal("1")]), [1,1,0])

    def test_negative_zero_and_huge_exact_values(self):
        self.assertEqual(allocate(-10, [Decimal("1"), Decimal("1"), Decimal("1")]), [-4,-3,-3])
        self.assertEqual(allocate(0, [Decimal("0"), Decimal("4")]), [0,0])
        huge = 10**80 + 7
        result = allocate(huge, [Decimal("1e-1000"), Decimal("2e-1000"), Decimal("3e-1000")])
        self.assertEqual(sum(result), huge); self.assertEqual(result, allocate(huge, [Decimal(1),Decimal(2),Decimal(3)]))

    def test_zero_weights_and_input_independence(self):
        weights=[Decimal("0"),Decimal("2"),Decimal("0"),Decimal("3")]; before=list(weights)
        result=allocate(11,weights)
        self.assertEqual(result,[0,4,0,7]); self.assertEqual(weights,before); self.assertTrue(all(type(x) is int for x in result))

    def test_rejects_invalid_values_without_coercion(self):
        invalid = [
            (True,[Decimal(1)]), (1,[]), (1,[Decimal(0),Decimal(0)]),
            (1,[Decimal(-1),Decimal(2)]), (1,[Decimal("NaN")]),
            (1,[Decimal("Infinity")]), (1,[1]), (1,[True]),
        ]
        for total, weights in invalid:
            with self.subTest(total=total,weights=weights):
                with self.assertRaises((TypeError,ValueError)): allocate(total,weights)

if __name__ == "__main__": unittest.main()
''',
        },
        "reference_files": {
            "allocator.py": '''from decimal import Decimal, localcontext

def allocate(total_cents, weights):
    if type(total_cents) is not int:
        raise TypeError("total_cents must be an int")
    values = list(weights)
    if not values:
        raise ValueError("weights must not be empty")
    if any(type(value) is not Decimal for value in values):
        raise TypeError("weights must be Decimal")
    if any(not value.is_finite() or value < 0 for value in values):
        raise ValueError("weights must be finite and nonnegative")
    denominator = sum(values, Decimal(0))
    if denominator == 0:
        if total_cents == 0: return [0] * len(values)
        raise ValueError("positive total weight required")
    magnitude = abs(total_cents)
    with localcontext() as context:
        digits = len(str(magnitude)) if magnitude else 1
        context.prec = max(50, digits + max((len(v.as_tuple().digits) for v in values), default=1) + 20)
        quotas = [Decimal(magnitude) * value / denominator for value in values]
        floors = [int(quota) for quota in quotas]
        remaining = magnitude - sum(floors)
        order = sorted(range(len(values)), key=lambda index: (-(quotas[index] - floors[index]), index))
    for index in order[:remaining]: floors[index] += 1
    sign = -1 if total_cents < 0 else 1
    return [sign * value for value in floors]
''',
        },
        "prompt": "Replace allocate(total_cents, weights) while preserving its signature. total_cents must be an exact int (bool is invalid). weights is a non-empty sequence of finite, nonnegative Decimal objects only and must not be mutated. Unless total_cents is zero, total weight must be positive; zero-weight entries receive zero. Allocate the signed integer cents proportionally using the largest-remainder method on the absolute magnitude: floor each exact quota, then give remaining cents to greatest fractional remainders with lower input index winning ties, finally restore the sign. Return plain ints whose sum is exactly total_cents. Support negative totals, extremely large integers, and arbitrarily small Decimal exponents without float conversion. Use only the standard library.",
        "verify": [["python3", "-m", "unittest", "-v"]],
    },
    {
        "id": "javascript_secure_archive_plan_v2",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "javascript",
        "task_type": "security_archive_path_validation",
        "public_files": {
            "package.json": "{\"type\":\"module\",\"scripts\":{\"test\":\"node --test\"}}\n",
            "archive.js": '''import path from "node:path";

export function planExtraction(root, entries) {
  return entries.map(entry => ({...entry, path: path.join(root, entry.name)}));
}
''',
            "archive.test.js": '''import test from "node:test";
import assert from "node:assert/strict";
import { planExtraction } from "./archive.js";

test("plans a normal file", () => {
  const plan=planExtraction("/safe/root",[{name:"docs/readme.txt",type:"file"}]);
  assert.equal(plan[0].path,"/safe/root/docs/readme.txt");
});
''',
        },
        "hidden_files": {
            "hidden.test.js": '''import test from "node:test";
import assert from "node:assert/strict";
import { planExtraction } from "./archive.js";

const bad = entries => assert.throws(() => planExtraction("/safe/root",entries), {name:"TypeError"});

test("accepts ordered safe entries and returns canonical plans", () => {
  assert.deepEqual(planExtraction("/safe/root",[
    {name:"docs",type:"dir"},
    {name:"docs/readme.txt",type:"file"},
    {name:"latest",type:"symlink",target:"docs/readme.txt"},
  ]),[
    {name:"docs",type:"dir",path:"/safe/root/docs"},
    {name:"docs/readme.txt",type:"file",path:"/safe/root/docs/readme.txt"},
    {name:"latest",type:"symlink",target:"docs/readme.txt",path:"/safe/root/latest",resolvedTarget:"/safe/root/docs/readme.txt"},
  ]);
});

test("rejects traversal absolute ambiguous and duplicate names", () => {
  for (const name of ["../x","a/../../x","/etc/passwd","a//b","a/./b","a"+String.fromCharCode(92)+"b","a"+String.fromCharCode(0)+"b",""]) bad([{name,type:"file"}]);
  bad([{name:"a",type:"file"},{name:"a",type:"dir"}]);
  bad([{name:"a",type:"weird"}]);
  bad([{name:"a/b",type:"file"},{name:"a",type:"file"}]);
});

test("rejects children beneath files or symlinks regardless of target", () => {
  bad([{name:"a",type:"file"},{name:"a/b",type:"file"}]);
  bad([{name:"link",type:"symlink",target:"target"},{name:"link/child",type:"file"}]);
  bad([{name:"dir",type:"dir"},{name:"dir/link",type:"symlink",target:"../other"},{name:"dir/link/x",type:"file"}]);
});

test("resolves relative symlink targets but rejects escape and ambiguous targets", () => {
  const plan=planExtraction("/safe/root",[{name:"a",type:"dir"},{name:"a/l",type:"symlink",target:"../b"}]);
  assert.equal(plan[1].resolvedTarget,"/safe/root/b");
  for (const target of ["../../escape","/absolute","a"+String.fromCharCode(92)+"b","x"+String.fromCharCode(0)+"y",""]) bad([{name:"a/l",type:"symlink",target}]);
  bad([{name:"x",type:"file",target:"ignored"}]);
});

test("does not mutate root or entry objects", () => {
  const entries=[Object.freeze({name:"a",type:"dir"})];
  const result=planExtraction("/safe/root/../root",Object.freeze(entries));
  assert.equal(result[0].path,"/safe/root/a"); assert.deepEqual(entries,[{name:"a",type:"dir"}]);
});

test("canonicalizes root and permits explicit upgrade of an implicit directory", () => {
  assert.equal(planExtraction("/safe/root/",[{name:"x",type:"file"}])[0].path,"/safe/root/x");
  assert.equal(planExtraction("/",[{name:"x",type:"file"}])[0].path,"/x");
  assert.doesNotThrow(()=>planExtraction("/safe/root",[{name:"a/b",type:"file"},{name:"a",type:"dir"}]));
  bad([{name:"a/l",type:"symlink",target:"."}]);
  bad([{name:"a/l",type:"symlink",target:"./x"}]);
});
''',
        },
        "reference_files": {
            "archive.js": '''import path from "node:path";

function segments(value, field) {
  if (typeof value !== "string" || value === "" || value.includes("\\0") || value.includes("\\\\") || value.startsWith("/")) throw new TypeError(`invalid ${field}`);
  const parts=value.split("/");
  if (parts.some(part => part === "" || part === "." || part === "..")) throw new TypeError(`invalid ${field}`);
  return parts;
}

function resolveTarget(base, parent, value) {
  if (typeof value !== "string" || value === "" || value.includes("\\0") || value.includes("\\\\") || value.startsWith("/")) throw new TypeError("invalid target");
  const stack=[...parent];
  for (const part of value.split("/")) {
    if (part === "" || part === ".") throw new TypeError("invalid target");
    if (part === "..") { if (stack.length === 0) throw new TypeError("escaping target"); stack.pop(); }
    else stack.push(part);
  }
  return path.join(base,...stack);
}

export function planExtraction(root, entries) {
  if (typeof root !== "string" || !path.isAbsolute(root) || !Array.isArray(entries)) throw new TypeError("invalid arguments");
  const base=path.resolve(root), kinds=new Map(), result=[];
  for (const entry of entries) {
    if (!entry || typeof entry !== "object") throw new TypeError("invalid entry");
    const parts=segments(entry.name,"name"), name=parts.join("/");
    if (!new Set(["file","dir","symlink"]).has(entry.type) || kinds.has(name)) throw new TypeError("invalid entry");
    if (entry.type !== "dir" && [...kinds.keys()].some(existing => existing.startsWith(name+"/"))) throw new TypeError("unsafe replacement of implicit directory");
    for (let i=1;i<parts.length;i++) {
      const parent=parts.slice(0,i).join("/"), kind=kinds.get(parent);
      if (kind && kind !== "dir") throw new TypeError("unsafe parent");
    }
    const planned={...entry,path:path.join(base,...parts)};
    if (entry.type === "symlink") {
      planned.resolvedTarget=resolveTarget(base,parts.slice(0,-1),entry.target);
    } else if (Object.prototype.hasOwnProperty.call(entry,"target")) throw new TypeError("target only valid for symlink");
    kinds.set(name,entry.type); result.push(planned);
  }
  return result;
}
''',
        },
        "prompt": "Implement planExtraction(root, entries) as a pure lexical archive planner without changing its export. The existing filesystem and pre-existing symlinks are intentionally out of scope; root is a trusted absolute POSIX path that must be lexically canonicalized, including trailing slashes and '/'. Each ordered entry has a POSIX name and type file, dir, or symlink. Names must be non-empty relative paths with no NUL, backslash, empty, dot, or dot-dot segment; canonical names must be unique. Missing parents are implicit directories and may later be declared as dir, but may never later become a file or symlink. No child may appear beneath an earlier file or symlink. Only symlinks have a non-empty relative target. A target may contain ordinary segments and '..', but empty and '.' segments, absolute paths, NUL, and backslash are invalid; apply '..' lexically from the symlink parent and reject escape above root. Targets need not exist. Return new entry objects with canonical absolute path, plus resolvedTarget for symlinks, without mutating inputs. Reject every invalid input with TypeError. Use only Node standard modules and do not access the filesystem.",
        "verify": [["node", "--test"]],
    },
    {
        "id": "java_multifile_optimistic_transfer",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "java",
        "task_type": "multifile_transaction_refactor",
        "public_files": {
            "Transfer.java": '''public record Transfer(String id, String from, String to, long amount) {}\n''',
            "LedgerStore.java": '''import java.util.*;

public interface LedgerStore {
    record Snapshot(long revision, Map<String,Long> balances, Map<String,Transfer> applied) {}
    Snapshot snapshot();
    boolean commit(long expectedRevision, Map<String,Long> balances, Map<String,Transfer> applied);
}
''',
            "InMemoryLedgerStore.java": '''import java.util.*;

public final class InMemoryLedgerStore implements LedgerStore {
    private long revision;
    private Map<String,Long> balances;
    private Map<String,Transfer> applied = new HashMap<>();
    public InMemoryLedgerStore(Map<String,Long> balances) { this.balances = balances; }
    public Snapshot snapshot() { return new Snapshot(revision, balances, applied); }
    public boolean commit(long expectedRevision, Map<String,Long> nextBalances, Map<String,Transfer> nextApplied) {
        balances = nextBalances; applied = nextApplied; revision++; return true;
    }
}
''',
            "TransferService.java": '''import java.util.*;

public final class TransferService {
    private final LedgerStore store;
    private final int maxAttempts;
    public TransferService(LedgerStore store, int maxAttempts) { this.store=store; this.maxAttempts=maxAttempts; }
    public long applyBatch(List<Transfer> transfers) {
        var snapshot=store.snapshot();
        var balances=new HashMap<>(snapshot.balances());
        for (var transfer:transfers) {
            balances.put(transfer.from(),balances.get(transfer.from())-transfer.amount());
            balances.put(transfer.to(),balances.get(transfer.to())+transfer.amount());
        }
        store.commit(snapshot.revision(),balances,snapshot.applied());
        return snapshot.revision()+1;
    }
}
''',
            "VisibleTest.java": '''import java.util.*;
public final class VisibleTest {
  public static void main(String[] args) {
    var store=new InMemoryLedgerStore(Map.of("a",10L,"b",0L));
    var service=new TransferService(store,2);
    if(service.applyBatch(List.of(new Transfer("t1","a","b",4)))!=1)throw new AssertionError();
    if(!store.snapshot().balances().equals(Map.of("a",6L,"b",4L)))throw new AssertionError();
  }
}
''',
        },
        "hidden_files": {
            "HiddenTest.java": '''import java.util.*;
import java.util.concurrent.*;

public final class HiddenTest {
  static void eq(Object a,Object b){if(!Objects.equals(a,b))throw new AssertionError(a+" != "+b);}
  static void bad(Runnable r){try{r.run();throw new AssertionError("accepted invalid batch");}catch(IllegalArgumentException expected){}}
  static final class ConflictOnce implements LedgerStore {
    final InMemoryLedgerStore delegate; int commits;
    ConflictOnce(Map<String,Long> values){delegate=new InMemoryLedgerStore(values);}
    public Snapshot snapshot(){return delegate.snapshot();}
    public boolean commit(long version,Map<String,Long>b,Map<String,Transfer>a){commits++;return commits>1&&delegate.commit(version,b,a);}
  }
  public static void main(String[] args)throws Exception{
    var input=new HashMap<String,Long>(Map.of("a",10L,"b",1L)); var store=new InMemoryLedgerStore(input); input.put("a",999L);
    eq(store.snapshot().balances(),Map.of("a",10L,"b",1L));
    try{store.snapshot().balances().put("x",1L);throw new AssertionError("mutable snapshot");}catch(UnsupportedOperationException expected){}

    var service=new TransferService(store,3); var before=store.snapshot();
    bad(()->service.applyBatch(List.of(new Transfer("bad","a","b",99)))); eq(store.snapshot(),before);
    bad(()->service.applyBatch(List.of(new Transfer("x","a","missing",1)))); eq(store.snapshot(),before);
    bad(()->service.applyBatch(List.of(new Transfer("x","a","a",1)))); eq(store.snapshot(),before);

    eq(service.applyBatch(List.of(new Transfer("t1","a","b",4))),1L);
    eq(service.applyBatch(List.of(new Transfer("t1","a","b",4))),1L);
    bad(()->service.applyBatch(List.of(new Transfer("t1","a","b",3))));
    eq(store.snapshot().balances(),Map.of("a",6L,"b",5L));

    var conflict=new ConflictOnce(Map.of("a",5L,"b",0L));
    eq(new TransferService(conflict,2).applyBatch(List.of(new Transfer("r","a","b",2))),1L); eq(conflict.commits,2);
    try{new TransferService(new ConflictOnce(Map.of("a",5L,"b",0L)),1).applyBatch(List.of(new Transfer("r","a","b",2)));throw new AssertionError();}catch(ConcurrentModificationException expected){}

    var overflow=new InMemoryLedgerStore(Map.of("a",1L,"b",Long.MAX_VALUE)); var os=new TransferService(overflow,1); var ob=overflow.snapshot();
    bad(()->os.applyBatch(List.of(new Transfer("o","a","b",1)))); eq(overflow.snapshot(),ob);

    var concurrent=new InMemoryLedgerStore(Map.of("a",10L,"b",0L,"c",0L));
    var gate=new CyclicBarrier(3); var pool=Executors.newFixedThreadPool(2); var futures=new ArrayList<Future<?>>();
    for(int i=0;i<2;i++){final int n=i;futures.add(pool.submit(()->{gate.await();new TransferService(concurrent,3).applyBatch(List.of(new Transfer("c"+n,"a",n==0?"b":"c",6)));return null;}));}
    gate.await(); int success=0; for(var f:futures){try{f.get();success++;}catch(ExecutionException e){if(!(e.getCause() instanceof IllegalArgumentException))throw e;}} pool.shutdown();
    eq(success,1); eq(concurrent.snapshot().balances().get("a"),4L);
  }
}
''',
        },
        "reference_files": {
            "InMemoryLedgerStore.java": '''import java.util.*;

public final class InMemoryLedgerStore implements LedgerStore {
    private long revision;
    private Map<String,Long> balances;
    private Map<String,Transfer> applied = new HashMap<>();
    public InMemoryLedgerStore(Map<String,Long> balances) { this.balances = new HashMap<>(Objects.requireNonNull(balances)); }
    public synchronized Snapshot snapshot() { return new Snapshot(revision, Map.copyOf(balances), Map.copyOf(applied)); }
    public synchronized boolean commit(long expectedRevision, Map<String,Long> nextBalances, Map<String,Transfer> nextApplied) {
        if (revision != expectedRevision) return false;
        balances = new HashMap<>(nextBalances); applied = new HashMap<>(nextApplied); revision++; return true;
    }
}
''',
            "TransferService.java": '''import java.util.*;

public final class TransferService {
    private final LedgerStore store;
    private final int maxAttempts;
    public TransferService(LedgerStore store, int maxAttempts) {
        this.store=Objects.requireNonNull(store); if(maxAttempts<1)throw new IllegalArgumentException(); this.maxAttempts=maxAttempts;
    }
    public long applyBatch(List<Transfer> transfers) {
        Objects.requireNonNull(transfers);
        for(int attempt=0;attempt<maxAttempts;attempt++){
            var snapshot=store.snapshot(); var balances=new HashMap<>(snapshot.balances()); var applied=new HashMap<>(snapshot.applied()); boolean changed=false;
            for(var transfer:transfers){
                if(transfer==null||transfer.id()==null||transfer.id().isEmpty()||transfer.from()==null||transfer.to()==null||transfer.from().equals(transfer.to())||transfer.amount()<=0)throw new IllegalArgumentException();
                var previous=applied.get(transfer.id()); if(previous!=null){if(!previous.equals(transfer))throw new IllegalArgumentException();continue;}
                Long from=balances.get(transfer.from()); Long to=balances.get(transfer.to()); if(from==null||to==null||from<transfer.amount())throw new IllegalArgumentException();
                long nextTo; try{nextTo=Math.addExact(to,transfer.amount());}catch(ArithmeticException e){throw new IllegalArgumentException(e);}
                balances.put(transfer.from(),from-transfer.amount());balances.put(transfer.to(),nextTo);applied.put(transfer.id(),transfer);changed=true;
            }
            if(!changed)return snapshot.revision();
            if(store.commit(snapshot.revision(),balances,applied))return snapshot.revision()+1;
        }
        throw new ConcurrentModificationException();
    }
}
''',
        },
        "prompt": "Repair the four-file transfer component without changing any public type, record component, constructor, or method signature. InMemoryLedgerStore must own its input state, return deeply immutable snapshots, and commit atomically only when expectedRevision matches; a successful commit copies inputs and increments revision once. TransferService validates maxAttempts and applies a batch optimistically with up to that many commit attempts. Each transfer needs a non-empty id, distinct existing accounts, and positive amount; reject insufficient funds and checked-arithmetic overflow. The batch is atomic. Replaying an already-applied identical transfer is a no-op, while reusing its id with different fields is invalid, including duplicates inside one batch. If all transfers are replays or the batch is empty, do not commit. Retry only commit conflicts from a fresh snapshot; after exhaustion throw ConcurrentModificationException. Do not mutate caller collections. Use only the Java standard library.",
        "verify": [["javac", "-d", "out", "Transfer.java", "LedgerStore.java", "InMemoryLedgerStore.java", "TransferService.java", "VisibleTest.java", "HiddenTest.java"], ["java", "-cp", "out", "VisibleTest"], ["java", "-cp", "out", "HiddenTest"]],
    },
]
