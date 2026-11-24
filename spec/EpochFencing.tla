---- MODULE EpochFencing ----
EXTENDS Naturals, FiniteSets

(***************************************************************************)
(* Epoch fencing protocol.                                                  *)
(*                                                                          *)
(* Every lock acquisition mints a NEW epoch (worker_id#epoch). A fenced    *)
(* write succeeds only while the writer's held epoch equals the stored    *)
(* lock epoch. The critical case: the SAME worker loses the lease and     *)
(* reacquires — it receives a different epoch, so writes attempted with   *)
(* the stale epoch must be rejected even though the worker identity is    *)
(* unchanged.                                                             *)
(*                                                                          *)
(* Configs:                                                                 *)
(*   old: unfenced writes (stale holders can write) -> NoStaleWrite violated*)
(*   new: fenced writes                             -> NoStaleWrite holds   *)
(***************************************************************************)

CONSTANTS Workers,        \* set of worker ids
          Fenced,         \* TRUE: fenced writes; FALSE: unfenced (old)
          MaxEpoch        \* bound for finite checking (>= 2 * Cardinality(Workers))

VARIABLES
    epoch,       \* epoch counter (next epoch = epoch + 1)
    holder,      \* current lock holder worker id ("none" when unlocked)
    heldEpoch,   \* heldEpoch[w]: epoch worker w believes it holds (0 = none)
    wrote,       \* wrote[w]: w has committed a write (bounded flag)
    staleWrite   \* TRUE once a write succeeded under a stale epoch

vars == <<epoch, holder, heldEpoch, wrote, staleWrite>>

NoHolder == "none"

Init ==
    /\ epoch = 0
    /\ holder = NoHolder
    /\ heldEpoch = [w \in Workers |-> 0]
    /\ wrote = [w \in Workers |-> FALSE]
    /\ staleWrite = FALSE

\* Acquire the lock: mints a fresh epoch. Always succeeds (models the
\* atomic conditional-UPDATE claim — the old holder's lease is gone).
Acquire(w) ==
    /\ epoch < MaxEpoch
    /\ epoch' = epoch + 1
    /\ holder' = w
    /\ heldEpoch' = [heldEpoch EXCEPT ![w] = epoch']
    /\ UNCHANGED <<wrote, staleWrite>>

\* Lease expiry: the holder loses the lock but keeps its (now stale) epoch
\* credential — this is what makes the reacquire case interesting.
Expire ==
    /\ holder # NoHolder
    /\ holder' = NoHolder
    /\ UNCHANGED <<epoch, heldEpoch, wrote, staleWrite>>

\* Fenced write attempt: succeeds iff the writer's epoch is current.
WWrite(w) ==
    LET stale == holder # w \/ heldEpoch[w] # epoch
        unfenced == ~Fenced /\ stale /\ heldEpoch[w] > 0
    IN
        IF Fenced
        THEN /\ ~wrote[w]
             /\ holder = w /\ heldEpoch[w] = epoch
             /\ wrote' = [wrote EXCEPT ![w] = TRUE]
        ELSE /\ heldEpoch[w] > 0
             /\ wrote' = [wrote EXCEPT ![w] = TRUE]
             /\ staleWrite' = staleWrite \/ (holder # w \/ heldEpoch[w] # epoch)
    /\ UNCHANGED <<epoch, holder, heldEpoch>>

Next == \E w \in Workers : Acquire(w) \/ WWrite(w) \/ Expire

Spec == Init /\ [][Next]_vars /\ WF_vars(Next)

\* Core safety: no write ever commits under a stale epoch — including
\* writes by a worker that lost and reacquired the lock.
NoStaleWrite == ~staleWrite

\* Liveness: whoever currently holds the lock can write.
WriteProgress == [](holder # NoHolder => <>(\E w \in Workers : wrote[w]))

====
