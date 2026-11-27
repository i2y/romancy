---
title: "OpenTelemetry Integration"
weight: 2
---

Romancy ships [OpenTelemetry](https://opentelemetry.io/) tracing hooks in the `github.com/i2y/romancy/hooks/otel` package. They turn the lifecycle events of workflows, activities, event waits, timers and replays into spans.

## Overview

`NewOTelHooks` returns an implementation of `hooks.WorkflowHooks` that records:

- **Workflow spans** for each run of a workflow, with **activity spans** as their children
- **Spans for waits and timers**: event waits, received events, timers, replays and activity cache hits
- **Errors and retries**: a failed workflow or activity sets its span status to error, and every retry adds a span event to the activity span

The hooks record traces only. To export metrics, implement the hook methods you need yourself (see [Lifecycle Hooks](/docs/core-features/hooks)).

## Installation

The hooks are part of the Romancy module. Add the OpenTelemetry SDK and an exporter:

```bash
go get github.com/i2y/romancy
go get go.opentelemetry.io/otel/sdk
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/hooks/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type ReservationResult struct {
	Reserved bool `json:"reserved"`
}

type OrderResult struct {
	Status string `json:"status"`
}

var reserveInventory = romancy.DefineActivity("reserve_inventory",
	func(ctx context.Context, orderID string) (ReservationResult, error) {
		return ReservationResult{Reserved: true}, nil
	},
)

var orderWorkflow = romancy.DefineWorkflow("order_workflow",
	func(ctx *romancy.WorkflowContext, orderID string) (OrderResult, error) {
		if _, err := reserveInventory.Execute(ctx, orderID); err != nil {
			return OrderResult{}, err
		}
		return OrderResult{Status: "completed"}, nil
	},
)

func main() {
	ctx := context.Background()

	// Export spans with OTLP over gRPC (localhost:4317 by default):
	// an OpenTelemetry Collector, Jaeger, Tempo, ...
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	defer func() { _ = provider.Shutdown(ctx) }()

	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithHooks(otelromancy.NewOTelHooks(provider)),
	)
	romancy.RegisterWorkflow(app, orderWorkflow)

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	instanceID, err := romancy.StartWorkflow(ctx, app, orderWorkflow, "ORD-123")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Workflow started: %s\n", instanceID)
}
```

`NewOTelHooks(nil)` uses the global tracer provider (`otel.SetTracerProvider`) instead of an explicit one. The tracer is named `romancy`.

## Spans

```
workflow/order_workflow
├── activity/reserve_inventory
├── activity/process_payment
└── activity/ship_order
```

| Span | Started by | Attributes |
|------|------------|------------|
| `workflow/<workflow name>` | a run of the workflow | `romancy.instance_id`, `romancy.workflow_name`, `romancy.start_time`; at the end `romancy.duration_ms`, `romancy.cancel_reason` when cancelled, and `romancy.suspend_reason` when the run ends without finishing the workflow |
| `activity/<activity name>` | an activity call (a child of the workflow span) | `romancy.instance_id`, `romancy.workflow_name`, `romancy.activity_id`, `romancy.activity_name`, `romancy.is_replay`; at the end `romancy.duration_ms`, and `romancy.attempt` when it failed |
| `activity_cache_hit/<activity name>` | an activity call answered from history during replay (the activity span, renamed) | those of the activity span |
| `event_wait/<event type>` | `WaitEvent` / `Receive` | `romancy.instance_id`, `romancy.workflow_name`, `romancy.event_type`, `romancy.timeout_ms` (with a timeout); `romancy.duration_ms` when the wait ends in the same run, `romancy.suspend_reason` when the run suspends to wait |
| `event_timeout/<event type>` | an event wait's timeout, in the process that handles it (when the wait's own span has ended) | `romancy.instance_id`, `romancy.workflow_name`, `romancy.event_type`; error status |
| `event_received/<event type>` | a delivered event | `romancy.event_type`, `romancy.event_id`, `romancy.event_source`, `romancy.instance_id` |
| `timer/<timer id>` | `Sleep` / `SleepUntil` | `romancy.instance_id`, `romancy.workflow_name`, `romancy.timer_id`, `romancy.expires_at`; it ends with the run that waits for the timer (`romancy.suspend_reason`) |
| `timer_fired/<timer id>` | a timer firing, in the process that fires it | `romancy.instance_id`, `romancy.workflow_name`, `romancy.timer_id`, `romancy.fired_at` |
| `replay/<workflow name>` | a run that replays history | `romancy.instance_id`, `romancy.workflow_name`, `romancy.history_events`; when the run completes `romancy.cache_hits`, `romancy.new_activities`, `romancy.duration_ms` |

A retry adds an `activity_retry` event to the activity span, with `romancy.attempt`, `romancy.max_attempts`, `romancy.next_delay_ms` and `romancy.error`. A failed workflow, a failed activity, a cancelled workflow and an event timeout set the span status to error.

A workflow span covers one run. A run that waits, recurs or is aborted ends every span the process holds for the instance, with `romancy.suspend_reason` set to `waiting`, `recur` or `aborted` (the hooks implement `OnWorkflowSuspended`, see [Lifecycle Hooks](/docs/core-features/hooks#runs-that-suspend)). The next run, possibly on another worker, starts new spans, so a wait shows as the gap between two runs; `romancy.instance_id` ties the runs of an instance together. These spans keep an unset status: an aborted run, for example on shutdown, records its error as an event.

## Trace Context

The hooks keep the open spans of a run in memory, so an activity span becomes a child of its workflow span within one run. Workflow runs execute in the background, often after a wait and on another worker, so each run starts its own trace: a span in the context passed to `StartWorkflow` (for example one created by HTTP middleware) is not the parent of the workflow's spans.

The hooks do not put their spans into the context your code receives, and Romancy does not read `traceparent` from incoming CloudEvents. To continue a trace across services, start a span of your own in an activity and put its trace context into the event you send:

```go
package main

import (
	"context"

	"github.com/i2y/romancy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

var notifyShipped = romancy.DefineActivity("notify_shipped",
	func(ctx context.Context, orderID string) (struct{}, error) {
		ctx, span := otel.Tracer("orders").Start(ctx, "notify_shipped")
		defer span.End()

		// Inject the span's trace context into the event payload so
		// consumers can continue the trace.
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)
		eventData := map[string]any{"order_id": orderID}
		for k, v := range carrier {
			eventData["trace_"+k] = v
		}

		err := romancy.SendEventTransactional(romancy.GetWorkflowContext(ctx),
			"order.shipped",
			"orders",
			eventData,
		)
		return struct{}{}, err
	},
)
```

The receiving service extracts it with the same propagator (a `propagation.MapCarrier` over the received fields). With the default `otel.GetTextMapPropagator()`, set a propagator first, for example `otel.SetTextMapPropagator(propagation.TraceContext{})`.

## Using an Existing TracerProvider

If your application already configures a TracerProvider, pass it to the hooks, or register it globally and pass `nil`:

```go
package main

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/hooks/otel"
)

func main() {
	// Configure your own provider
	provider := trace.NewTracerProvider(
		trace.WithResource(myResource),
	)
	otel.SetTracerProvider(provider)

	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		// otelromancy.NewOTelHooks(nil) would use the same, global provider
		romancy.WithHooks(otelromancy.NewOTelHooks(provider)),
	)
	_ = app
}
```

## Combining with Other Hooks

`WithHooks` takes one `hooks.WorkflowHooks`. To run your own hooks next to the tracing hooks, embed `*otel.OTelHooks` in your type and override the methods you need, calling the embedded method first:

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy/hooks"
	otelromancy "github.com/i2y/romancy/hooks/otel"
)

type TracingAndLogging struct {
	*otelromancy.OTelHooks
}

func (h TracingAndLogging) OnWorkflowFailed(ctx context.Context, info hooks.WorkflowFailedInfo) {
	h.OTelHooks.OnWorkflowFailed(ctx, info)
	log.Printf("workflow %s (%s) failed: %v", info.WorkflowName, info.InstanceID, info.Error)
}
```

Pass `TracingAndLogging{otelromancy.NewOTelHooks(provider)}` to `romancy.WithHooks`.

## Related Documentation

- [Lifecycle Hooks](/docs/core-features/hooks) - Detailed hooks documentation
- [OpenTelemetry Documentation](https://opentelemetry.io/docs/)
- [OpenTelemetry Go SDK](https://opentelemetry.io/docs/languages/go/)
