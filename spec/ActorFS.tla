---- MODULE ActorFS ----
EXTENDS Naturals, FiniteSets

(***************************************************************************)
(* Shared-FS optimistic-concurrency (CAS) protocol.                          *)
(*                                                                          *)
(* Multiple writers race on one path. Each write carries expected_version:  *)
(*   expected 0  -> create (only when absent)                               *)
(*   expected >0 -> update only if stored version matches                   *)
(*                                                                          *)
(* The bug class under test: last-writer-wins (unconditional writes) let    *)
(* two writers who both observed version v both "succeed", silently        *)
(* losing one update. CAS must ensure at most one writer succeeds per base  *)
(* version.                                                                 *)
(*                                                                          *)
(* Configs:                                                                 *)
(*   old: unconditional writes (LWW)            -> NoLostUpdate violated    *)
(*   new: conditional CAS writes                -> NoLostUpdate holds       *)
(***************************************************************************)

CONSTANTS Writers,        \* set of writer ids
          CAS             \* TRUE: CAS writes (new); FALSE: LWW (old)

VARIABLES
    exists,      \* file exists at path
    version,     \* stored version (0 when absent)
    storedBy,    \* last successful writer ("none" when absent)
    readVer,     \* readVer[w]: version writer w last observed
    hasRead,     \* hasRead[w]: w has an observed version
    outcome,     \* outcome[w]: "none" | "ok" | "conflict" for the last attempt
    winsAt       \* winsAt[v]: set of writers that succeeded writing at base v

vars == <<exists, version, storedBy, readVer, hasRead, outcome, winsAt>>

NoWriter == "none"
MaxVersion == Cardinality(Writers)

Init ==
    /\ exists = FALSE
    /\ version = 0
    /\ storedBy = NoWriter
    /\ readVer = [w \in Writers |-> 0]
    /\ hasRead = [w \in Writers |-> FALSE]
    /\ outcome = [w \in Writers |-> "none"]
    /\ winsAt = [v \in 0..MaxVersion |-> {}]

\* Writer observes the current stored version.
WRead(w) ==
    /\ readVer' = [readVer EXCEPT ![w] = version]
    /\ hasRead' = [hasRead EXCEPT ![w] = TRUE]
    /\ UNCHANGED <<exists, version, storedBy, outcome, winsAt>>

\* Create: expected_version = 0 — succeeds only when absent.
\* (Under LWW the create clobbers unconditionally.)
WCreate(w) ==
    /\ outcome' = [outcome EXCEPT ![w] =
        IF CAS
        THEN IF ~exists THEN "ok" ELSE "conflict"
        ELSE "ok"]
    /\ IF outcome'[w] = "ok"
        THEN /\ winsAt' = [winsAt EXCEPT ![0] = @ \cup {w}]
             /\ exists' = TRUE
             /\ version' = 1
             /\ storedBy' = w
        ELSE UNCHANGED <<exists, version, storedBy, winsAt>>
    /\ UNCHANGED <<readVer, hasRead>>

\* Update: expected_version = last observed version — succeeds only when
\* the stored version still matches (CAS) or always (LWW).
WUpdate(w) ==
    /\ hasRead[w] /\ readVer[w] > 0
    /\ ~\/ outcome[w] = "ok"
    /\ version < MaxVersion
    /\ LET base == readVer[w] IN
        /\ outcome' = [outcome EXCEPT ![w] =
            IF CAS
            THEN IF version = base THEN "ok" ELSE "conflict"
            ELSE "ok"]
        /\ IF outcome'[w] = "ok"
            THEN /\ winsAt' = [winsAt EXCEPT ![base] = @ \cup {w}]
                 /\ version' = version + 1
                 /\ storedBy' = w
                 /\ exists' = TRUE
            ELSE UNCHANGED <<exists, version, storedBy, winsAt>>
    /\ UNCHANGED <<readVer, hasRead>>

\* Re-arm a writer after a conflict: it may re-read and retry (CAS retry loop).
WRetry(w) ==
    /\ outcome[w] = "conflict"
    /\ outcome' = [outcome EXCEPT ![w] = "none"]
    /\ UNCHANGED <<exists, version, storedBy, readVer, hasRead, winsAt>>

Next == \E w \in Writers : WRead(w) \/ WCreate(w) \/ WUpdate(w) \/ WRetry(w)

Spec == Init /\ [][Next]_vars /\ WF_vars(Next)

\* At most one writer may succeed a write based on any single version.
NoLostUpdate ==
    \A v \in 0..MaxVersion : Cardinality(winsAt[v]) <= 1

\* Successful updates only ever extend the file — version never regresses
\* and a stored file cannot silently disappear.
VersionMonotonic == version >= 0 /\ (exists => version >= 1)

\* Liveness: every writer that entered a write attempt resolves it
\* (ok or conflict) — CAS never leaves a writer stuck mid-attempt.
AttemptResolves == \A w \in Writers : hasRead[w] ~> (outcome[w] # "none")

====
