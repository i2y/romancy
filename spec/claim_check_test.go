// Package spec model-checks the workflow-instance claim protocol described
// in InstanceClaim.tla by exhaustive state-space exploration in Go. The
// transition relation mirrors the spec action for action, so the tests
// establish the same results TLC would report:
//
//   - the pre-fix design (non-atomic claim, unfenced writes) violates both
//     safety invariants;
//   - the current design (atomic conditional claim, fenced writes,
//     heartbeat) has no reachable violation, no deadlock, and can always
//     still finish the instance.
//
// Run TLC on the spec proper when a Java toolchain is available:
//
//	java -jar tla2tools.jar -config InstanceClaim_new.cfg InstanceClaim.tla
package spec

import (
	"strings"
	"testing"
)

const nWorkers = 2
const null = -1

// claimState mirrors the spec variables.
type claimState struct {
	holder     int // Workers index, or null
	expired    bool
	believes   [nWorkers]bool
	pc         [nWorkers]string // idle, read, running, aborted, done
	sawFree    [nWorkers]bool
	legit      [nWorkers]bool
	wrote      [nWorkers]bool
	staleWrite bool
}

type protocol struct {
	atomic bool // conditional-UPDATE claim
	fenced bool // writes conditioned on holder
}

func freeFor(s claimState, w int) bool {
	return s.holder == null || s.expired || s.holder == w
}

// successors returns the states reachable in one step, one entry per fired
// action. It mirrors Next in InstanceClaim.tla.
func successors(s claimState, p protocol) []claimState {
	var out []claimState
	add := func(n claimState) { out = append(out, n) }

	for w := 0; w < nWorkers; w++ {
		// ---- claim ----
		if p.atomic {
			// TryAcquire(w)
			if s.pc[w] == "idle" {
				n := s
				if freeFor(s, w) {
					n.holder = w
					n.expired = false
					n.believes[w] = true
					n.legit[w] = true
					n.pc[w] = "running"
				}
				add(n)
			}
		} else {
			// ReadLock(w)
			if s.pc[w] == "idle" {
				n := s
				n.pc[w] = "read"
				n.sawFree[w] = freeFor(s, w)
				add(n)
			}
			// Claim(w): acts on the possibly stale read; legit records what
			// a conditional claim would have seen at this instant.
			if s.pc[w] == "read" {
				n := s
				n.sawFree[w] = false
				if s.sawFree[w] {
					n.legit[w] = freeFor(s, w)
					n.holder = w
					n.expired = false
					n.believes[w] = true
					n.pc[w] = "running"
				} else {
					n.pc[w] = "idle"
				}
				add(n)
			}
		}

		// ---- run ----
		// Renew(w): heartbeat extends a held lease.
		if s.pc[w] == "running" && s.holder == w {
			n := s
			n.expired = false
			add(n)
		}
		// Write(w)
		if s.pc[w] == "running" && s.believes[w] {
			if p.fenced {
				if s.holder == w {
					n := s
					n.wrote[w] = true
					add(n)
				}
			} else {
				n := s
				n.wrote[w] = true
				if s.holder != w {
					n.staleWrite = true
				}
				add(n)
			}
		}
		// Abort(w): heartbeat/ErrLockNotHeld ends a run that lost the row.
		if s.pc[w] == "running" && s.holder != w {
			n := s
			n.believes[w] = false
			n.pc[w] = "aborted"
			add(n)
		}
		// Finish(w): release is conditional on still holding.
		if s.pc[w] == "running" {
			n := s
			n.believes[w] = false
			n.pc[w] = "done"
			if s.holder == w {
				n.holder = null
				n.expired = false
			}
			add(n)
		}
	}

	// ExpireLease: abstract tick; may fire while the holder still runs.
	if s.holder != null && !s.expired {
		n := s
		n.expired = true
		add(n)
	}

	return out
}

func (s claimState) key() string {
	var b strings.Builder
	b.WriteString(itoa(s.holder))
	b.WriteByte('|')
	writeBool(&b, s.expired)
	for w := 0; w < nWorkers; w++ {
		b.WriteByte('|')
		b.WriteString(s.pc[w])
		writeBool(&b, s.believes[w])
		writeBool(&b, s.sawFree[w])
		writeBool(&b, s.legit[w])
		writeBool(&b, s.wrote[w])
	}
	b.WriteByte('|')
	writeBool(&b, s.staleWrite)
	return b.String()
}

func writeBool(b *strings.Builder, v bool) {
	if v {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
}

func itoa(i int) string {
	if i == null {
		return "N"
	}
	return string(rune('a' + i))
}

func initState() claimState {
	return claimState{
		holder: null,
		pc:     [nWorkers]string{"idle", "idle"},
		legit:  [nWorkers]bool{true, true},
	}
}

// explore runs BFS over the reachable state space. It calls check on every
// state; a false return aborts the search and reports the violating state
// plus the action trace that produced it.
func explore(p protocol, check func(claimState) bool) (states int, bad claimState, trace []string, ok bool) {
	start := initState()
	seen := map[string]bool{start.key(): true}
	parent := map[string]claimState{}
	parentAct := map[string]string{}
	queue := []claimState{start}
	ok = true

	for len(queue) > 0 && ok {
		s := queue[0]
		queue = queue[1:]
		states++
		if !check(s) {
			bad, ok = s, false
			break
		}
		for _, n := range successors(s, p) {
			k := n.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			parent[k] = s
			parentAct[k] = actionLabel(s, n)
			queue = append(queue, n)
		}
	}

	if !ok {
		for cur := bad; ; {
			k := cur.key()
			act, has := parentAct[k]
			if !has {
				break
			}
			trace = append([]string{describe(cur, act)}, trace...)
			cur = parent[k]
		}
	}
	return
}

// actionLabel describes which spec action turned s into n.
func actionLabel(s, n claimState) string {
	switch {
	case s.holder != n.holder && n.holder != null && n.pc != s.pc:
		return "claim"
	case s.holder != n.holder && n.holder == null:
		return "finish(release)"
	case !s.expired && n.expired:
		return "expire"
	case s.pc != n.pc:
		for w := 0; w < nWorkers; w++ {
			if s.pc[w] != n.pc[w] {
				return "pc:" + s.pc[w] + "->" + n.pc[w]
			}
		}
		return "step"
	default:
		return "write/renew"
	}
}

func describe(s claimState, act string) string {
	return act + " => holder=" + itoa(s.holder) +
		" expired=" + tf(s.expired) +
		" pc=" + s.pc[0] + "," + s.pc[1] +
		" wrote=" + tf(s.wrote[0]) + "," + tf(s.wrote[1]) +
		" legit=" + tf(s.legit[0]) + "," + tf(s.legit[1]) +
		" staleWrite=" + tf(s.staleWrite)
}

func tf(b bool) string {
	if b {
		return "t"
	}
	return "f"
}

// safety combines both spec invariants.
func safety(s claimState) bool {
	if s.staleWrite {
		return false // NoStaleWrite
	}
	for w := 0; w < nWorkers; w++ {
		if s.wrote[w] && !s.legit[w] {
			return false // LegitimateWriters
		}
	}
	return true
}

// TestClaimOldDesignViolates reproduces the bug that motivated the fix:
// under the non-atomic claim and unfenced writes, TLC's counterpart finds a
// state where a worker's write lands while it does not hold the row.
func TestClaimOldDesignViolates(t *testing.T) {
	states, bad, trace, ok := explore(protocol{atomic: false, fenced: false}, safety)
	if ok {
		t.Fatalf("old design: expected a safety violation in %d states, found none", states)
	}
	t.Logf("old design: violation after %d states, trace (%d steps):", states, len(trace))
	for _, step := range trace {
		t.Logf("  %s", step)
	}
	_ = bad
}

// TestClaimNewDesignSafe exhausts the state space of the fixed protocol:
// no stale write and no illegitimate write is reachable.
func TestClaimNewDesignSafe(t *testing.T) {
	states, _, trace, ok := explore(protocol{atomic: true, fenced: true}, safety)
	if !ok {
		t.Fatalf("new design: safety violated; trace:")
		for _, step := range trace {
			t.Logf("  %s", step)
		}
	}
	t.Logf("new design: %d states explored, safety holds", states)
}

// TestClaimNewDesignLiveness checks the bounded analogue of the spec's
// / Liveness property: from every reachable state the instance can still be
// / finished by some worker (no reachable dead end), and the system never
// / deadlocks.
func TestClaimNewDesignLiveness(t *testing.T) {
	p := protocol{atomic: true, fenced: true}
	start := initState()
	seen := map[string]bool{start.key(): true}
	queue := []claimState{start}

	var states int
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		states++

		succ := successors(s, p)
		if len(succ) == 0 {
			// No enabled action. That is only acceptable in a terminal
			// state where the instance actually finished.
			allTerminal, someDone := true, false
			for w := 0; w < nWorkers; w++ {
				if s.pc[w] != "done" && s.pc[w] != "aborted" {
					allTerminal = false
				}
				if s.pc[w] == "done" {
					someDone = true
				}
			}
			if !allTerminal || !someDone {
				t.Fatalf("stuck state reached: holder=%d pc=%v", s.holder, s.pc)
			}
		}
		if !reachableDone(s, p) {
			t.Fatalf("some worker cannot reach done from: holder=%d expired=%v pc=%v",
				s.holder, s.expired, s.pc)
		}

		for _, n := range succ {
			k := n.key()
			if !seen[k] {
				seen[k] = true
				queue = append(queue, n)
			}
		}
	}
	t.Logf("new design: %d states, finish reachable from all of them", states)
}

// reachableDone reports whether some state with a worker in "done" is
// reachable from s.
func reachableDone(s claimState, p protocol) bool {
	for w := 0; w < nWorkers; w++ {
		if s.pc[w] == "done" {
			return true
		}
	}
	seen := map[string]bool{s.key(): true}
	queue := []claimState{s}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range successors(cur, p) {
			for w := 0; w < nWorkers; w++ {
				if n.pc[w] == "done" {
					return true
				}
			}
			k := n.key()
			if !seen[k] {
				seen[k] = true
				queue = append(queue, n)
			}
		}
	}
	return false
}
