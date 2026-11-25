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

## Practical notes

- **At-least-once execution**: task handlers and supervisor children must
  be idempotent. Fencing protects the database rows; it cannot prevent a
  duplicate side effect when a slow-but-alive worker is reclaimed.
  See `spec/TaskReclaim.tla` and `spec/SupervisorRestart.tla` in the
  [schema repo](https://github.com/durax-io/schema).
- The formal spec suite (`schema/spec/`) is the normative reference for
  anyone implementing another language binding.
