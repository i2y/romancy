---- MODULE TaskClaim ----
EXTENDS Naturals, FiniteSets

(***************************************************************************)
(* Task claim protocol (actor_tasks).                                       *)
(*                                                                          *)
(* Workers compete for open tasks on a channel. The real implementation    *)
(* claims via a single atomic statement:                                     *)
(*   UPDATE actor_tasks SET state='claimed', claimed_by=w                  *)
(*   WHERE id = (SELECT id FROM actor_tasks                                *)
(*               WHERE channel=c AND state='open'                          *)
(*               ORDER BY created_at LIMIT 1) AND state='open'             *)
(* which we model as a non-atomic PICK (select a candidate) followed by    *)
(* a CLAIM — CAS-guarded or not. The interesting race: two workers pick    *)
(* the same open task and both attempt to claim it.                        *)
(*                                                                          *)
(* Configs:                                                                 *)
(*   old: unconditional claim write (no CAS guard) -> violated             *)
(*   new: claim only when state is still 'open'    -> holds                *)
(*                                                                          *)
(* Orphan reclaim is deliberately not modeled: with reclaim, two workers   *)
(* may legitimately hold the same task at different epochs. SingleClaimant *)
(* here means "no two workers both believe they won the SAME claim".       *)
(***************************************************************************)

CONSTANTS Workers,   \* set of worker ids
          Tasks,     \* set of task ids
          CAS        \* TRUE: guarded claim; FALSE: racy write

VARIABLES
    state,      \* state[t] in {"open","claimed","done"}
    claimedBy,  \* claimedBy[t] in Workers \/ {none}
    picked,     \* picked[w]: candidate task w's SELECT returned (none when idle)
    won,        \* won[w]: set of tasks w's claim returned success for
    violated    \* TRUE once two workers won the same task

vars == <<state, claimedBy, picked, won, violated>>

None == "none"

Init ==
    /\ state = [t \in Tasks |-> "open"]
    /\ claimedBy = [t \in Tasks |-> None]
    /\ picked = [w \in Workers |-> None]
    /\ won = [w \in Workers |-> {}]
    /\ violated = FALSE

\* Non-atomic candidate selection: w observes an open task.
Pick(w, t) ==
    /\ picked[w] = None
    /\ state[t] = "open"
    /\ picked' = [picked EXCEPT ![w] = t]
    /\ UNCHANGED <<state, claimedBy, won, violated>>

\* Claim attempt. CAS: succeeds only if the task is STILL open — models
\* the AND state='open' guard. Non-CAS: the write always lands.
Claim(w) ==
    LET t == picked[w]
        success == CAS => (state[t] = "open")
    IN
        /\ t # None
        /\ IF success
           THEN /\ state' = [state EXCEPT ![t] = "claimed"]
                /\ claimedBy' = [claimedBy EXCEPT ![t] = w]
                /\ won' = [won EXCEPT ![w] = @ \cup {t}]
                /\ violated' = \/ violated
                               \/ \E w2 \in Workers \ {w}: t \in won[w2]
           ELSE UNCHANGED <<state, claimedBy, won, violated>>
        /\ picked' = [picked EXCEPT ![w] = None]

\* The claimant finishes its task.
Complete(w, t) ==
    /\ state[t] = "claimed"
    /\ claimedBy[t] = w
    /\ state' = [state EXCEPT ![t] = "done"]
    /\ UNCHANGED <<claimedBy, picked, won, violated>>

Next ==
    \/ \E w \in Workers, t \in Tasks : Pick(w, t)
    \/ \E w \in Workers : Claim(w)
    \/ \E w \in Workers, t \in Tasks : Complete(w, t)

Spec == Init /\ [][Next]_vars /\ WF_vars(Next)

\* Core safety: no two workers both believe they claimed the same task.
SingleClaimant == ~violated

\* The recorded claimant really did win its claim.
ClaimantIsWinner ==
    \A t \in Tasks : (state[t] = "claimed") => claimedBy[t] \in Workers

====
