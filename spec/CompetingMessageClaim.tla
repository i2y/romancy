---- MODULE CompetingMessageClaim ----
EXTENDS Naturals, FiniteSets

(***************************************************************************)
(* Competing-channel message claim protocol.                                *)
(*                                                                          *)
(* One message, N competing consumers. A consumer must claim the message    *)
(* before recording receive history; recording consumes (deletes) it.       *)
(*                                                                          *)
(* The bug class under test: a consumer that claims and dies before         *)
(* consuming leaves an orphan claim. Without takeover rules (stale claim,   *)
(* terminal owner) the message is hidden from every other consumer          *)
(* forever.                                                                 *)
(*                                                                          *)
(* Configs:                                                                 *)
(*   old: claim once, never take over           -> liveness violated        *)
(*   new: takeover on stale/terminal            -> progress holds           *)
(***************************************************************************)

CONSTANTS Consumers,      \* set of consumer ids
          Takeover        \* TRUE: stale/terminal takeover enabled (new protocol)

VARIABLES
    msgState,    \* "unclaimed" | "claimed" | "consumed"
    owner,       \* current claim holder ("none" when unclaimed)
    fresh,       \* claim freshness: FALSE models past the stale threshold
    alive,       \* alive[c]: consumer c has not died
    consumedBy   \* consumers that recorded receive history (the consume event)

vars == <<msgState, owner, fresh, alive, consumedBy>>

NoOwner == "none"

Init ==
    /\ msgState = "unclaimed"
    /\ owner = NoOwner
    /\ fresh = FALSE
    /\ alive = [c \in Consumers |-> TRUE]
    /\ consumedBy = {}

(***************************************************************************)
(* Claim: succeeds for consumer c when the message is unclaimed, or the     *)
(* existing claim is voidable: same owner (idempotent reclaim), stale       *)
(* claim, or a claim whose owner reached a terminal state. A fresh claim    *)
(* by another live consumer is the only thing that blocks.                  *)
(***************************************************************************)
Claim(c) ==
    /\ alive[c]
    /\ msgState /= "consumed"
    /\ \/ msgState = "unclaimed"
       \/ /\ Takeover
          /\ msgState = "claimed"
          /\ \/ owner = c
             \/ ~fresh
             \/ ~alive[owner]
    /\ msgState' = "claimed"
    /\ owner' = c
    /\ fresh' = TRUE
    /\ UNCHANGED <<alive, consumedBy>>

(***************************************************************************)
(* Consume: the claim owner records receive history and deletes the         *)
(* message+claim. Atomic enough for the model: history recorded and row     *)
(* deleted in one step. The owner's own claim stays consumable regardless   *)
(* of its freshness.                                                        *)
(***************************************************************************)
Consume(c) ==
    /\ alive[c]
    /\ msgState = "claimed"
    /\ owner = c
    /\ msgState' = "consumed"
    /\ consumedBy' = consumedBy \cup {c}
    /\ UNCHANGED <<owner, fresh, alive>>

(***************************************************************************)
(* Time passing: a held claim ages past the staleness threshold.            *)
(***************************************************************************)
Expire ==
    /\ msgState = "claimed"
    /\ fresh
    /\ fresh' = FALSE
    /\ UNCHANGED <<msgState, owner, alive, consumedBy>>

(***************************************************************************)
(* A consumer dies (process crash or its workflow reaches a terminal        *)
(* status) possibly while holding a claim. It never revives.                *)
(***************************************************************************)
Die(c) ==
    /\ alive[c]
    /\ alive' = [alive EXCEPT ![c] = FALSE]
    /\ UNCHANGED <<msgState, owner, fresh, consumedBy>>

Next ==
    \/ \E c \in Consumers : Claim(c)
    \/ \E c \in Consumers : Consume(c)
    \/ \E c \in Consumers : Die(c)
    \/ Expire

(***************************************************************************)
(* Safety: at most one consumer ever records the message — the              *)
(* "exactly one subscriber" guarantee of competing mode.                    *)
(***************************************************************************)
AtMostOneConsume == Cardinality(consumedBy) =< 1

(***************************************************************************)
(* Liveness: the message is eventually consumed provided some consumer     *)
(* stays alive forever and keeps trying. Modelled weakly: if at least one  *)
(* consumer is permanently alive, the message must eventually be consumed.  *)
(* Fairness on claims is assumed; without it a deadlocked claimant could   *)
(* hold the message while everyone waits.                                   *)
(***************************************************************************)
SomeAliveForever == \E c \in Consumers : []<>(alive[c])

ConsumedEventually == SomeAliveForever ~> (msgState = "consumed")

(***************************************************************************)
(* Fairness: a live consumer eventually claims when it can (WF on Claim),  *)
(* and a consumer that is infinitely often able to consume does so (SF on  *)
(* Consume — without it, two consumers could pass a stale claim back and   *)
(* forth forever, each one's Consume enabled only during its own claim).   *)
(* Die and Expire are environment actions: they MAY happen, progress must  *)
(* hold whether or not they do.                                            *)
(***************************************************************************)
Spec == Init /\ [][Next]_vars
        /\ \A c \in Consumers : WF_vars(Claim(c)) /\ SF_vars(Consume(c))

====
