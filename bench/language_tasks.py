"""Hard language-native fixtures for capability routing calibration."""

TASKS = [
    {
        "id": "python_async_singleflight_cache",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "python",
        "task_type": "async_singleflight_cache",
        "public_files": {
            "cache.py": '''import asyncio


class AsyncCache:
    def __init__(self, clock):
        self._clock = clock
        self._values = {}

    async def get(self, key, ttl, loader):
        if key in self._values:
            value, expires = self._values[key]
            if self._clock() < expires:
                return value
        value = await loader()
        self._values[key] = (value, self._clock() + ttl)
        return value

    async def invalidate(self, key):
        self._values.pop(key, None)
''',
            "test_cache.py": '''import asyncio
import unittest
from cache import AsyncCache


class Clock:
    def __init__(self): self.now = 0.0
    def __call__(self): return self.now


class CacheTests(unittest.IsolatedAsyncioTestCase):
    async def test_hit_and_expiry(self):
        clock = Clock(); cache = AsyncCache(clock); calls = 0
        async def load():
            nonlocal calls; calls += 1; return calls
        self.assertEqual(await cache.get("k", 2, load), 1)
        self.assertEqual(await cache.get("k", 2, load), 1)
        clock.now = 3
        self.assertEqual(await cache.get("k", 2, load), 2)


if __name__ == "__main__": unittest.main()
''',
        },
        "hidden_files": {
            "test_hidden.py": '''import asyncio
import unittest
from cache import AsyncCache
from test_cache import Clock


class HiddenCacheTests(unittest.IsolatedAsyncioTestCase):
    async def test_singleflight_and_cancelled_waiter(self):
        clock = Clock(); cache = AsyncCache(clock); started = asyncio.Event(); release = asyncio.Event(); calls = 0
        async def load():
            nonlocal calls; calls += 1; started.set(); await release.wait(); return 7
        first = asyncio.create_task(cache.get("k", 10, load)); await started.wait()
        second = asyncio.create_task(cache.get("k", 10, load)); await asyncio.sleep(0)
        first.cancel()
        with self.assertRaises(asyncio.CancelledError): await first
        release.set()
        self.assertEqual(await second, 7); self.assertEqual(calls, 1)
        self.assertEqual(await cache.get("k", 10, load), 7)

    async def test_failure_shared_but_not_cached(self):
        clock = Clock(); cache = AsyncCache(clock); release = asyncio.Event(); calls = 0
        async def bad():
            nonlocal calls; calls += 1; await release.wait(); raise RuntimeError("boom")
        a = asyncio.create_task(cache.get("k", 10, bad)); b = asyncio.create_task(cache.get("k", 10, bad))
        await asyncio.sleep(0); release.set()
        for task in (a, b):
            with self.assertRaisesRegex(RuntimeError, "boom"): await task
        self.assertEqual(calls, 1)
        async def good(): return 9
        self.assertEqual(await cache.get("k", 10, good), 9)

    async def test_invalidate_blocks_stale_repopulation(self):
        clock = Clock(); cache = AsyncCache(clock); started = asyncio.Event(); release = asyncio.Event(); calls = 0
        async def old(): started.set(); await release.wait(); return "old"
        pending = asyncio.create_task(cache.get("k", 10, old)); await started.wait()
        await cache.invalidate("k"); release.set(); self.assertEqual(await pending, "old")
        async def fresh():
            nonlocal calls; calls += 1; return "fresh"
        self.assertEqual(await cache.get("k", 10, fresh), "fresh"); self.assertEqual(calls, 1)

    async def test_invalid_ttl_and_different_keys(self):
        clock = Clock(); cache = AsyncCache(clock); calls = 0
        async def never():
            nonlocal calls; calls += 1; return 1
        for ttl in (0, -1):
            with self.assertRaises(ValueError): await cache.get("bad", ttl, never)
        self.assertEqual(calls, 0)
        active = 0; peak = 0; release = asyncio.Event()
        async def load():
            nonlocal active, peak; active += 1; peak = max(peak, active); await release.wait(); active -= 1; return 1
        a = asyncio.create_task(cache.get("a", 1, load)); b = asyncio.create_task(cache.get("b", 1, load))
        for _ in range(20):
            if peak == 2: break
            await asyncio.sleep(0)
        release.set(); await asyncio.gather(a, b); self.assertEqual(peak, 2)


if __name__ == "__main__": unittest.main()
''',
        },
        "reference_files": {
            "cache.py": '''import asyncio


class AsyncCache:
    def __init__(self, clock):
        self._clock = clock
        self._values = {}
        self._inflight = {}
        self._generation = {}
        self._lock = asyncio.Lock()

    async def get(self, key, ttl, loader):
        if ttl <= 0:
            raise ValueError("ttl must be positive")
        async with self._lock:
            cached = self._values.get(key)
            if cached is not None:
                value, expires = cached
                if self._clock() < expires:
                    return value
                self._values.pop(key, None)
            task = self._inflight.get(key)
            if task is None:
                generation = self._generation.get(key, 0)
                task = asyncio.create_task(self._load(key, generation, ttl, loader))
                self._inflight[key] = task
        return await asyncio.shield(task)

    async def _load(self, key, generation, ttl, loader):
        task = asyncio.current_task()
        try:
            value = await loader()
        except BaseException:
            async with self._lock:
                if self._inflight.get(key) is task:
                    self._inflight.pop(key, None)
            raise
        async with self._lock:
            if self._generation.get(key, 0) == generation:
                self._values[key] = (value, self._clock() + ttl)
            if self._inflight.get(key) is task:
                self._inflight.pop(key, None)
        return value

    async def invalidate(self, key):
        async with self._lock:
            self._generation[key] = self._generation.get(key, 0) + 1
            self._values.pop(key, None)
''',
        },
        "prompt": "Repair AsyncCache without changing its public class or get/invalidate signatures. ttl must be positive and invalid calls must not invoke loader. Concurrent get calls for the same uncached key share exactly one loader task; different keys remain independent. Cancelling one waiter must not cancel the shared load for other waiters. Loader failure is observed by all current waiters but is never cached, so a later call retries. Expiry is ttl after successful loader completion using only the injected clock. invalidate removes a cached value and advances the key generation: an already-running old load may still return to its current waiters but must never repopulate the cache after invalidation. Do not hold a lock while awaiting user loader code. Use only the standard library.",
        "verify": [["python3", "-m", "unittest", "-v"]],
    },
    {
        "id": "rust_optimistic_atomic_store",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "rust",
        "task_type": "optimistic_atomic_store",
        "public_files": {
            "Cargo.toml": '''[package]
name = "optimistic-store"
version = "0.1.0"
edition = "2021"
''',
            "src/lib.rs": '''use std::collections::BTreeMap;
use std::sync::RwLock;

#[derive(Clone, Debug)]
pub enum Op { Put(String, i64), Delete(String), Transfer(String, String, i64) }

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Snapshot { pub version: u64, pub values: BTreeMap<String, i64> }

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CommitError { Conflict, Invalid }

struct State { version: u64, values: BTreeMap<String, i64> }
pub struct Store { state: RwLock<State> }

impl Store {
    pub fn new(values: BTreeMap<String, i64>) -> Self { Self { state: RwLock::new(State { version: 0, values }) } }
    pub fn snapshot(&self) -> Snapshot { let s = self.state.read().unwrap(); Snapshot { version: s.version, values: s.values.clone() } }
    pub fn commit(&self, base_version: u64, ops: &[Op]) -> Result<u64, CommitError> {
        let mut s = self.state.write().unwrap();
        if s.version != base_version { return Err(CommitError::Conflict); }
        for op in ops {
            match op {
                Op::Put(k, v) => { s.values.insert(k.clone(), *v); }
                Op::Delete(k) => { s.values.remove(k); }
                Op::Transfer(from, to, amount) => { *s.values.entry(from.clone()).or_default() -= amount; *s.values.entry(to.clone()).or_default() += amount; }
            }
        }
        s.version += 1; Ok(s.version)
    }
}
''',
            "tests/basic.rs": '''use optimistic_store::{Op, Store};
use std::collections::BTreeMap;
#[test]
fn basic_commit() {
    let s = Store::new(BTreeMap::from([("a".into(), 10), ("b".into(), 0)]));
    let v = s.commit(0, &[Op::Transfer("a".into(), "b".into(), 4)]).unwrap();
    let snap = s.snapshot(); assert_eq!(v, 1); assert_eq!(snap.values["a"], 6); assert_eq!(snap.values["b"], 4);
}
''',
        },
        "hidden_files": {
            "tests/hidden.rs": '''use optimistic_store::{CommitError, Op, Store};
use std::collections::BTreeMap;
use std::sync::{Arc, Barrier};
use std::thread;

fn store() -> Store { Store::new(BTreeMap::from([("a".into(), 10), ("b".into(), 2)])) }

#[test]
fn rollback_on_invalid_or_overflow() {
    let s = store(); let before = s.snapshot();
    let bad = [Op::Put("x".into(), 5), Op::Transfer("a".into(), "b".into(), 99)];
    assert_eq!(s.commit(0, &bad), Err(CommitError::Invalid)); assert_eq!(s.snapshot(), before);
    let overflow = [Op::Put("x".into(), i64::MAX), Op::Transfer("a".into(), "x".into(), 1)];
    assert_eq!(s.commit(0, &overflow), Err(CommitError::Invalid)); assert_eq!(s.snapshot(), before);
    for op in [Op::Put("x".into(), -1), Op::Transfer("a".into(), "a".into(), 1), Op::Transfer("a".into(), "b".into(), 0)] {
        assert_eq!(s.commit(0, &[op]), Err(CommitError::Invalid)); assert_eq!(s.snapshot(), before);
    }
}

#[test]
fn empty_commit_and_conflict_semantics() {
    let s = store(); assert_eq!(s.commit(0, &[]), Ok(0));
    assert_eq!(s.commit(0, &[Op::Delete("missing".into())]), Err(CommitError::Invalid));
    assert_eq!(s.commit(0, &[Op::Put("x".into(), 1)]), Ok(1));
    let after = s.snapshot(); assert_eq!(s.commit(0, &[Op::Put("y".into(), 1)]), Err(CommitError::Conflict)); assert_eq!(s.snapshot(), after);
}

#[test]
fn snapshot_isolation_and_one_concurrent_winner() {
    let s = Arc::new(store()); let mut old = s.snapshot(); old.values.insert("a".into(), 999); assert_eq!(s.snapshot().values["a"], 10);
    let barrier = Arc::new(Barrier::new(3)); let mut handles = vec![];
    for n in 0..2 { let s=Arc::clone(&s); let b=Arc::clone(&barrier); handles.push(thread::spawn(move || { b.wait(); s.commit(0, &[Op::Put(format!("x{n}"), 1)]) })); }
    barrier.wait(); let results: Vec<_> = handles.into_iter().map(|h|h.join().unwrap()).collect();
    assert_eq!(results.iter().filter(|r|r.is_ok()).count(), 1); assert_eq!(results.iter().filter(|r|**r==Err(CommitError::Conflict)).count(), 1); assert_eq!(s.snapshot().version, 1);
}
''',
        },
        "reference_files": {
            "src/lib.rs": '''use std::collections::BTreeMap;
use std::sync::RwLock;

#[derive(Clone, Debug)]
pub enum Op { Put(String, i64), Delete(String), Transfer(String, String, i64) }
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Snapshot { pub version: u64, pub values: BTreeMap<String, i64> }
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CommitError { Conflict, Invalid }
struct State { version: u64, values: BTreeMap<String, i64> }
pub struct Store { state: RwLock<State> }

impl Store {
    pub fn new(values: BTreeMap<String, i64>) -> Self { Self { state: RwLock::new(State { version: 0, values }) } }
    pub fn snapshot(&self) -> Snapshot { let s = self.state.read().unwrap(); Snapshot { version: s.version, values: s.values.clone() } }
    pub fn commit(&self, base_version: u64, ops: &[Op]) -> Result<u64, CommitError> {
        let mut state = self.state.write().map_err(|_| CommitError::Invalid)?;
        if state.version != base_version { return Err(CommitError::Conflict); }
        if ops.is_empty() { return Ok(state.version); }
        let mut next = state.values.clone();
        for op in ops {
            match op {
                Op::Put(key, value) if !key.is_empty() && *value >= 0 => { next.insert(key.clone(), *value); }
                Op::Delete(key) if next.contains_key(key) => { next.remove(key); }
                Op::Transfer(from, to, amount) if from != to && *amount > 0 => {
                    let from_value = *next.get(from).ok_or(CommitError::Invalid)?;
                    let to_value = *next.get(to).ok_or(CommitError::Invalid)?;
                    let new_from = from_value.checked_sub(*amount).filter(|v| *v >= 0).ok_or(CommitError::Invalid)?;
                    let new_to = to_value.checked_add(*amount).ok_or(CommitError::Invalid)?;
                    next.insert(from.clone(), new_from); next.insert(to.clone(), new_to);
                }
                _ => return Err(CommitError::Invalid),
            }
        }
        let version = state.version.checked_add(1).ok_or(CommitError::Invalid)?;
        state.values = next; state.version = version; Ok(version)
    }
}
''',
        },
        "prompt": "Repair the optimistic store without changing the exported Store, Snapshot, Op, CommitError, new, snapshot, or commit API. All stored values are non-negative. commit first requires an exact base_version or returns Conflict without mutation. An empty valid commit is a no-op returning the current version. A non-empty commit applies its ordered operations atomically to a private candidate state and increments the version exactly once. Put requires a non-empty key and non-negative value. Delete requires an existing key. Transfer requires distinct existing accounts, amount > 0, sufficient funds, and checked arithmetic. Any invalid operation or overflow returns Invalid and rolls back every earlier operation and the version. Snapshot is an isolated clone safe during concurrent commits. Concurrent commits from the same version have exactly one winner. Do not panic on lock poisoning; use only the standard library.",
        "verify": [["cargo", "test", "--all-targets", "--quiet"]],
    },
]
