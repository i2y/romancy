---
title: "Actor OS Vision"
weight: 2
---

# Actor OS Vision

Romancy is the Go implementation of a shared **actor OS** kernel: the same
database-backed kernel is implemented in Go (romancy), Python (edda), and
Ruby (shikibu), so actors written in different languages exchange messages,
tasks, and supervision on one kernel.

![ActorOS architecture](/images/actoros-architecture.svg)

The kernel is not a server — it is a **shared database schema plus a
semantics constitution** ([SEMANTICS.md](https://github.com/durax-io/schema/blob/main/SEMANTICS.md)),
with its concurrency protocols machine-checked in TLA+.

## Unix analogies

| Unix | Actor OS | Backed by |
|---|---|---|
| process | workflow instance | one row, replayed after crashes |
| init / systemd | supervision tree | one_for_one / one_for_all / rest_for_one |
| pipe / mailbox | channel | broadcast / competing / direct, cross-language |
| filesystem | actor FS | `/actor/{id}/` private, `/shared/` shared |
| file descriptor | capability token | sealed channels, FS writes |
| scheduler | lock claim + epoch fencing | TLA+-verified |

## Kernel primitives

All primitives take the `*romancy.WorkflowContext`.

| Primitive | API | Behavior |
|---|---|---|
| Names | `RegisterName`, `UnregisterName`, `Whereis` | A name binds to one instance. Registering a name the instance already holds succeeds; a name held by another instance fails with `ErrActorNameTaken`. Only the holder can unregister a name (`ErrNotOwner` otherwise; an unbound name is a no-op). Names are freed once the holder has completed, failed or been cancelled. |
| Monitors | `Monitor`, `Unmonitor` | When the target completes, fails or is cancelled, exactly one DOWN message (`{"type":"DOWN","actor":…,"status":…}`) is published to the monitoring instance's direct channel; receive it with a `ModeDirect` subscription. A recurred instance is alive and produces no DOWN. |
| Capabilities | `IssueCapability`, `CheckCapability`, `RevokeCapability` | Bearer tokens `cap:{kind}:{target}:{secret}`. The capability table stores only a hash of the secret. Without a capability key the issued token is also recorded in the issuing workflow's history; with one (`WithCapabilityKey`, or `DURAX_CAPABILITY_KEY`) the secret is derived from the capability ID and history holds no token. `WithCapabilityTTL` sets an expiry. Only the issuing instance can revoke a capability (`ErrNotOwner` otherwise; an unknown ID is a no-op). Kinds: `channel_pub`, `channel_sub`, `fs_write`, `actor`. A sandboxed workflow cannot issue capabilities. |
| Sealed channels | `WithPublishCapability`, `WithReceiveCapability` | A channel with a grant requires the token, including its direct channels and CloudEvents posted to it; an expired grant still seals the channel. See [Sealed Channels](/docs/core-features/channels#sealed-channels). |
| Tasks | `PostTask`, `ClaimTask`, `CompleteTask`, `FailTask`, `ListTasks`, `WaitTask`, `WaitAnyTask` | A durable work queue. Only the claimant may complete or fail a task (`ErrTaskConflict`); a claim older than the stale-lock timeout (300 s) is reclaimed, so execution is at-least-once. `WaitTask` and `WaitAnyTask` suspend the workflow until a task settles or the timeout expires. |
| Actor FS | `FSRead`, `FSWrite`, `FSList`, `FSDelete` | Activities only (`ErrFSOutsideActivity`). `/actor/{id}/` belongs to its owner; `/shared/` is readable by all, and writing to it needs an `fs_write` capability. Writes are compare-and-set on the version (`ErrFSConflict`). |
| Blobs | `StorePayload`, `LoadPayload`, `BlobStat` | Content-addressed (`blob:sha256:…`), so readable by anyone who knows the hash: encrypt secrets before storing them. |
| Event log | `ReadChannelSince`, `ReadChannelAfter` | Read a channel's messages after a cursor: a message ID (`ErrCursorNotFound` once that message is gone) or the numeric ID of the last message read (keeps working after it is deleted). |
| Supervision | `DefineSupervisor` | `OneForOne` / `OneForAll` / `RestForOne`; restart modes `RestartPermanent` (always), `RestartTransient` (only after failure or cancellation) and `RestartTemporary` (never); restart intensity limits. |
| Sandboxing | `WithSandboxed(true)` | See the README's *Sandboxed Actors* section. |

DOWN messages, name release, task reclaim and the removal of expired capabilities run on the leader's cleanup tick (`WithStaleLockInterval`, 60 s by default).

## Rules every primitive follows

- **Replay-safe.** Called from workflow code, a primitive is a recorded step: it runs once, and a replay returns the recorded outcome, including a recorded error such as `ErrActorNameTaken` or `ErrChannelForbidden`, so the workflow takes the same branch. Inside an activity, primitives run directly and the activity is what is recorded.
- **Fenced.** A primitive with a side effect first proves that the run still holds the instance lock, then applies the effect and records it in the same transaction. A run that lost its lease to another worker changes nothing and gets `ErrLockNotHeld`.
- **Only kernel errors are recorded.** A step records its value or an error with a kernel code (`ErrTaskConflict`, `ErrActorNameTaken`, `ErrFSConflict`, `ErrChannelForbidden`, `ErrNotOwner`, `ErrCursorNotFound`, `ErrInvalidArgument`, …). Any other error, such as a database outage, aborts the run instead: that step and every later step and activity of the run return `ErrRunAborted`, nothing is completed, failed or compensated, and the instance stays `running` until a later run replays it and tries the step again. Lock loss and shutdown end a run the same way.
- **Terminal is final.** A completed, failed or cancelled instance never runs again: late timers, waits and deliveries cannot revive it.
- **External state only in activities.** The FS can change between a run and its replay, so FS calls belong in activities.

## Practical notes

- **At-least-once execution**: task handlers and supervisor children must
  be idempotent. Fencing protects the database rows; it cannot prevent a
  duplicate side effect when a slow-but-alive worker is reclaimed.
  See `spec/TaskReclaim.tla` and `spec/SupervisorRestart.tla` in the
  [schema repo](https://github.com/durax-io/schema).
- **Restarts follow the child's actual status**: when a strategy restarts a
  live child, the supervisor cancels it conditionally. A child that already
  finished on its own is judged by its real status, so a transient child
  that completed is not restarted, while a permanent child always is.
- **Recur keeps the actor's identity**: a recur continues the same
  instance, so names, monitors, issued capabilities, tasks and
  `/actor/{id}/` files keep pointing at it, and no DOWN is sent. Edda and
  Shikibu do the same since the 2026-11-27 releases.
- The formal spec suite (`schema/spec/`) is the normative reference for
  anyone implementing another language binding.
