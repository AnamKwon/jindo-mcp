"""Mutation-scored fixtures for test-generation and verification capability."""

PY_LEDGER = '''from decimal import Decimal

def reconcile(records):
    if type(records) is not list:
        raise TypeError("records must be a list")
    latest = {}
    seen = {}
    for record in records:
        if type(record) is not dict or set(record) != {"id", "version", "deleted", "amount", "tags"}:
            raise TypeError("invalid record shape")
        record_id = record["id"]
        version = record["version"]
        deleted = record["deleted"]
        amount = record["amount"]
        tags = record["tags"]
        if type(record_id) is not str or not record_id:
            raise TypeError("invalid id")
        if type(version) is not int or version < 0:
            raise TypeError("invalid version")
        if type(deleted) is not bool:
            raise TypeError("invalid deleted")
        if type(amount) is not Decimal or not amount.is_finite():
            raise TypeError("invalid amount")
        if type(tags) is not list or any(type(tag) is not str for tag in tags):
            raise TypeError("invalid tags")
        canonical = (deleted, amount, tuple(tags))
        key = (record_id, version)
        if key in seen:
            if seen[key] != canonical:
                raise ValueError("conflicting revision")
            continue
        seen[key] = canonical
        current = latest.get(record_id)
        if current is None or version > current[0]:
            latest[record_id] = (version, deleted, amount, tuple(tags))
    return tuple(
        (record_id, amount, tags)
        for record_id, (_, deleted, amount, tags) in sorted(latest.items())
        if not deleted
    )
'''

GO_TABLE = '''package lease

import (
    "sync"
    "time"
)

type Lease struct {
    Owner string
    Token uint64
    Expires time.Time
}

type Table struct {
    mu sync.Mutex
    leases map[string]Lease
}

func NewTable() *Table { return &Table{leases: make(map[string]Lease)} }

func (t *Table) Acquire(key, owner string, token uint64, now, expires time.Time) bool {
    if key == "" || owner == "" || token == 0 || !expires.After(now) { return false }
    t.mu.Lock()
    defer t.mu.Unlock()
    if current, ok := t.leases[key]; ok {
        if token <= current.Token { return false }
        if now.Before(current.Expires) && owner != current.Owner { return false }
    }
    t.leases[key] = Lease{Owner: owner, Token: token, Expires: expires}
    return true
}

func (t *Table) Release(key, owner string, token uint64) bool {
    t.mu.Lock()
    defer t.mu.Unlock()
    current, ok := t.leases[key]
    if !ok || current.Owner != owner || current.Token != token { return false }
    delete(t.leases, key)
    return true
}

func (t *Table) Snapshot() map[string]Lease {
    t.mu.Lock()
    defer t.mu.Unlock()
    out := make(map[string]Lease, len(t.leases))
    for key, lease := range t.leases { out[key] = lease }
    return out
}
'''

TASKS = [
    {
        "id": "python_contract_mutation_test_generation",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "python",
        "task_type": "contract_mutation_test_generation",
        "public_files": {
            "ledger.py": PY_LEDGER,
            "test_ledger.py": '''import unittest
from decimal import Decimal
from ledger import reconcile

class LedgerTests(unittest.TestCase):
    def test_one_record(self):
        records = [{"id":"a", "version":1, "deleted":False, "amount":Decimal("1.2"), "tags":["x"]}]
        self.assertEqual(reconcile(records), (("a", Decimal("1.2"), ("x",)),))

if __name__ == "__main__": unittest.main()
''',
        },
        "hidden_files": {
            "evaluate_tests.py": '''import ast, pathlib, subprocess, sys

REFERENCE = %r
TEST = pathlib.Path("test_ledger.py")
PRODUCT = pathlib.Path("ledger.py")

def run_tests():
    return subprocess.run([sys.executable, "-m", "unittest", "-q", "test_ledger.py"],
                          text=True, capture_output=True, timeout=20).returncode

assert PRODUCT.read_text() == REFERENCE, "production code changed"
changed = subprocess.run(["git", "diff", "--name-only", "HEAD"], text=True, capture_output=True, check=True).stdout.splitlines()
assert changed == ["test_ledger.py"], "only test_ledger.py may change"
allowed = {"unittest", "decimal", "ledger"}
tree = ast.parse(TEST.read_text())
for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        assert all(alias.name.split(".")[0] in allowed for alias in node.names), "behavioral tests only"
    if isinstance(node, ast.ImportFrom):
        assert (node.module or "").split(".")[0] in allowed, "behavioral tests only"
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
        assert node.func.id not in {"open", "exec", "eval", "compile", "__import__"}, "behavioral tests only"

assert run_tests() == 0, "tests fail on the correct implementation"
mutants = [
    ("bool_version", "type(version) is not int", "not isinstance(version, int)"),
    ("conflict_ignored", "raise ValueError(\\"conflicting revision\\")", "continue"),
    ("oldest_wins", "version > current[0]", "version < current[0]"),
    ("deleted_survives", "if not deleted", "if deleted"),
    ("insertion_order", "sorted(latest.items())", "latest.items()"),
    ("float_roundtrip", "canonical = (deleted, amount, tuple(tags))", "amount = Decimal(str(float(amount)))\\n        canonical = (deleted, amount, tuple(tags))"),
]
try:
    for name, old, new in mutants:
        mutant = REFERENCE.replace(old, new)
        assert mutant != REFERENCE, name
        PRODUCT.write_text(mutant)
        assert run_tests() != 0, "survived mutant: " + name
finally:
    PRODUCT.write_text(REFERENCE)
''' % PY_LEDGER,
        },
        "reference_files": {
            "test_ledger.py": '''import unittest
from decimal import Decimal
from ledger import reconcile

def row(i, v, deleted=False, amount="1", tags=None):
    return {"id":i, "version":v, "deleted":deleted, "amount":Decimal(amount), "tags":list(tags or [])}

class LedgerTests(unittest.TestCase):
    def test_latest_revision_and_delete(self):
        self.assertEqual(reconcile([row("a",2,amount="2"), row("a",1,amount="1"), row("b",3,True)]),
                         (("a", Decimal("2"), ()),))
    def test_conflicting_and_identical_duplicate(self):
        same=row("a",1,tags=["x"])
        self.assertEqual(reconcile([same, dict(same)]), (("a",Decimal("1"),("x",)),))
        with self.assertRaises(ValueError): reconcile([same, row("a",1,amount="2",tags=["x"])])
    def test_exact_decimal_and_sorted_ids(self):
        precise="0.12345678901234567890123456789"
        self.assertEqual(reconcile([row("z",1),row("a",1,amount=precise)]),
                         (("a",Decimal(precise),()),("z",Decimal("1"),())))
    def test_strict_version_type(self):
        with self.assertRaises(TypeError): reconcile([row("a",True)])
    def test_inputs_are_not_mutated(self):
        records=[row("a",1,tags=["x"])]; before=repr(records); reconcile(records); self.assertEqual(repr(records),before)

if __name__ == "__main__": unittest.main()
''',
        },
        "prompt": "Improve test_ledger.py only; do not change ledger.py or add files. Write behavioral unittest tests for reconcile's full public contract. Records must be a list of exact dicts with exactly id/version/deleted/amount/tags; id is nonempty str, version is a nonnegative exact int (bool invalid), deleted exact bool, amount a finite Decimal, and tags a list of strs. Duplicate id+version records are allowed only when deleted/amount/tags are identical, otherwise ValueError. The highest version wins regardless of input order; a latest deleted revision removes the id. Return immutable (id, amount, tags-tuple) rows sorted by id, preserve Decimal exactly without float conversion, and never mutate inputs. Tests must pass the correct implementation while detecting subtle contract mutations. Use only unittest and the standard library, and do not inspect source text or invoke subprocesses.",
        "verify": [["python3", "evaluate_tests.py"]],
    },
    {
        "id": "go_concurrency_fencing_test_generation",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "go",
        "task_type": "concurrency_fencing_test_generation",
        "public_files": {
            "go.mod": "module verificationbench\n\ngo 1.23\n",
            "lease/table.go": GO_TABLE,
            "lease/table_test.go": '''package lease

import (
    "testing"
    "time"
)

func TestAcquire(t *testing.T) {
    table := NewTable(); now := time.Unix(100, 0)
    if !table.Acquire("k", "a", 1, now, now.Add(time.Second)) { t.Fatal("acquire failed") }
}
''',
        },
        "hidden_files": {
            "evaluate_tests.py": '''import pathlib, subprocess

REFERENCE = %r
PRODUCT = pathlib.Path("lease/table.go")
TEST = pathlib.Path("lease/table_test.go")

def run_tests():
    return subprocess.run(["go", "test", "-race", "./...", "-count=1"],
                          text=True, capture_output=True, timeout=45).returncode

assert PRODUCT.read_text() == REFERENCE, "production code changed"
changed = subprocess.run(["git", "diff", "--name-only", "HEAD"], text=True, capture_output=True, check=True).stdout.splitlines()
assert changed == ["lease/table_test.go"], "only lease/table_test.go may change"
text = TEST.read_text()
for forbidden in ("os.ReadFile", "ioutil.ReadFile", "runtime.Caller", "debug.ReadBuildInfo", "os/exec"):
    assert forbidden not in text, "behavioral tests only"
assert run_tests() == 0, "tests fail on the correct implementation"
mutants = [
    ("equal_token", "token <= current.Token", "token < current.Token"),
    ("active_owner_ignored", "if now.Before(current.Expires) && owner != current.Owner { return false }", "if false { return false }"),
    ("zero_duration", "!expires.After(now)", "expires.Before(now)"),
    ("release_owner_ignored", "current.Owner != owner || current.Token != token", "current.Token != token"),
    ("release_token_ignored", "current.Owner != owner || current.Token != token", "current.Owner != owner"),
    ("snapshot_alias", "out := make(map[string]Lease, len(t.leases))\\n    for key, lease := range t.leases { out[key] = lease }\\n    return out", "return t.leases"),
    ("snapshot_value_lost", "for key, lease := range t.leases { out[key] = lease }", "for key := range t.leases { out[key] = Lease{} }"),
    ("locks_removed", "    t.mu.Lock()\\n    defer t.mu.Unlock()\\n", ""),
]
try:
    for name, old, new in mutants:
        mutant = REFERENCE.replace(old, new)
        assert mutant != REFERENCE, name
        PRODUCT.write_text(mutant)
        assert run_tests() != 0, "survived mutant: " + name
finally:
    PRODUCT.write_text(REFERENCE)
''' % GO_TABLE,
        },
        "reference_files": {
            "lease/table_test.go": '''package lease

import (
    "sync"
    "testing"
    "time"
)

func TestValidationFencingAndOwnership(t *testing.T) {
    table := NewTable(); now := time.Unix(100, 0); later := now.Add(time.Second)
    if table.Acquire("k","a",0,now,later) || table.Acquire("k","a",1,now,now) { t.Fatal("invalid acquire") }
    if !table.Acquire("k","a",1,now,later) { t.Fatal("initial") }
    if table.Acquire("k","a",1,now,later) || table.Acquire("k","b",2,now,later) { t.Fatal("fence/owner") }
    if !table.Acquire("k","b",2,later,later.Add(time.Second)) { t.Fatal("expired takeover") }
    if table.Release("k","a",2) || table.Release("k","b",1) || !table.Release("k","b",2) { t.Fatal("release identity") }
}

func TestSnapshotIsIndependent(t *testing.T) {
    table:=NewTable(); now:=time.Unix(1,0); table.Acquire("k","a",1,now,now.Add(time.Second))
    snap:=table.Snapshot()
    if snap["k"].Owner != "a" || snap["k"].Token != 1 || !snap["k"].Expires.Equal(now.Add(time.Second)) { t.Fatal("snapshot value") }
    delete(snap,"k")
    if _, ok:=table.Snapshot()["k"]; !ok { t.Fatal("snapshot aliased") }
}

func TestConcurrentAcquireIsRaceFree(t *testing.T) {
    table:=NewTable(); now:=time.Unix(1,0); const workers=64
    var wg sync.WaitGroup; wg.Add(workers)
    for i:=1; i<=workers; i++ { go func(token uint64) { defer wg.Done(); table.Acquire("k","a",token,now,now.Add(time.Second)) }(uint64(i)) }
    wg.Wait(); if table.Snapshot()["k"].Token == 0 { t.Fatal("missing lease") }
}
''',
        },
        "prompt": "Improve lease/table_test.go only; do not change table.go or add files. Write behavioral Go tests for the concurrent lease table. Acquire rejects empty key/owner, zero token, and expires<=now. While a lease exists, tokens must strictly increase relative to that lease. A successful Release removes it completely, so a later Acquire starts a fresh token sequence. An unexpired lease may only be renewed by its owner; after expiry a different owner may take over with a higher token. Release requires the exact owner and token. Snapshot must be an independent map preserving every Lease value exactly. All operations must be race-free under concurrent Acquire, Release, and Snapshot calls. Tests must pass the correct implementation, detect subtle fencing/ownership/value-copy/aliasing/locking mutations, and run reliably with go test -race. Do not inspect production source text or spawn subprocesses.",
        "verify": [["python3", "evaluate_tests.py"]],
        "verify_timeout": 420,
    },
]
