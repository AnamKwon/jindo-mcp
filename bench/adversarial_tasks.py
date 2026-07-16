"""Hermetic tasks whose hidden tests target plausible, locally-passing mistakes."""

GO_MOD = "module bench\n\ngo 1.23\n"

TASKS = [
    {
        "id": "lease_registry_fencing",
        "difficulty": "extreme",
        "public_files": {
            "go.mod": GO_MOD,
            "lease/lease.go": '''package lease

import (
    "errors"
    "sync"
    "time"
)

var ErrHeld = errors.New("lease held")
var ErrStale = errors.New("stale fencing token")

type Clock interface { Now() time.Time }
type record struct { owner string; token uint64; expires time.Time }
type Registry struct { mu sync.RWMutex; clock Clock; next uint64; leases map[string]record }

func New(clock Clock) *Registry { return &Registry{clock: clock, leases: map[string]record{}} }

// Acquire grants a lease or renews the current owner's lease. This incomplete
// implementation has subtle fencing, expiry and concurrency bugs.
func (r *Registry) Acquire(key, owner string, ttl time.Duration) (uint64, error) {
    r.mu.Lock(); defer r.mu.Unlock()
    if old, ok := r.leases[key]; ok && old.owner != owner { return 0, ErrHeld }
    r.next++
    r.leases[key] = record{owner: owner, token: r.next, expires: time.Now().Add(ttl)}
    return r.next, nil
}

func (r *Registry) Release(key, owner string, token uint64) error {
    r.mu.Lock(); defer r.mu.Unlock(); delete(r.leases, key); return nil
}

func (r *Registry) Snapshot() map[string]uint64 {
    out := map[string]uint64{}; for k,v := range r.leases { out[k] = v.token }; return out
}
''',
            "lease/lease_test.go": '''package lease
import ("sync"; "testing"; "time")
type fakeClock struct { mu sync.Mutex; now time.Time }
func (f *fakeClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fakeClock) add(d time.Duration) { f.mu.Lock(); f.now=f.now.Add(d); f.mu.Unlock() }
func TestExpiredLeaseCanBeTaken(t *testing.T) { c:=&fakeClock{now:time.Unix(1,0)}; r:=New(c); a,_:=r.Acquire("k","a",time.Second); c.add(2*time.Second); b,e:=r.Acquire("k","b",time.Second); if e!=nil||b<=a { t.Fatalf("%v %d %d",e,a,b) } }
''',
        },
        "hidden_files": {
            "lease/hidden_test.go": '''package lease
import ("errors"; "sync"; "testing"; "time")
func TestRenewKeepsTokenAndExtends(t *testing.T){ c:=&fakeClock{now:time.Unix(1,0)}; r:=New(c); a,_:=r.Acquire("k","a",time.Second); c.add(500*time.Millisecond); b,e:=r.Acquire("k","a",time.Second); if e!=nil||a!=b {t.Fatalf("renew token %d -> %d err=%v",a,b,e)}; c.add(700*time.Millisecond); if _,e=r.Acquire("k","b",time.Second); !errors.Is(e,ErrHeld){t.Fatalf("renew did not extend: %v",e)} }
func TestStaleReleaseCannotDeleteNewOwner(t *testing.T){ c:=&fakeClock{now:time.Unix(1,0)}; r:=New(c); old,_:=r.Acquire("k","a",time.Second); c.add(2*time.Second); fresh,_:=r.Acquire("k","b",time.Second); if e:=r.Release("k","a",old); !errors.Is(e,ErrStale){t.Fatalf("stale release=%v",e)}; if got:=r.Snapshot()["k"]; got!=fresh {t.Fatalf("deleted new lease: %d",got)} }
func TestInvalidTTLAndRace(t *testing.T){ c:=&fakeClock{now:time.Unix(1,0)}; r:=New(c); if _,e:=r.Acquire("x","a",0); e==nil {t.Fatal("zero ttl accepted")}; var wg sync.WaitGroup; for i:=0;i<100;i++ {wg.Add(1); go func(){defer wg.Done(); r.Acquire("k","a",time.Second); _=r.Snapshot()}()}; wg.Wait() }
'''
        },
        "prompt": "Repair package lease without changing its public API. A registry uses the injected Clock. Acquisition of an absent or expired key creates a globally monotonic fencing token; takeover after expiry must get a strictly larger token. Re-acquiring a live lease by the same owner is a renewal: extend expiry but retain its token. A different owner gets ErrHeld while live. Release succeeds only for the current owner and exact token; otherwise return ErrStale and preserve the lease. ttl <= 0 is invalid and must not mutate state. Snapshot returns only live leases, must be concurrency-safe, and callers must not be able to mutate registry state through it. Preserve global token monotonicity even after releases. The package must pass the race detector.",
        "verify": [["go", "test", "./lease", "-race", "-count=1"]],
    },
    {
        "id": "ledger_idempotent_transfer",
        "difficulty": "adversarial",
        "public_files": {
            "go.mod": GO_MOD,
            "ledger/ledger.go": '''package ledger
import ("errors"; "sync")
var ErrFunds=errors.New("insufficient funds")
var ErrConflict=errors.New("idempotency conflict")
type receipt struct{ from,to string; amount int64; err error }
type Ledger struct{ mu sync.Mutex; balances map[string]int64; seen map[string]receipt }
func New(initial map[string]int64)*Ledger{return &Ledger{balances:initial,seen:map[string]receipt{}}}
func(l *Ledger) Transfer(id,from,to string,amount int64) error { l.mu.Lock(); defer l.mu.Unlock(); if r,ok:=l.seen[id];ok{return r.err}; if l.balances[from]<amount{return ErrFunds}; l.balances[from]-=amount;l.balances[to]+=amount;l.seen[id]=receipt{from,to,amount,nil};return nil }
func(l *Ledger) Balance(account string)int64{ return l.balances[account] }
''',
            "ledger/ledger_test.go": '''package ledger
import "testing"
func TestTransfer(t *testing.T){l:=New(map[string]int64{"a":10});if e:=l.Transfer("1","a","b",4);e!=nil{t.Fatal(e)};if l.Balance("a")!=6||l.Balance("b")!=4{t.Fatal("balances")};if e:=l.Transfer("1","a","b",4);e!=nil{t.Fatal(e)}}
''',
        },
        "hidden_files": {
            "ledger/hidden_test.go": '''package ledger
import("errors";"sync";"testing")
func TestInputIsolation(t *testing.T){src:=map[string]int64{"a":10};l:=New(src);src["a"]=999;if l.Balance("a")!=10{t.Fatal("aliased input")}}
func TestConflictAndFailureReplay(t *testing.T){l:=New(map[string]int64{"a":3});if e:=l.Transfer("x","a","b",4);!errors.Is(e,ErrFunds){t.Fatal(e)};l.balances["a"]=10;if e:=l.Transfer("x","a","b",4);!errors.Is(e,ErrFunds){t.Fatalf("failure was not replayed: %v",e)};if e:=l.Transfer("x","a","c",4);!errors.Is(e,ErrConflict){t.Fatalf("conflict=%v",e)}}
func TestValidationNoMintAndRace(t *testing.T){l:=New(map[string]int64{"a":100});for _,tc:=range[]struct{id,f,to string;n int64}{{"","a","b",1},{"x","","b",1},{"y","a","",1},{"z","a","a",1},{"q","a","b",0},{"r","a","b",-2}}{if e:=l.Transfer(tc.id,tc.f,tc.to,tc.n);e==nil{t.Fatalf("accepted %+v",tc)}};var wg sync.WaitGroup;for i:=0;i<100;i++{wg.Add(2);go func(i int){defer wg.Done();l.Transfer(string(rune(i+100)),"a","b",1)}(i);go func(){defer wg.Done();_=l.Balance("a")}()};wg.Wait();if l.Balance("a")+l.Balance("b")!=100{t.Fatal("money changed")}}
'''
        },
        "prompt": "Repair package ledger while preserving New, Transfer, Balance and the two sentinel errors. New must defensively copy and reject no existing data shape. Transfer is a concurrent in-memory atomic ledger command. id/from/to must be non-empty, from != to, and amount > 0; invalid commands return an error without mutation. The first result for an id, including ErrFunds, is permanently replayable for an exactly identical command. Reusing an id with different from/to/amount returns ErrConflict without mutation, even if the first attempt failed. Successful transfers conserve the total and never expose intermediate balances. Balance is safe during transfers. Do not add exported API. Pass -race.",
        "verify": [["go", "test", "./ledger", "-race", "-count=1"]],
    },
    {
        "id": "atomic_config_migration",
        "difficulty": "hard",
        "public_files": {
            "go.mod": GO_MOD,
            "migration/migrate.go": '''package migration
import("encoding/json";"fmt";"os";"path/filepath")
type V1 struct{Endpoint string `json:"endpoint"`;Token string `json:"token"`}
type V2 struct{Version int `json:"version"`;BaseURL string `json:"base_url"`;Credential string `json:"credential"`}
func Migrate(path string) error{b,e:=os.ReadFile(path);if e!=nil{return e};var v V1;if e=json.Unmarshal(b,&v);e!=nil{return e};out,_:=json.Marshal(V2{2,v.Endpoint,v.Token});if e=os.WriteFile(filepath.Clean(path),out,0644);e!=nil{return fmt.Errorf("write: %w",e)};return nil}
''',
            "migration/migrate_test.go": '''package migration
import("encoding/json";"os";"path/filepath";"testing")
func TestMigrate(t *testing.T){p:=filepath.Join(t.TempDir(),"c.json");os.WriteFile(p,[]byte(`{"endpoint":"https://x","token":"t"}`),0600);if e:=Migrate(p);e!=nil{t.Fatal(e)};var got V2;b,_:=os.ReadFile(p);json.Unmarshal(b,&got);if got.Version!=2||got.BaseURL!="https://x"{t.Fatalf("%+v",got)}}
''',
        },
        "hidden_files": {
            "migration/hidden_test.go": '''package migration
import("bytes";"os";"path/filepath";"testing")
func TestPreservesModeAndUnknownFields(t *testing.T){p:=filepath.Join(t.TempDir(),"c.json");os.WriteFile(p,[]byte(`{"endpoint":"x","token":"t","region":"kr"}`),0640);if e:=Migrate(p);e!=nil{t.Fatal(e)};b,_:=os.ReadFile(p);if !bytes.Contains(b,[]byte(`"region"`)){t.Fatal("unknown field lost")};st,_:=os.Stat(p);if st.Mode().Perm()!=0640{t.Fatalf("mode %o",st.Mode().Perm())}}
func TestV2NoOpAndInvalidUnchanged(t *testing.T){d:=t.TempDir();p:=filepath.Join(d,"c.json");v2:=[]byte(`{"version":2,"base_url":"x","credential":"t"}`);os.WriteFile(p,v2,0600);if e:=Migrate(p);e!=nil{t.Fatal(e)};after,_:=os.ReadFile(p);if !bytes.Equal(after,v2){t.Fatal("v2 was rewritten")};bad:=[]byte(`{"endpoint":3}`);os.WriteFile(p,bad,0600);if e:=Migrate(p);e==nil{t.Fatal("bad accepted")};after,_=os.ReadFile(p);if !bytes.Equal(after,bad){t.Fatal("bad input changed")}}
func TestNoTempLeak(t *testing.T){d:=t.TempDir();p:=filepath.Join(d,"c.json");os.WriteFile(p,[]byte(`{"endpoint":"x","token":"t"}`),0600);if e:=Migrate(p);e!=nil{t.Fatal(e)};es,_:=os.ReadDir(d);if len(es)!=1{t.Fatalf("temp leak: %v",es)}}
'''
        },
        "prompt": "Make migration.Migrate a crash-safe, backward-compatible in-place migration. Valid v1 requires string endpoint and token and becomes v2 with version=2/base_url/credential while preserving all unknown JSON fields. Already-valid v2 must be a byte-for-byte no-op. Invalid JSON, mixed/unsupported versions, missing or wrongly typed required fields return an error and leave the original bytes untouched. A successful migration preserves the original permission bits, writes via a temporary file in the same directory, fsyncs the file, atomically renames, fsyncs the directory where supported, and cleans temporary files on every failure. Do not follow a symlink at path. Keep the exported types and Migrate signature; use only the standard library.",
        "verify": [["go", "test", "./migration", "-count=1"], ["go", "vet", "./migration"]],
    },
]
