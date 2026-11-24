package storage

// Property-based test (hand-rolled, zero-dependency): random sequences of
// acquire / release / fenced-write operations against real SQLite storage,
// checked against an exact oracle.
//
// Invariants exercised (the same safety properties EpochFencing.tla
// verifies at the design level — here against the implementation):
//  1. TryAcquireLock fails while another epoch's lock is held.
//  2. A fenced write returns success ⟺ its lock identity equals the
//     current locked_by — stale epochs are always rejected.
//  3. ReleaseLock with a wrong identity never clears the lock.
//
// Seeds are fixed so failures are reproducible; on failure the failing
// seed and op trace are logged. Lease-expiry steal is covered separately
// by the claim tests (second-granularity expiry is not oracle-friendly).

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestPBT_LockFencing(t *testing.T) {
	for seed := int64(1); seed <= 30; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			s, ctx := newClaimTestStorage(t)
			createClaimInstance(t, ctx, s, "inst-pbt", "wf")

			r := rand.New(rand.NewSource(seed))
			var lockedBy string // oracle: current locked_by ("" = unlocked)
			epoch := 0
			var trace []string
			log := func(format string, a ...any) {
				trace = append(trace, fmt.Sprintf(format, a...))
			}
			fail := func(format string, a ...any) {
				for i, l := range trace {
					t.Logf("op[%d] %s", i, l)
				}
				t.Fatalf(format, a...)
			}

			const workers = 3
			identities := []string{"w0#0", "w1#0", "w2#0"}

			for op := 0; op < 200; op++ {
				w := r.Intn(workers)
				switch r.Intn(4) {
				case 0: // acquire with a fresh epoch identity
					epoch++
					id := fmt.Sprintf("w%d#%d", w, epoch)
					ok, err := s.TryAcquireLock(ctx, "inst-pbt", id, 300)
					log("acquire w%d -> %s ok=%v err=%v (oracle=%q)", w, id, ok, err, lockedBy)
					if err != nil {
						fail("acquire error: %v", err)
					}
					if ok != (lockedBy == "") {
						fail("acquire ok=%v but oracle=%q", ok, lockedBy)
					}
					if ok {
						lockedBy = id
						identities[w] = id
					}
				case 1: // fenced status write with (possibly stale) identity
					id := identities[w]
					err := s.UpdateInstanceStatusIfLocked(ctx, "inst-pbt", StatusRunning, "", id)
					log("fenced-status w%d id=%q err=%v (oracle=%q)", w, id, err, lockedBy)
					if (err == nil) != (id == lockedBy && id != "") {
						fail("fenced status: err=%v, oracle=%q", err, lockedBy)
					}
				case 2: // fenced history write with unique activity id
					id := identities[w]
					aid := fmt.Sprintf("act:%d", op)
					err := s.AppendHistoryIfLocked(ctx, &HistoryEvent{
						InstanceID: "inst-pbt", ActivityID: aid,
						EventType: HistoryActivityCompleted, EventData: []byte(`{}`),
					}, id)
					log("fenced-history w%d id=%q err=%v (oracle=%q)", w, id, err, lockedBy)
					if (err == nil) != (id == lockedBy && id != "") {
						fail("fenced history: err=%v, oracle=%q", err, lockedBy)
					}
				case 3: // release — right or wrong identity
					if lockedBy == "" {
						continue
					}
					right := lockedBy
					id := right
					if r.Intn(3) == 0 {
						id = fmt.Sprintf("w%d#bogus", w)
					}
					err := s.ReleaseLock(ctx, "inst-pbt", id)
					log("release id=%q err=%v (oracle=%q)", id, err, lockedBy)
					if err != nil {
						fail("release error: %v", err)
					}
					if id == right {
						lockedBy = ""
					} else {
						err2 := s.UpdateInstanceStatusIfLocked(ctx, "inst-pbt", StatusRunning, "", right)
						if err2 != nil {
							fail("release with wrong identity cleared the lock")
						}
					}
				}
			}
		})
	}
}
