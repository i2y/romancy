package spec

// Exhaustive model checker for TaskClaim.tla (2 workers, 2 tasks).
// Establishes the same results TLC would report:
//
//	java -jar tla2tools.jar -deadlock -config TaskClaim_new.cfg TaskClaim.tla
//
// new (CAS=TRUE):  SingleClaimant holds over all reachable states.
// old (CAS=FALSE): a state where two workers both won the same task is
// reachable — the double-claim race the AND state='open' guard prevents.

import (
	"fmt"
	"testing"
)

const (
	tcOpen    = 0
	tcClaimed = 1
	tcDone    = 2
)

type tcState struct {
	state     [2]int    // per task: open/claimed/done
	claimedBy [2]string // per task: "", w1, w2
	picked    [2]string // per worker: candidate task "", t0, t1
	won       [2]uint   // per worker: bitmask of tasks its claim won
	violated  bool
}

func (s tcState) key() string {
	return fmt.Sprintf("%d%d|%s%s|%s%s|%d%d|%v",
		s.state[0], s.state[1], s.claimedBy[0], s.claimedBy[1],
		s.picked[0], s.picked[1], s.won[0], s.won[1], s.violated)
}

var tcWorkers = [2]string{"w1", "w2"}
var tcTasks = [2]string{"t0", "t1"}

func tcSuccessors(s tcState, cas bool) []tcState {
	var out []tcState
	add := func(n tcState) { out = append(out, n) }
	for i, w := range tcWorkers {
		// Pick: observe an open task as candidate.
		if s.picked[i] == "" {
			for ti := range tcTasks {
				if s.state[ti] == tcOpen {
					n := s
					n.picked[i] = tcTasks[ti]
					add(n)
				}
			}
		}
		// Claim: guarded (task still open) or racy write.
		if s.picked[i] != "" {
			var ti int
			for j, t := range tcTasks {
				if s.picked[i] == t {
					ti = j
				}
			}
			success := !cas || s.state[ti] == tcOpen
			if success {
				n := s
				n.state[ti] = tcClaimed
				n.claimedBy[ti] = w
				n.won[i] |= 1 << ti
				for j := range tcWorkers {
					if j != i && n.won[j]&(1<<ti) != 0 {
						n.violated = true
					}
				}
				n.picked[i] = ""
				add(n)
			} else {
				n := s
				n.picked[i] = ""
				add(n)
			}
		}
		// Complete: claimant finishes its task.
		for ti := range tcTasks {
			if s.state[ti] == tcClaimed && s.claimedBy[ti] == w {
				n := s
				n.state[ti] = tcDone
				add(n)
			}
		}
	}
	return out
}

func tcCheck(t *testing.T, cas bool) (states int, violationAt int) {
	t.Helper()
	start := tcState{}
	seen := map[string]bool{start.key(): true}
	queue := []tcState{start}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		states++
		if s.violated && violationAt == 0 {
			violationAt = states
		}
		// claimant sanity: claimed => claimedBy is a real worker
		for ti := range tcTasks {
			if s.state[ti] == tcClaimed && s.claimedBy[ti] == "" {
				t.Fatalf("task claimed by nobody: %+v", s)
			}
		}
		for _, n := range tcSuccessors(s, cas) {
			if !seen[n.key()] {
				seen[n.key()] = true
				queue = append(queue, n)
			}
		}
	}
	return states, violationAt
}

func TestTaskClaim_CAS_SingleClaimant(t *testing.T) {
	states, violationAt := tcCheck(t, true)
	if violationAt != 0 {
		t.Fatalf("SingleClaimant violated at state %d", violationAt)
	}
	t.Logf("CAS: %d states, SingleClaimant held", states)
}

func TestTaskClaim_NoCAS_DoubleClaimReachable(t *testing.T) {
	states, violationAt := tcCheck(t, false)
	if violationAt == 0 {
		t.Fatalf("expected double-claim state unreachable?? %d states", states)
	}
	t.Logf("no CAS: %d states, violation first reachable at %d", states, violationAt)
}
