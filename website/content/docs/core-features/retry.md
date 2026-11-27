---
title: "Automatic Retry"
weight: 7
---

Romancy retries a failed activity with exponential backoff before the failure reaches the workflow. The retry policy is set per activity; transient failures such as a network timeout need no retry logic in the activity itself.

## How Retry Works

```
Run the activity body
    ↓
Success? ── yes ──→ record the result and commit ✅
    ↓ no
Terminal error, or policy exhausted? ── yes ──→ return the error ❌
    ↓ no
Wait for the backoff delay, then run the body again
```

### Transaction Boundaries

The retry loop is **outside** the transaction. Each attempt of a transactional activity (the default) runs in its own transaction:

```go
// Simplified
for attempt := 1; ; attempt++ {
    tx := begin()
    result, err := body(ctx, input) // writes through ctx.Session() go to tx
    if err == nil {
        recordResult(tx, result) // in the same transaction
        commit(tx)
        return result, nil
    }
    rollback(tx) // nothing from this attempt survives
    if isTerminal(err) || !policy.ShouldRetry(attempt, err) {
        return zero, err
    }
    sleep(policy.GetDelay(attempt))
}
```

- A failed attempt leaves no partial state: its writes, outbox events and post-commit callbacks are rolled back with it.
- Each retry starts in a new transaction. On PostgreSQL, a failed statement aborts the transaction it ran in, so a retry could not succeed inside the same one.
- The backoff wait does not hold a transaction open.
- On SQLite, an attempt that reads before its first write fails at once with a write conflict when another connection writes in between. Romancy rolls the attempt back and runs it again in a new transaction; this restart does not count as an attempt.

Activities with `WithTransactional(false)` run the same loop without a transaction.

## Default Retry Policy

An activity without `WithRetryPolicy` uses `retry.DefaultPolicy()`:

| Field | Value |
|---|---|
| `MaxAttempts` | 3, including the first attempt |
| `InitialInterval` | 100 ms |
| `Multiplier` | 2.0 |
| `MaxInterval` | 10 s |
| `RandomizationFactor` | 0.5 (each delay varies by ±50%) |

A body that keeps failing runs three times, about 100 ms and then 200 ms apart, and the activity returns `*romancy.RetryExhaustedError`.

## Custom Retry Policies

Pass a policy with `WithRetryPolicy`:

```go
var chargeCard = romancy.DefineActivity("charge_card",
    func(ctx context.Context, req ChargeRequest) (Receipt, error) {
        return paymentClient.Charge(ctx, req)
    },
    romancy.WithRetryPolicy[ChargeRequest, Receipt](retry.Exponential(
        5,                    // max attempts
        500*time.Millisecond, // initial interval
        30*time.Second,       // max interval
        2.0,                  // multiplier
    )),
    // An external API call: no database transaction needed.
    romancy.WithTransactional[ChargeRequest, Receipt](false),
)
```

The `retry` package provides these constructors:

| Constructor | Behavior |
|---|---|
| `retry.DefaultPolicy()` | 3 attempts, 100 ms doubling up to 10 s, ±50% jitter |
| `retry.NoRetry()` | One attempt |
| `retry.Fixed(n, d)` | n attempts, a fixed delay d, no jitter |
| `retry.Exponential(n, initial, max, multiplier)` | n attempts, exponential backoff, ±50% jitter |

The builder sets each field:

```go
policy := retry.NewBuilder().
    WithMaxAttempts(10).
    WithInitialInterval(time.Second).
    WithMaxInterval(time.Minute).
    WithMultiplier(2).
    WithJitter(0.2).
    WithNonRetryableErrors(ErrInvalidCard).
    Build()

// Pass it to activities with romancy.WithRetryPolicy[In, Out](policy)
```

`retry.NewBuilder()` starts from 3 attempts, 100 ms, ×2 and 10 s, without jitter.

### Policy Fields

| Field | Meaning |
|---|---|
| `MaxAttempts` | Attempts including the first; 0 means no limit |
| `InitialInterval` | Delay before the first retry |
| `Multiplier` | Factor applied to the delay for each further retry; 0 means 2.0 |
| `MaxInterval` | Upper bound on the delay, applied before jitter; 0 means no bound |
| `RandomizationFactor` | Jitter: 0.5 puts each delay within [0.5×, 1.5×] |
| `NonRetryableErrors` | Errors, matched with `errors.Is`, that end the retries |

## Non-Retryable Errors

Return a `TerminalError` for a failure that no retry can fix:

```go
var validateOrder = romancy.DefineActivity("validate_order",
    func(ctx context.Context, order Order) (Order, error) {
        if order.Amount <= 0 {
            return Order{}, romancy.NewTerminalErrorf("invalid amount: %f", order.Amount)
        }
        return order, nil
    },
)
```

The activity returns a `TerminalError` (or an error that wraps one) at once, as it is; `romancy.IsTerminalError(err)` reports it. An error listed in the policy's `NonRetryableErrors` also ends the retries, but comes back wrapped in a `RetryExhaustedError`.

## RetryExhaustedError

When the policy gives up, the activity returns a `*romancy.RetryExhaustedError`. It wraps the last attempt's error, so `errors.Is` and `errors.As` see through it:

```go
_, err := chargeCard.Execute(ctx, req)
if err != nil {
    var exhausted *romancy.RetryExhaustedError
    if errors.As(err, &exhausted) {
        slog.Warn("charge failed", "activity", exhausted.ActivityName,
            "attempts", exhausted.Attempts, "error", exhausted.LastErr)
    }
    return Result{}, err // the workflow fails and its compensations run
}
```

### Failures are recorded

When an activity gives up (a `TerminalError`, or the policy is exhausted), its failure is recorded in history, for transactional activities too: the failed attempt is rolled back first, then the failure is written. A replay returns the recorded failure and does not run the activity again, so a workflow that handles the failure and carries on takes the same branch after it suspends.

The recorded failure keeps its kind, not its Go type:

- a `TerminalError` stays terminal (`romancy.IsTerminalError`);
- an exhausted policy comes back as a `*romancy.RetryExhaustedError` with its `Attempts`, wrapping a plain error with the last attempt's message;
- kernel errors such as `romancy.ErrChannelForbidden` still match `errors.Is`.

Your own error types come back as plain errors with their message, so after a replay `errors.As` no longer finds them. Branch on the kinds above, or on whether the error is nil.

A run that loses its lock or is shut down mid-attempt records nothing: another run executes the activity again (see [Retry vs. Crash Recovery](#retry-vs-crash-recovery)).

## Timeouts

`WithTimeout(d)` bounds each attempt: the context passed to the body is cancelled after `d`. A body that then returns an error counts as a failed attempt, which the policy may retry.

```go
var fetchQuote = romancy.DefineActivity("fetch_quote",
    func(ctx context.Context, symbol string) (Quote, error) {
        return quoteClient.Get(ctx, symbol) // ctx expires after 10 s
    },
    romancy.WithTimeout[string, Quote](10*time.Second),
    romancy.WithRetryPolicy[string, Quote](retry.Fixed(3, time.Second)),
    romancy.WithTransactional[string, Quote](false),
)
```

## Observing Retries

[Lifecycle hooks](/docs/core-features/hooks) report each step of the loop:

| Hook | When |
|---|---|
| `OnActivityRetry` | Before each backoff wait, with `Attempt`, `MaxAttempts`, `NextDelay` and the attempt's `Error` |
| `OnActivityFailed` | When the activity gives up (terminal error or exhausted policy), with the final `Attempt` |
| `OnActivityComplete` | After the successful attempt (for a transactional activity, once it has committed) |

The [OpenTelemetry integration](/docs/integrations/opentelemetry) adds an `activity_retry` event to the activity's span for each retry and records the final error on the span.

## Retry vs. Crash Recovery

| | Retry | Crash recovery |
|---|---|---|
| **Trigger** | The activity body returns an error | The worker dies or loses its lease |
| **Scope** | One activity | The whole workflow run |
| **When** | After the backoff delay | After the lease (300 s) expires and another worker claims the instance |
| **Mechanism** | The retry loop | Deterministic replay from history |
| **Transactions** | Each attempt in a new transaction | The interrupted attempt never committed; it runs again, from its first attempt |

```go
var orderWorkflow = romancy.DefineWorkflow("order",
    func(ctx *romancy.WorkflowContext, orderID string) (string, error) {
        // Recorded before the crash: the replay returns the stored result.
        if _, err := reserveInventory.Execute(ctx, orderID); err != nil {
            return "", err
        }
        // A network timeout here is retried by the retry loop. If the worker
        // crashes during an attempt, the attempt's transaction never commits,
        // and the worker that recovers the instance runs the activity again.
        if _, err := processPayment.Execute(ctx, orderID); err != nil {
            return "", err
        }
        return "shipped", nil
    },
)
```

Activities therefore run at least once. Keep external side effects idempotent, for example with an idempotency key derived from the instance ID and activity ID.

## Examples

See the retry example in the examples directory on GitHub:

- **[examples/retry](https://github.com/i2y/romancy/tree/main/examples/retry)**: Retry policy demonstrations

## Next Steps

- **[Workflows and Activities](/docs/core-features/workflows-activities)**: Learn more about activity behavior
- **[Saga Pattern](/docs/core-features/saga-compensation)**: Automatic compensation on failure
- **[Durable Execution](/docs/core-features/durable-execution/replay)**: How Romancy ensures workflows never lose progress
