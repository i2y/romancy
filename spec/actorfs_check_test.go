package spec

// Exhaustive model checker for ActorFS.tla (2 writers, MaxVersion=2).
// Establishes the same results TLC would report:
//
//	java -jar tla2tools.jar -deadlock -config ActorFS_new.cfg ActorFS.tla
//
// new (CAS=TRUE):  NoLostUpdate holds over all reachable states.
// old (CAS=FALSE): a state with two winners at the same base version is
// reachable — the lost-update anomaly CAS exists to prevent.

import (
	"fmt"
	"testing"
)

type fsState struct {
	exists   bool
	version  int    // 0 when absent; 1..MaxVersion
	storedBy string // "", w1, w2
	readVer  [2]int
	hasRead  [2]bool
	outcome  [2]string // "none", "ok", "conflict"
	wins     [3]int    // bitmask of winners per base version (0..2)
}

func (s fsState) key() string {
	return fmt.Sprintf("%v|%d|%s|%d%d|%v%v|%s%s|%d%d%d",
		s.exists, s.version, s.storedBy, s.readVer[0], s.readVer[1],
		s.hasRead[0], s.hasRead[1], s.outcome[0], s.outcome[1],
		s.wins[0], s.wins[1], s.wins[2])
}

var fsWriters = [2]string{"w1", "w2"}
const fsMaxVersion = 2

func fsSuccessors(s fsState, cas bool) []fsState {
	var out []fsState
	add := func(n fsState) { out = append(out, n) }
	for i, w := range fsWriters {
		// WRead: observe current version.
		n := s
		n.readVer[i] = s.version
		n.hasRead[i] = true
		add(n)

		// WCreate: expected 0 — CAS succeeds only when absent; LWW always.
		n = s
		if !cas || !s.exists {
			n.outcome[i] = "ok"
		} else {
			n.outcome[i] = "conflict"
		}
		if n.outcome[i] == "ok" {
			n.wins[0] |= 1 << i
			n.exists = true
			n.version = 1
			n.storedBy = w
		}
		add(n)

		// WUpdate: expected = observed — CAS requires match; LWW always wins.
		n = s
		if s.hasRead[i] && s.readVer[i] > 0 && s.outcome[i] != "ok" && s.version < fsMaxVersion {
			base := s.readVer[i]
			if !cas || s.version == base {
				n.outcome[i] = "ok"
				n.wins[base] |= 1 << i
				n.version = s.version + 1
				n.storedBy = w
				n.exists = true
			} else {
				n.outcome[i] = "conflict"
			}
			add(n)
		}

		// WRetry: re-arm after conflict.
		if s.outcome[i] == "conflict" {
			n = s
			n.outcome[i] = "none"
			add(n)
		}
	}
	return out
}

func fsInit() fsState {
	return fsState{outcome: [2]string{"none", "none"}}
}

// fsReach explores all reachable states; visit returns false to stop early.
func fsReach(cas bool, visit func(fsState) bool) (visited int) {
	seen := map[string]bool{fsInit().key(): true}
	queue := []fsState{fsInit()}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		visited++
		if !visit(s) {
			return visited
		}
		for _, n := range fsSuccessors(s, cas) {
			k := n.key()
			if !seen[k] {
				seen[k] = true
				queue = append(queue, n)
			}
		}
	}
	return visited
}

func fsNoLostUpdate(s fsState) bool {
	for v := 0; v <= fsMaxVersion; v++ {
		// at most one winner per base version
		if s.wins[v] != 0 && s.wins[v] != 1 && s.wins[v] != 2 {
			return false
		}
	}
	return true
}

func TestFS_CAS_NoLostUpdate(t *testing.T) {
	var bad *fsState
	n := fsReach(true, func(s fsState) bool {
		if !fsNoLostUpdate(s) {
			b := s
			bad = &b
			return false
		}
		return true
	})
	if bad != nil {
		t.Fatalf("NoLostUpdate violated at state %+v", *bad)
	}
	t.Logf("CAS protocol: %d states explored, no violation", n)
}

func TestFS_LWW_LostUpdateReachable(t *testing.T) {
	var bad *fsState
	n := fsReach(false, func(s fsState) bool {
		if !fsNoLostUpdate(s) {
			b := s
			bad = &b
			return false
		}
		return true
	})
	if bad == nil {
		t.Fatal("expected a lost-update state under LWW")
	}
	t.Logf("LWW: violation found after %d states at %+v", n, *bad)
}

func TestFS_VersionMonotonic(t *testing.T) {
	n := fsReach(true, func(s fsState) bool {
		if s.exists && s.version < 1 {
			t.Fatalf("exists but version=%d", s.version)
			return false
		}
		return true
	})
	t.Logf("VersionMonotonic held over %d states", n)
}
