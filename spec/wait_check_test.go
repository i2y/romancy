// wait_check_test.go model-checks the channel-receive suspend protocol
// described in WaitRegistration.tla by exhaustive state-space exploration in
// Go. The transition relation mirrors the spec action for action:
//
//   - the pre-fix design (pending check and wait registration as separate
//     steps) reaches a terminal missed-wakeup state;
//   - atomic registration with a reliable publisher scan does not;
//   - atomic registration alone still admits the residual (a waiter scan can
//     run before the registration commits);
//   - adding the requeue sweep heals every stranded state into delivery.
//
// Run TLC on the spec proper when a Java toolchain is available:
//
//	java -jar tla2tools.jar -deadlock -config WaitRegistration_sweep.cfg WaitRegistration.tla
package spec

import (
	"fmt"
	"testing"
)

// waitState mirrors the spec variables of WaitRegistration.tla.
type waitState struct {
	pending  bool
	waiting  bool
	received bool
	rpc      string // checking, suspending, done
	ppc      string // publishing, scanned, done
}

type waitProtocol struct {
	atomicReg   bool // pending recheck inside the registration
	scanCanMiss bool // waiter scan may miss a committed/uncommitted wait
	sweep       bool // periodic requeue of waiting+pending
}

func waitSuccessors(s waitState, p waitProtocol) []waitState {
	var out []waitState
	add := func(n waitState) { out = append(out, n) }

	// ---- receiver ----
	if p.atomicReg {
		// R_RegisterAtomic
		if s.rpc == "checking" {
			n := s
			n.rpc = "done"
			if s.pending {
				n.pending = false
				n.received = true
			} else {
				n.waiting = true
			}
			add(n)
		}
	} else {
		// R_CheckPending
		if s.rpc == "checking" {
			n := s
			if s.pending {
				n.pending = false
				n.received = true
				n.rpc = "done"
			} else {
				n.rpc = "suspending"
			}
			add(n)
		}
		// R_RegisterWait
		if s.rpc == "suspending" {
			n := s
			n.waiting = true
			n.rpc = "done"
			add(n)
		}
	}

	// ---- publisher ----
	// P_Insert
	if s.ppc == "publishing" {
		n := s
		n.pending = true
		n.ppc = "scanned"
		add(n)
	}
	// P_Deliver
	if s.ppc == "scanned" && s.waiting {
		n := s
		n.pending = false
		n.waiting = false
		n.received = true
		n.ppc = "done"
		add(n)
	}
	// P_ScanMiss
	if s.ppc == "scanned" && (!s.waiting || p.scanCanMiss) {
		n := s
		n.ppc = "done"
		add(n)
	}

	// ---- environment ----
	// S_Requeue
	if p.sweep && s.waiting && s.pending {
		n := s
		n.waiting = false
		n.pending = false
		n.received = true
		add(n)
	}

	return out
}

func (s waitState) key() string {
	return fmt.Sprintf("%v|%v|%v|%s|%s", s.pending, s.waiting, s.received, s.rpc, s.ppc)
}

func waitInit() waitState {
	return waitState{rpc: "checking", ppc: "publishing"}
}

// stranded reports a terminal missed wakeup: R is registered-waiting on a
// committed message, P is done, and no sweep exists to heal it. Mirrors
// NoMissedWakeup in the spec.
func stranded(s waitState, p waitProtocol) bool {
	return s.waiting && s.pending && s.rpc == "done" && s.ppc == "done" && !p.sweep
}

func exploreWait(p waitProtocol) (states int, violation *waitState, trace []string) {
	start := waitInit()
	seen := map[string]bool{start.key(): true}
	parent := map[string]waitState{}
	queue := []waitState{start}
	var all []waitState

	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		states++
		all = append(all, s)
		if stranded(s, p) && violation == nil {
			v := s
			violation = &v
			// keep exploring to also count reachable states
		}
		for _, n := range waitSuccessors(s, p) {
			k := n.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			parent[k] = s
			queue = append(queue, n)
		}
	}

	if violation != nil {
		for cur := *violation; ; {
			k := cur.key()
			prev, has := parent[k]
			if !has {
				break
			}
			trace = append([]string{fmt.Sprintf("%+v", cur)}, trace...)
			cur = prev
		}
	}
	_ = all
	return
}

// canReachReceived checks whether s can still reach a received=true state.
func canReachReceived(s waitState, p waitProtocol) bool {
	seen := map[string]bool{s.key(): true}
	queue := []waitState{s}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.received {
			return true
		}
		for _, n := range waitSuccessors(cur, p) {
			if seen[n.key()] {
				continue
			}
			seen[n.key()] = true
			queue = append(queue, n)
		}
	}
	return false
}

// TestWait_OldDesignMissedWakeup expects a violation of NoMissedWakeup in the
// pre-fix design: the publish lands between the pending check and the wait
// registration, the scan sees no waiter, and R waits forever on a committed
// message.
func TestWait_OldDesignMissedWakeup(t *testing.T) {
	p := waitProtocol{atomicReg: false, scanCanMiss: false, sweep: false}
	states, violation, trace := exploreWait(p)
	t.Logf("explored %d states", states)
	if violation == nil {
		t.Fatal("expected a missed-wakeup violation in the old design")
	}
	for _, step := range trace {
		t.Logf("  %s", step)
	}
}

// TestWait_AtomicRegistration verifies the current design closes the
// check/register gap when eager delivery is reliable.
func TestWait_AtomicRegistration(t *testing.T) {
	p := waitProtocol{atomicReg: true, scanCanMiss: false, sweep: false}
	states, violation, _ := exploreWait(p)
	t.Logf("explored %d states", states)
	if violation != nil {
		t.Fatalf("unexpected violation: %+v", *violation)
	}
}

// TestWait_ResidualGap verifies honestly that atomic registration alone does
// not fully close the gap under read-committed isolation: a publisher's
// waiter scan may still run before the registration commits.
func TestWait_ResidualGap(t *testing.T) {
	p := waitProtocol{atomicReg: true, scanCanMiss: true, sweep: false}
	states, violation, _ := exploreWait(p)
	t.Logf("explored %d states", states)
	if violation == nil {
		t.Fatal("expected the residual missed-wakeup to be reachable")
	}
}

// TestWait_SweepHeals verifies the backstop: with the periodic requeue
// sweep every reachable state can still reach delivery.
func TestWait_SweepHeals(t *testing.T) {
	p := waitProtocol{atomicReg: true, scanCanMiss: true, sweep: true}
	start := waitInit()
	seen := map[string]bool{start.key(): true}
	queue := []waitState{start}
	states := 0
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		states++
		if !canReachReceived(s, p) {
			t.Fatalf("state cannot reach delivery: %+v", s)
		}
		for _, n := range waitSuccessors(s, p) {
			if seen[n.key()] {
				continue
			}
			seen[n.key()] = true
			queue = append(queue, n)
		}
	}
	t.Logf("explored %d states, all can reach delivery", states)
}
