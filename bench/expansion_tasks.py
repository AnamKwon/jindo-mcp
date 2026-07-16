"""Hard fixtures for previously unmeasured local language/toolchain cells."""

TASKS = [
    {
        "id": "c_incremental_binary_frame_decoder",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "c",
        "task_type": "memory_safe_incremental_parser",
        "public_files": {
            "decoder.h": r'''#ifndef DECODER_H
#define DECODER_H
#include <stddef.h>
#include <stdint.h>

typedef int (*frame_callback)(const uint8_t *payload, uint32_t length, void *context);

struct frame_decoder {
    uint32_t max_frame;
    uint32_t frame_length;
    uint32_t received;
    uint8_t header[4];
    size_t header_used;
    uint8_t *payload;
    frame_callback callback;
    void *context;
    int failed;
};

int frame_decoder_init(struct frame_decoder *decoder, uint32_t max_frame,
                       frame_callback callback, void *context);
int frame_decoder_feed(struct frame_decoder *decoder, const uint8_t *data, size_t length);
void frame_decoder_destroy(struct frame_decoder *decoder);
#endif
''',
            "decoder.c": r'''#include "decoder.h"
#include <stdlib.h>
#include <string.h>

int frame_decoder_init(struct frame_decoder *d, uint32_t max_frame, frame_callback cb, void *ctx) {
    if (!d || !cb || max_frame == 0) return -1;
    memset(d, 0, sizeof(*d)); d->max_frame = max_frame; d->callback = cb; d->context = ctx;
    return 0;
}

int frame_decoder_feed(struct frame_decoder *d, const uint8_t *data, size_t length) {
    if (!d || !data || length < 4) return -1;
    uint32_t n = ((uint32_t)data[0] << 24) | ((uint32_t)data[1] << 16) |
                 ((uint32_t)data[2] << 8) | data[3];
    if (n > d->max_frame || length != (size_t)n + 4) return -1;
    return d->callback(data + 4, n, d->context);
}

void frame_decoder_destroy(struct frame_decoder *d) { if (d) free(d->payload); }
''',
            "test_decoder.c": r'''#include "decoder.h"
#include <assert.h>
#include <string.h>
static int seen(const uint8_t *p, uint32_t n, void *ctx) {
    int *count = ctx; assert(n == 3 && memcmp(p, "abc", 3) == 0); (*count)++; return 0;
}
int main(void) {
    struct frame_decoder d; int count = 0; const uint8_t frame[] = {0,0,0,3,'a','b','c'};
    assert(frame_decoder_init(&d, 16, seen, &count) == 0);
    assert(frame_decoder_feed(&d, frame, sizeof(frame)) == 0 && count == 1);
    frame_decoder_destroy(&d); return 0;
}
''',
        },
        "hidden_files": {
            "hidden_test.c": r'''#include "decoder.h"
#include <assert.h>
#include <stdint.h>
#include <string.h>

struct capture { int calls; uint32_t lengths[8]; uint8_t bytes[8][8]; int fail_on; };
static int capture_frame(const uint8_t *p, uint32_t n, void *raw) {
    struct capture *c = raw; int i = c->calls++;
    c->lengths[i] = n; if (n) memcpy(c->bytes[i], p, n);
    return c->fail_on == c->calls ? 7 : 0;
}

static void split_and_multiple(void) {
    struct capture c = {0}; struct frame_decoder d;
    const uint8_t stream[] = {0,0,0,3,'a',0,'c', 0,0,0,0, 0,0,0,2,'x','y'};
    assert(frame_decoder_init(&d, 8, capture_frame, &c) == 0);
    for (size_t i = 0; i < sizeof(stream); i++) assert(frame_decoder_feed(&d, stream+i, 1) == 0);
    assert(c.calls == 3 && c.lengths[0] == 3 && c.bytes[0][1] == 0);
    assert(c.lengths[1] == 0 && c.lengths[2] == 2 && memcmp(c.bytes[2], "xy", 2) == 0);
    assert(frame_decoder_feed(&d, NULL, 0) == 0);
    frame_decoder_destroy(&d); frame_decoder_destroy(&d);
}

static void malformed_is_sticky(void) {
    struct capture c = {0}; struct frame_decoder d;
    const uint8_t too_big[] = {0,0,0,9}; const uint8_t good[] = {0,0,0,1,'z'};
    assert(frame_decoder_init(&d, 8, capture_frame, &c) == 0);
    assert(frame_decoder_feed(&d, too_big, sizeof(too_big)) == -1);
    assert(frame_decoder_feed(&d, good, sizeof(good)) == -1 && c.calls == 0);
    frame_decoder_destroy(&d);
}

static void callback_failure_is_sticky(void) {
    struct capture c = {.fail_on = 1}; struct frame_decoder d;
    const uint8_t frames[] = {0,0,0,1,'a',0,0,0,1,'b'};
    assert(frame_decoder_init(&d, 4, capture_frame, &c) == 0);
    assert(frame_decoder_feed(&d, frames, sizeof(frames)) == 7);
    assert(c.calls == 1 && frame_decoder_feed(&d, frames, 5) == -1);
    frame_decoder_destroy(&d);
}

static void validation(void) {
    struct frame_decoder d; struct capture c = {0}; uint8_t byte = 0;
    assert(frame_decoder_init(NULL, 1, capture_frame, &c) == -1);
    assert(frame_decoder_init(&d, 0, capture_frame, &c) == -1);
    assert(frame_decoder_init(&d, 1, NULL, &c) == -1);
    assert(frame_decoder_init(&d, 1, capture_frame, &c) == 0);
    assert(frame_decoder_feed(&d, NULL, 1) == -1);
    frame_decoder_destroy(&d);
    assert(frame_decoder_init(&d, 1, capture_frame, &c) == 0);
    assert(frame_decoder_feed(&d, &byte, 1) == 0);
    frame_decoder_destroy(&d);
}

int main(void) { split_and_multiple(); malformed_is_sticky(); callback_failure_is_sticky(); validation(); return 0; }
''',
        },
        "reference_files": {
            "decoder.c": r'''#include "decoder.h"
#include <stdlib.h>
#include <string.h>

int frame_decoder_init(struct frame_decoder *d, uint32_t max_frame, frame_callback cb, void *ctx) {
    if (!d || !cb || max_frame == 0) return -1;
    memset(d, 0, sizeof(*d)); d->max_frame = max_frame; d->callback = cb; d->context = ctx;
    return 0;
}

static int fail(struct frame_decoder *d, int code) {
    free(d->payload); d->payload = NULL; d->failed = 1; return code;
}

int frame_decoder_feed(struct frame_decoder *d, const uint8_t *data, size_t length) {
    if (!d || d->failed || (!data && length != 0) || !d->callback || d->max_frame == 0) return -1;
    size_t at = 0;
    while (at < length) {
        while (d->header_used < 4 && at < length) d->header[d->header_used++] = data[at++];
        if (d->header_used < 4) break;
        if (!d->payload && d->received == 0) {
            d->frame_length = ((uint32_t)d->header[0] << 24) | ((uint32_t)d->header[1] << 16) |
                              ((uint32_t)d->header[2] << 8) | d->header[3];
            if (d->frame_length > d->max_frame) return fail(d, -1);
            if (d->frame_length == 0) {
                int rc = d->callback(NULL, 0, d->context); d->header_used = 0;
                if (rc != 0) return fail(d, rc);
                continue;
            }
            d->payload = malloc(d->frame_length);
            if (!d->payload) return fail(d, -1);
        }
        size_t needed = (size_t)d->frame_length - d->received;
        size_t available = length - at; size_t take = needed < available ? needed : available;
        memcpy(d->payload + d->received, data + at, take); d->received += (uint32_t)take; at += take;
        if (d->received == d->frame_length) {
            int rc = d->callback(d->payload, d->frame_length, d->context);
            free(d->payload); d->payload = NULL; d->received = 0; d->frame_length = 0; d->header_used = 0;
            if (rc != 0) return fail(d, rc);
        }
    }
    return 0;
}

void frame_decoder_destroy(struct frame_decoder *d) {
    if (!d) return; free(d->payload); d->payload = NULL; d->failed = 1;
}
''',
        },
        "prompt": "Implement the C frame_decoder API without changing decoder.h. The stream is a sequence of four-byte unsigned big-endian lengths followed by that many binary payload bytes. feed must accept arbitrary chunk boundaries, zero-length feeds, split headers/payloads, multiple frames per call, embedded NUL bytes, and zero-length frames. Never allocate more than max_frame. Reject invalid arguments and oversized frames with -1 and enter a sticky failed state. If the callback returns nonzero, stop immediately, return that exact code, and enter the same sticky state; no later frame may be delivered. A NULL data pointer is valid only for length zero. destroy must be NULL-safe and idempotent. Use C11 only, preserve input bytes, and remain clean under address and undefined-behavior sanitizers.",
        "verify": [
            ["cc", "-std=c11", "-Wall", "-Wextra", "-Werror", "-fsanitize=address,undefined", "decoder.c", "test_decoder.c", "-o", "visible_test"],
            ["./visible_test"],
            ["cc", "-std=c11", "-Wall", "-Wextra", "-Werror", "-fsanitize=address,undefined", "decoder.c", "hidden_test.c", "-o", "hidden_test"],
            ["./hidden_test"],
        ],
    },
    {
        "id": "swift_actor_atomic_ledger",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "swift",
        "task_type": "actor_isolation_atomic_batch",
        "public_files": {
            "Ledger.swift": r'''public struct Transfer: Sendable, Equatable {
    public let id: String
    public let from: String
    public let to: String
    public let amount: Int64
    public init(id: String, from: String, to: String, amount: Int64) {
        self.id = id; self.from = from; self.to = to; self.amount = amount
    }
}

public struct LedgerSnapshot: Sendable, Equatable {
    public let balances: [String: Int64]
    public let applied: [String: Transfer]
}

public enum LedgerError: Error, Equatable { case invalid, missingAccount, insufficientFunds, overflow, conflictingID }

public actor Ledger {
    private var balances: [String: Int64]
    private var applied: [String: Transfer] = [:]
    public init(_ balances: [String: Int64]) throws { self.balances = balances }
    public func applyBatch(_ transfers: [Transfer]) throws -> Int {
        for t in transfers { balances[t.from, default: 0] -= t.amount; balances[t.to, default: 0] += t.amount }
        return transfers.count
    }
    public func snapshot() -> LedgerSnapshot { LedgerSnapshot(balances: balances, applied: applied) }
}
''',
            "Visible.swift": r'''@main struct Visible {
    static func main() async throws {
        let ledger = try Ledger(["a": 10, "b": 0])
        let count = try await ledger.applyBatch([Transfer(id: "x", from: "a", to: "b", amount: 3)])
        let snap = await ledger.snapshot()
        precondition(count == 1 && snap.balances["a"] == 7 && snap.balances["b"] == 3)
    }
}
''',
        },
        "hidden_files": {
            "Hidden.swift": r'''@MainActor func expectError(_ expected: LedgerError, _ operation: () async throws -> Void) async {
    do { try await operation(); preconditionFailure("expected \(expected)") }
    catch let error as LedgerError { precondition(error == expected) }
    catch { preconditionFailure("wrong error \(error)") }
}

@main struct Hidden {
    static func main() async throws {
        var source: [String: Int64] = ["a": 10, "b": 0, "max": Int64.max]
        let ledger = try Ledger(source); source["a"] = 999
        let before = await ledger.snapshot(); precondition(before.balances["a"] == 10)

        await expectError(.missingAccount) {
            _ = try await ledger.applyBatch([
                Transfer(id: "ok", from: "a", to: "b", amount: 2),
                Transfer(id: "bad", from: "missing", to: "b", amount: 1),
            ])
        }
        let rolledBack = await ledger.snapshot()
        precondition(rolledBack == before)

        let t = Transfer(id: "once", from: "a", to: "b", amount: 4)
        let firstCount = try await ledger.applyBatch([t, t])
        let replayCount = try await ledger.applyBatch([t])
        precondition(firstCount == 1 && replayCount == 0)
        let after = await ledger.snapshot()
        precondition(after.balances["a"] == 6 && after.balances["b"] == 4 && after.applied["once"] == t)
        await expectError(.conflictingID) {
            _ = try await ledger.applyBatch([Transfer(id: "once", from: "a", to: "b", amount: 3)])
        }
        await expectError(.overflow) {
            _ = try await ledger.applyBatch([Transfer(id: "overflow", from: "a", to: "max", amount: 1)])
        }
        let afterOverflow = await ledger.snapshot()
        precondition(afterOverflow == after)

        let concurrent = try Ledger(["a": 10, "b": 0, "c": 0])
        let one = Task { try await concurrent.applyBatch([Transfer(id: "one", from: "a", to: "b", amount: 7)]) }
        let two = Task { try await concurrent.applyBatch([Transfer(id: "two", from: "a", to: "c", amount: 7)]) }
        var successes = 0
        do { _ = try await one.value; successes += 1 } catch let e as LedgerError { precondition(e == .insufficientFunds) }
        do { _ = try await two.value; successes += 1 } catch let e as LedgerError { precondition(e == .insufficientFunds) }
        let concurrentSnapshot = await concurrent.snapshot()
        precondition(successes == 1 && concurrentSnapshot.balances.values.reduce(0, +) == 10)

        await expectError(.invalid) { _ = try Ledger(["": 1]) }
        await expectError(.invalid) { _ = try Ledger(["a": -1]) }
        await expectError(.invalid) { _ = try await ledger.applyBatch([Transfer(id: "", from: "a", to: "b", amount: 1)]) }
        await expectError(.invalid) { _ = try await ledger.applyBatch([Transfer(id: "same", from: "a", to: "a", amount: 1)]) }
        await expectError(.invalid) { _ = try await ledger.applyBatch([Transfer(id: "zero", from: "a", to: "b", amount: 0)]) }
    }
}
''',
        },
        "reference_files": {
            "Ledger.swift": r'''public struct Transfer: Sendable, Equatable {
    public let id: String
    public let from: String
    public let to: String
    public let amount: Int64
    public init(id: String, from: String, to: String, amount: Int64) {
        self.id = id; self.from = from; self.to = to; self.amount = amount
    }
}

public struct LedgerSnapshot: Sendable, Equatable {
    public let balances: [String: Int64]
    public let applied: [String: Transfer]
}

public enum LedgerError: Error, Equatable { case invalid, missingAccount, insufficientFunds, overflow, conflictingID }

public actor Ledger {
    private var balances: [String: Int64]
    private var applied: [String: Transfer] = [:]
    public init(_ initial: [String: Int64]) throws {
        guard initial.allSatisfy({ !$0.key.isEmpty && $0.value >= 0 }) else { throw LedgerError.invalid }
        balances = initial
    }
    public func applyBatch(_ transfers: [Transfer]) throws -> Int {
        var nextBalances = balances; var nextApplied = applied; var added = 0
        for transfer in transfers {
            guard !transfer.id.isEmpty, transfer.from != transfer.to, transfer.amount > 0 else { throw LedgerError.invalid }
            guard let fromBalance = nextBalances[transfer.from], let toBalance = nextBalances[transfer.to] else { throw LedgerError.missingAccount }
            if let previous = nextApplied[transfer.id] {
                guard previous == transfer else { throw LedgerError.conflictingID }
                continue
            }
            guard fromBalance >= transfer.amount else { throw LedgerError.insufficientFunds }
            let (newFrom, underflow) = fromBalance.subtractingReportingOverflow(transfer.amount)
            let (newTo, overflow) = toBalance.addingReportingOverflow(transfer.amount)
            guard !underflow && !overflow else { throw LedgerError.overflow }
            nextBalances[transfer.from] = newFrom; nextBalances[transfer.to] = newTo
            nextApplied[transfer.id] = transfer; added += 1
        }
        balances = nextBalances; applied = nextApplied; return added
    }
    public func snapshot() -> LedgerSnapshot { LedgerSnapshot(balances: balances, applied: applied) }
}
''',
        },
        "prompt": "Repair Ledger.swift without changing any public declaration. Ledger is an actor-backed, value-isolated ledger. Its throwing initializer rejects empty account IDs and negative balances. applyBatch validates nonempty transfer IDs, distinct existing accounts, and positive amounts; it uses checked Int64 arithmetic and rejects insufficient funds. A batch is atomic. A previously applied identical transfer, including an identical duplicate inside the same batch, is a no-op; reusing an ID with different fields is conflictingID. Return the number of newly applied unique transfers. Failed batches change nothing. snapshot must expose value snapshots without mutable aliasing. Concurrent calls must remain serializable through actor isolation. Use only the Swift standard library and compile under Swift 6 strict concurrency.",
        "verify": [
            ["swiftc", "-strict-concurrency=complete", "-warnings-as-errors", "Ledger.swift", "Visible.swift", "-o", "visible_test"],
            ["./visible_test"],
            ["swiftc", "-strict-concurrency=complete", "-warnings-as-errors", "Ledger.swift", "Hidden.swift", "-o", "hidden_test"],
            ["./hidden_test"],
        ],
    },
    {
        "id": "bash_atomic_env_reconciler_v2",
        "difficulty": "extreme",
        "domain": "coding",
        "language": "shell",
        "task_type": "quoting_atomic_file_reconciliation",
        "public_files": {
            "reconcile-env.sh": r'''#!/usr/bin/env bash
set -e
current=$1
desired=$2
output=$3
cat "$current" "$desired" > "$output"
''',
            "test_visible.py": r'''import pathlib, subprocess, tempfile
with tempfile.TemporaryDirectory() as raw:
    root = pathlib.Path(raw); current=root/"current"; desired=root/"desired"; output=root/"out"
    current.write_text("A=1\n"); desired.write_text("A=2\nB=3\n")
    subprocess.run(["/bin/bash", "reconcile-env.sh", str(current), str(desired), str(output)], check=True)
    assert output.read_text() == "A=2\nB=3\n"
''',
        },
        "hidden_files": {
            "test_hidden.py": r'''import os, pathlib, stat, subprocess, tempfile

SCRIPT = str(pathlib.Path("reconcile-env.sh").resolve()); BASH = "/bin/bash"
def run(current, desired, output):
    return subprocess.run([BASH, SCRIPT, str(current), str(desired), str(output)], text=True, capture_output=True)

with tempfile.TemporaryDirectory(prefix="env bench ") as raw:
    root = pathlib.Path(raw); current=root/"current file"; desired=root/"desired file"; output=root/"output file"
    marker=root/"PWNED"
    current.write_text("# keep comments out\nZ=last\nA=old value\nLITERAL=$(touch %s)\n" % marker)
    desired.write_text("A=new=value with spaces\nB=two\n")
    output.write_text("OLD=preserve\n"); output.chmod(0o640)
    result=run(current, desired, output); assert result.returncode == 0, result.stderr
    assert output.read_text() == "A=new=value with spaces\nB=two\nLITERAL=$(touch %s)\nZ=last\n" % marker
    assert not marker.exists() and stat.S_IMODE(output.stat().st_mode) == 0o640

    before=output.read_bytes(); desired.write_text("GOOD=1\nBAD KEY=2\n")
    result=run(current, desired, output); assert result.returncode != 0
    assert output.read_bytes() == before
    assert not list(root.glob(".output file.tmp.*"))

    desired.write_text("DUP=1\nDUP=2\n"); result=run(current, desired, output)
    assert result.returncode != 0 and output.read_bytes() == before

    target=root/"target"; target.write_text("SAFE=1\n"); link=root/"link"; link.symlink_to(target)
    result=run(current, desired, link); assert result.returncode != 0 and target.read_text() == "SAFE=1\n"

    # Empty first input must not confuse per-file duplicate tracking.
    current.write_text(""); desired.write_text("A=1\n")
    assert run(current, desired, output).returncode == 0 and output.read_text() == "A=1\n"
    desired.write_text("A=1\nA=2\n"); assert run(current, desired, output).returncode != 0

    # Sort by key, not the full KEY=VALUE line; preserve a CR inside VALUE.
    current.write_text("A2=second\nA=first\n"); desired.write_bytes(b"CR=value\r\n")
    assert run(current, desired, output).returncode == 0
    assert output.read_bytes() == b"A=first\nA2=second\nCR=value\r\n"

    # Only an empty line is blank and only a leading # starts a comment.
    for invalid in ("   \n", "  #not-a-comment\n"):
        current.write_text(invalid); desired.write_text("")
        assert run(current, desired, output).returncode != 0

    # An '=' in a quoted absolute filename is data, not an awk assignment.
    current = root/"current=name"; current.write_text("X=1\n"); desired.write_text("Y=2\n")
    assert run(current, desired, output).returncode == 0 and output.read_text() == "X=1\nY=2\n"

    result=subprocess.run([BASH, SCRIPT], text=True, capture_output=True)
    assert result.returncode != 0
''',
        },
        "reference_files": {
            "reconcile-env.sh": r'''#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 3 ]; then exit 2; fi
current=$1; desired=$2; output=$3
if [ ! -f "$current" ] || [ -L "$current" ] || [ ! -f "$desired" ] || [ -L "$desired" ] || [ -L "$output" ]; then exit 2; fi
parent=$(dirname "$output"); base=$(basename "$output")
if [ ! -d "$parent" ]; then exit 2; fi
tmp=$(mktemp "$parent/.${base}.tmp.XXXXXX")
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT HUP INT TERM
if ! awk '
function invalid() { bad=1; exit 2 }
{
    if ($0 == "" || substr($0,1,1) == "#") next
    at=index($0,"="); if (at < 2) invalid()
    key=substr($0,1,at-1); value=substr($0,at+1)
    if (key !~ /^[A-Za-z_][A-Za-z0-9_]*$/) invalid()
    token=FILENAME SUBSEP key; if (seen[token]++) invalid()
    values[key]=value
}
END {
    if (bad) exit 2
    count=0; for (key in values) keys[++count]=key
    for (i=2; i<=count; i++) {
        candidate=keys[i]; j=i-1
        while (j>=1 && keys[j] > candidate) { keys[j+1]=keys[j]; j-- }
        keys[j+1]=candidate
    }
    for (i=1; i<=count; i++) print keys[i] "=" values[keys[i]]
}
' "$current" "$desired" > "$tmp"; then exit 2; fi
if [ -e "$output" ]; then
    mode=$(stat -f '%Lp' "$output" 2>/dev/null || stat -c '%a' "$output")
    chmod "$mode" "$tmp"
fi
mv -f "$tmp" "$output"
trap - EXIT HUP INT TERM
''',
        },
        "prompt": "Replace reconcile-env.sh while preserving its three-argument CLI: CURRENT DESIRED OUTPUT and compatibility with macOS /bin/bash 3.2. Inputs are UTF-8 line files containing exactly empty blank lines, comments whose first byte is #, or KEY=VALUE records. KEY is [A-Za-z_][A-Za-z0-9_]*; VALUE is the literal remainder and may contain spaces, tabs, carriage returns, shell metacharacters, or additional '='. Reject every other line and duplicate keys within either individual input; an empty input is valid and desired values override current values. Write every resulting key once in bytewise key-sorted order with a trailing newline. Never eval or source values. Reject symlink inputs/output and missing regular inputs. Update OUTPUT atomically with a sibling temporary file, preserve an existing output's permission bits, leave existing output byte-for-byte unchanged on any failure, and clean temporary files on normal failure or signals. Correctly quote paths containing spaces, '=' and metacharacters. Use only Bash 3.2 syntax plus standard POSIX/macOS command-line tools.",
        "verify": [["python3", "test_visible.py"], ["python3", "test_hidden.py"]],
    },
]
