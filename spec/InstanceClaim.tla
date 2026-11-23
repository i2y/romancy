----------------------------- MODULE InstanceClaim -----------------------------
\* Model of romancy's workflow-instance claim protocol.
\*
\* Workers acquire a lease on a workflow_instances row (locked_by /
\* lock_expires_at), run the workflow while holding it, and append history /
\* update status / write output. Both designs live in one model, selected by
\* constants:
\*
\*   AtomicClaim = FALSE, Fenced = FALSE   the pre-fix design:
\*       TryAcquireLock read the row, decided it was free, then issued a
\*       separate UPDATE -- two workers can both pass the check and both
\*       believe they hold the lock. Writes were unconditional, so a worker
\*       that lost its lease could still append history over the new owner.
\*
\*   AtomicClaim = TRUE,  Fenced = TRUE    the current design:
\*       TryAcquireLock is a single conditional UPDATE ... WHERE the row is
\*       still claimable (SQLite/PostgreSQL/MySQL all use RowsAffected as the
\*       verdict). A heartbeat renews the lease while running; every history /
\*       status / output write is conditioned on `locked_by = worker`, so a
\*       stale worker's writes no longer land.
\*
\* Abstract time: the lease is either alive or expired; ExpireLease is a
\* nondeterministic tick that can fire while the holder still runs.
\*
\* Check with TLC (see spec/*.cfg; -deadlock because workers that finish or
\* abort legitimately go idle):
\*   java -jar tla2tools.jar -deadlock -config InstanceClaim_old.cfg InstanceClaim.tla
\*   -> verified: invariant violation, a 6-step counterexample where both
\*      workers pass the stale read, both believe they hold the lock, and the
\*      non-holder's write lands (claim race in the pre-fix design)
\*   java -jar tla2tools.jar -deadlock -config InstanceClaim_new.cfg InstanceClaim.tla
\*   -> verified: no errors over all 81 distinct states
\*   java -jar tla2tools.jar -deadlock -config InstanceClaim_live.cfg InstanceClaim.tla
\*   -> verified: the liveness property holds
\* The same state machine is model-checked from Go in claim_check_test.go
\* so `go test ./spec` verifies the design without a Java toolchain.

EXTENDS Naturals

CONSTANTS Workers,      \* set of worker identities
          NULL,         \* no-holder marker (model value, distinct from Workers)
          AtomicClaim,  \* BOOLEAN: single conditional-UPDATE claim?
          Fenced        \* BOOLEAN: writes conditioned on `locked_by = w`?

VARIABLES lockHolder,   \* row's locked_by: Workers \cup {NULL}
          leaseExpired, \* row's lease state: BOOLEAN
          believes,     \* [Workers -> BOOLEAN] worker thinks it is running
          pc,           \* [Workers -> {"idle","read","running","aborted","done"}]
          sawFree,      \* [Workers -> BOOLEAN] stale read result (old design)
          legit,        \* [Workers -> BOOLEAN] the claim itself was legitimate
          wrote,        \* [Workers -> BOOLEAN] the DB accepted a write of w's
          staleWrite    \* BOOLEAN: a non-holder's write was accepted

vars == <<lockHolder, leaseExpired, believes, pc, sawFree, legit, wrote, staleWrite>>

\* Is the row claimable by w right now?
FreeFor(w) == lockHolder = NULL \/ leaseExpired \/ lockHolder = w

Init ==
    /\ lockHolder   = NULL
    /\ leaseExpired = FALSE
    /\ believes     = [w \in Workers |-> FALSE]
    /\ pc           = [w \in Workers |-> "idle"]
    /\ sawFree      = [w \in Workers |-> FALSE]
    /\ legit        = [w \in Workers |-> TRUE]
    /\ wrote        = [w \in Workers |-> FALSE]
    /\ staleWrite   = FALSE

\* ---- claim ----------------------------------------------------------

\* Fixed design: check and claim are one database operation.
\* The UPDATE either matches (row was free) and installs w, or matches no row
\* and w retries -- exactly RowsAffected > 0 in the Go code.
TryAcquire(w) ==
    /\ AtomicClaim
    /\ pc[w] = "idle"
    /\ IF FreeFor(w)
       THEN /\ lockHolder'   = w
            /\ leaseExpired' = FALSE
            /\ believes'     = [believes EXCEPT ![w] = TRUE]
            /\ legit'        = [legit EXCEPT ![w] = TRUE]
            /\ pc'           = [pc EXCEPT ![w] = "running"]
       ELSE /\ pc'    = [pc EXCEPT ![w] = "idle"]  \* busy; stays eligible
            /\ UNCHANGED <<lockHolder, leaseExpired, believes, legit>>
    /\ UNCHANGED <<sawFree, wrote, staleWrite>>
    \* legit is set TRUE only because FreeFor(w) held at that instant.

\* Old design step 1: read the row and remember whether it looked free.
ReadLock(w) ==
    /\ ~AtomicClaim
    /\ pc[w] = "idle"
    /\ pc'      = [pc EXCEPT ![w] = "read"]
    /\ sawFree' = [sawFree EXCEPT ![w] = FreeFor(w)]
    /\ UNCHANGED <<lockHolder, leaseExpired, believes, legit, wrote, staleWrite>>

\* Old design step 2: act on the stale read.
\* Between ReadLock and Claim another worker may have installed itself; the
\* UPDATE still fires, overwriting locked_by. `legit` records what a
\* conditional claim would have seen -- FALSE marks a stolen live lease.
Claim(w) ==
    /\ ~AtomicClaim
    /\ pc[w] = "read"
    /\ IF sawFree[w]
       THEN /\ legit'    = [legit EXCEPT ![w] = FreeFor(w)]
            /\ lockHolder'   = w
            /\ leaseExpired' = FALSE
            /\ believes'     = [believes EXCEPT ![w] = TRUE]
            /\ pc'           = [pc EXCEPT ![w] = "running"]
       ELSE /\ pc'    = [pc EXCEPT ![w] = "idle"]
            /\ UNCHANGED <<lockHolder, leaseExpired, believes, legit>>
    /\ sawFree' = [sawFree EXCEPT ![w] = FALSE]
    /\ UNCHANGED <<wrote, staleWrite>>

\* ---- run ------------------------------------------------------------

\* Abstract tick: any live lease can expire (the 300 s lease has no
\* relationship to how long the activity runs). The holder keeps believing;
\* only the row changes.
ExpireLease ==
    /\ lockHolder # NULL
    /\ ~leaseExpired
    /\ leaseExpired' = TRUE
    /\ UNCHANGED <<lockHolder, believes, pc, sawFree, legit, wrote, staleWrite>>

\* Heartbeat: the holder extends its lease (RefreshLock).
Renew(w) ==
    /\ pc[w] = "running"
    /\ lockHolder = w
    /\ leaseExpired' = FALSE
    /\ UNCHANGED <<lockHolder, believes, pc, sawFree, legit, wrote, staleWrite>>

\* A running worker appends history / updates status / writes output.
\* Fenced: the write is the conditional UPDATE ... WHERE locked_by = w, so it
\* is enabled only while w is the holder. Unfenced: always enabled, and a
\* write by a non-holder is recorded as the staleWrite violation.
Write(w) ==
    /\ pc[w] = "running"
    /\ believes[w]
    /\ IF Fenced
       THEN /\ lockHolder = w
            /\ wrote'     = [wrote EXCEPT ![w] = TRUE]
            /\ staleWrite' = staleWrite
       ELSE /\ wrote'     = [wrote EXCEPT ![w] = TRUE]
            /\ staleWrite' = staleWrite \/ lockHolder # w
    /\ UNCHANGED <<lockHolder, leaseExpired, believes, pc, sawFree, legit>>

\* The heartbeat or a rejected write (ErrLockNotHeld) aborts the run.
Abort(w) ==
    /\ pc[w] = "running"
    /\ lockHolder # w
    /\ believes' = [believes EXCEPT ![w] = FALSE]
    /\ pc'       = [pc EXCEPT ![w] = "aborted"]
    /\ UNCHANGED <<lockHolder, leaseExpired, sawFree, legit, wrote, staleWrite>>

\* The run finishes and releases conditionally (ReleaseLock only clears the
\* row while w is still the holder).
Finish(w) ==
    /\ pc[w] = "running"
    /\ believes' = [believes EXCEPT ![w] = FALSE]
    /\ pc'       = [pc EXCEPT ![w] = "done"]
    /\ IF lockHolder = w
       THEN /\ lockHolder'   = NULL
            /\ leaseExpired' = FALSE
       ELSE UNCHANGED <<lockHolder, leaseExpired>>
    /\ UNCHANGED <<sawFree, legit, wrote, staleWrite>>

Next ==
    \/ \E w \in Workers :
        \/ TryAcquire(w) \/ ReadLock(w) \/ Claim(w)
        \/ Renew(w) \/ Write(w) \/ Abort(w) \/ Finish(w)
    \/ ExpireLease

Spec == Init /\ [][Next]_vars

\* ---- properties ------------------------------------------------------

\* SAFETY 1: the DB never accepted a write from a worker that did not hold
\* the row at write time. Violated by the old design both via the claim race
\* (two believers, only one is locked_by) and via lease expiry without
\* fencing (stale holder writes after a takeover).
NoStaleWrite == ~staleWrite

\* SAFETY 2: no worker's accepted write follows an illegitimate claim -- the
\* non-atomic claim can install a worker over a live lease; the conditional
\* UPDATE cannot.
LegitimateWriters == \A w \in Workers : wrote[w] => legit[w]

\* LIVENESS: a resumable instance eventually finishes on some worker.
\* Checked under per-worker fairness on attempting and finishing.
Attempt(w) == TryAcquire(w) \/ ReadLock(w) \/ Claim(w)
FairSpec == Spec
    /\ \A w \in Workers : WF_vars(Attempt(w)) /\ WF_vars(Finish(w))
SomeDone == \E w \in Workers : pc[w] = "done"
Liveness == <>(SomeDone)

=============================================================================
