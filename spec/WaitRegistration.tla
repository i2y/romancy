------------------------- MODULE WaitRegistration -------------------------
\* Model of romancy's channel-receive suspend protocol: the "register wait +
\* release lock" step in Receive must not miss a message published
\* concurrently (a missed wakeup leaves the instance waiting forever on a
\* message that already arrived).
\*
\* One receiver R suspends on a channel; one publisher P commits a message
\* and then scans waiting subscribers to deliver eagerly. Both designs live
\* in one model, selected by constants:
\*
\*   AtomicReg = FALSE   the pre-fix design:
\*       Receive checked pending messages, then a separate step registered
\*       the wait (and the storage method was itself three autocommit
\*       statements). A publish committed between the check and the
\*       registration sees no waiter and no pending check ever runs again:
\*       the message sits in channel_messages while R waits forever.
\*
\*   AtomicReg = TRUE    the current design:
\*       RegisterChannelReceiveAndReleaseLock is a single transaction that
\*       re-checks pending inside it. A message visible at registration
\*       aborts the wait (R stays running and drains it on resumption).
\*
\*   ScanCanMiss = TRUE  models the residual the transaction cannot cover:
\*       under read-committed isolation a publisher's waiter scan may run
\*       before R's registration commits, so eager delivery can miss even an
\*       atomic registration.
\*
\*   Sweep = TRUE        models RequeueChannelWaitsWithPending, the periodic
\*       backstop that flips waiting instances with pending messages back to
\*       running. It turns any residual miss into bounded-latency healing.
\*
\* Check with TLC (see spec/*.cfg):
\*   java -jar tla2tools.jar -deadlock -config WaitRegistration_old.cfg WaitRegistration.tla
\*   -> verified: invariant violation, counterexample where the publish lands
\*      in the check/register gap and R is stranded waiting on a committed
\*      message (missed wakeup in the pre-fix design)
\*   java -jar tla2tools.jar -deadlock -config WaitRegistration_new.cfg WaitRegistration.tla
\*   -> verified: no errors; the atomic recheck closes the gap when the
\*      publisher's scan is reliable
\*   java -jar tla2tools.jar -deadlock -config WaitRegistration_residual.cfg WaitRegistration.tla
\*   -> verified: invariant violation, the honest residual -- under
\*      read-committed isolation a waiter scan can still miss an uncommitted
\*      wait, so atomic registration alone is not sufficient
\*   java -jar tla2tools.jar -deadlock -config WaitRegistration_sweep.cfg WaitRegistration.tla
\*   -> verified: with the periodic requeue sweep the message is still
\*      delivered (liveness under a fair sweep)
\* The same state machine is model-checked from Go in wait_check_test.go
\* so `go test ./spec` verifies the design without a Java toolchain.

EXTENDS Naturals

CONSTANTS AtomicReg,    \* BOOLEAN: pending recheck inside the wait registration?
          ScanCanMiss,  \* BOOLEAN: may the publisher's waiter scan miss a wait?
          Sweep         \* BOOLEAN: periodic requeue of waiting+pending?

VARIABLES pending,      \* a committed channel_message counts as undelivered for R
          waiting,      \* R's wait registration is visible to publishers
          received,     \* R got the message (0/1)
          rpc,          \* R's pc: "checking" -> "suspending" -> "done"
          ppc           \* P's pc: "publishing" -> "scanned" -> "done"

vars == <<pending, waiting, received, rpc, ppc>>

Init ==
    /\ pending  = 0
    /\ waiting  = 0
    /\ received = 0
    /\ rpc      = "checking"
    /\ ppc      = "publishing"

\* ---- receiver --------------------------------------------------------

\* Old design step 1: the pending check in Receive. No message -> proceed to
\* the separate suspend step; a message -> consume it and finish.
R_CheckPending ==
    /\ ~AtomicReg
    /\ rpc = "checking"
    /\ IF pending = 1
       THEN /\ pending'  = 0
            /\ received' = 1
            /\ rpc'      = "done"
       ELSE /\ rpc'      = "suspending"
            /\ UNCHANGED <<pending, received>>
    /\ UNCHANGED <<waiting, ppc>>

\* Old design step 2: register the wait and release the lock -- far too late
\* to see a message published since the check.
R_RegisterWait ==
    /\ ~AtomicReg
    /\ rpc = "suspending"
    /\ waiting' = 1
    /\ rpc'     = "done"
    /\ UNCHANGED <<pending, received, ppc>>

\* New design: the registration transaction sees a message committed any time
\* before its own commit and refuses to wait on it.
R_RegisterAtomic ==
    /\ AtomicReg
    /\ rpc = "checking"
    /\ IF pending = 1
       THEN /\ pending'  = 0
            /\ received' = 1
            /\ UNCHANGED <<waiting>>
       ELSE /\ waiting'  = 1
            /\ UNCHANGED <<pending, received>>
    /\ rpc' = "done"
    /\ UNCHANGED <<ppc>>

\* ---- publisher --------------------------------------------------------

\* Commit the message row.
P_Insert ==
    /\ ppc = "publishing"
    /\ pending' = 1
    /\ ppc'     = "scanned"
    /\ UNCHANGED <<waiting, received, rpc>>

\* Scan waiters and deliver to R if its wait is visible.
P_Deliver ==
    /\ ppc = "scanned"
    /\ waiting = 1
    /\ pending'  = 0
    /\ waiting'  = 0
    /\ received' = 1
    /\ ppc'      = "done"
    /\ UNCHANGED <<rpc>>

\* The scan finds no waiter -- either R genuinely is not waiting, or
\* (ScanCanMiss) its registration was still uncommitted when the scan ran.
P_ScanMiss ==
    /\ ppc = "scanned"
    /\ waiting = 0 \/ ScanCanMiss
    /\ ppc' = "done"
    /\ UNCHANGED <<pending, waiting, received, rpc>>

\* ---- environment ------------------------------------------------------

\* The periodic backstop: a waiting instance with a pending message is
\* flipped to running and drains it on resumption.
S_Requeue ==
    /\ Sweep
    /\ waiting = 1
    /\ pending = 1
    /\ waiting'  = 0
    /\ pending'  = 0
    /\ received' = 1
    /\ UNCHANGED <<rpc, ppc>>

Next ==
    \/ R_CheckPending \/ R_RegisterWait \/ R_RegisterAtomic
    \/ P_Insert \/ P_Deliver \/ P_ScanMiss
    \/ S_Requeue

Spec == Init /\ [][Next]_vars

\* ---- properties ------------------------------------------------------

\* SAFETY: no reachable state may be a terminal missed wakeup -- R done while
\* registered-waiting on a committed message, P done, and no sweep to heal.
NoMissedWakeup ==
    ~(waiting = 1 /\ pending = 1 /\ rpc = "done" /\ ppc = "done" /\ ~Sweep)

\* LIVENESS: a committed message is eventually received. WF on the whole
\* next relation rules out permanent stuttering while any action is
\* enabled; under ScanCanMiss delivery of a stranded wait relies on the
\* sweep being fair (covered by the same condition).
FairSpec == Spec /\ WF_vars(Next)
PendingLeadsToReceived == (pending = 1) ~> (received = 1)

==========================================================================
