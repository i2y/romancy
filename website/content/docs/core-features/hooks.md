---
title: "Lifecycle Hooks"
weight: 5
---

Romancy provides an interface-based hook system that allows you to integrate custom observability and monitoring tools without coupling the framework to specific vendors.

## Overview

The hook system enables you to:

- ✅ Add distributed tracing (OpenTelemetry, Jaeger, etc.)
- ✅ Send metrics to monitoring systems (Datadog, Prometheus)
- ✅ Track errors (Sentry, custom logging)
- ✅ Audit workflow execution
- ✅ Combine multiple observability backends

**Design Philosophy:** Romancy focuses on workflow orchestration, while observability is delegated to users through a flexible hook system.

## Quick Start

### 1. Implement WorkflowHooks Interface

```go
package main

import (
	"context"
	"log"

	"github.com/i2y/romancy/hooks"
)

type LoggingHooks struct {
	hooks.NoOpHooks
}

func (h *LoggingHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	log.Printf("workflow.start instance_id=%s workflow_name=%s", info.InstanceID, info.WorkflowName)
}

func (h *LoggingHooks) OnActivityComplete(ctx context.Context, info hooks.ActivityCompleteInfo) {
	log.Printf("activity.complete instance_id=%s activity_id=%s activity_name=%s duration=%s",
		info.InstanceID, info.ActivityID, info.ActivityName, info.Duration)
}

// Implement other methods as needed...
```

### 2. Pass Hooks to App

```go
app := romancy.NewApp(
	romancy.WithDatabase("workflow.db"),
	romancy.WithWorkerID("worker-1"),
	romancy.WithHooks(&LoggingHooks{}), // Your hook implementation
)

// Register your workflows (romancy.RegisterWorkflow), then start the app
ctx := context.Background()
if err := app.Start(ctx); err != nil {
	log.Fatal(err)
}
```

### 3. Run Your Workflows

All lifecycle events are automatically captured:

- ✅ Workflow start, complete, failure, cancellation
- ✅ Activity execution, retries and results answered from history during replay
- ✅ Event waits, received events and timers

## Available Hooks

The `hooks.WorkflowHooks` interface defines these methods. Every method receives a `context.Context` and an info struct. Embed `hooks.NoOpHooks` to implement only the methods you need:

| Hook Method | Info Struct | Description |
|-------------|-------------|-------------|
| `OnWorkflowStart` | `hooks.WorkflowStartInfo` | Called when a run of a workflow starts: its first run, and every later run after a wait, a timer, a recur or a crash. `InputData` is the workflow input in the first run, when the process that called `StartWorkflow` runs it; `nil` in later runs, and in a first run that a resumption poll begins (when the starting process does not register the workflow, or started it inside an activity) |
| `OnWorkflowComplete` | `hooks.WorkflowCompleteInfo` | Called when a workflow completes successfully |
| `OnWorkflowFailed` | `hooks.WorkflowFailedInfo` | Called once when a workflow fails; when a compensation also fails, `Error` reports both |
| `OnWorkflowCancelled` | `hooks.WorkflowCancelledInfo` | Called when a workflow is cancelled |
| `OnActivityStart` | `hooks.ActivityStartInfo` | Called for every activity call, including one answered from history; `IsReplay` reports whether the run is replaying history |
| `OnActivityComplete` | `hooks.ActivityCompleteInfo` | Called after an activity body completes successfully (not for results answered from history) |
| `OnActivityFailed` | `hooks.ActivityFailedInfo` | Called when an activity fails with an error |
| `OnActivityRetry` | `hooks.ActivityRetryInfo` | Called before an activity retry attempt |
| `OnActivityCacheHit` | `hooks.ActivityCacheHitInfo` | Called when an activity call is answered from history during replay |
| `OnEventReceived` | `hooks.EventReceivedInfo` | Called when a workflow receives an event |
| `OnEventWaitStart` | `hooks.EventWaitStartInfo` | Called when a workflow starts waiting for an event |
| `OnEventWaitComplete` | `hooks.EventWaitCompleteInfo` | Called when an event wait completes |
| `OnEventTimeout` | `hooks.EventTimeoutInfo` | Called when an event wait times out |
| `OnTimerStart` | `hooks.TimerStartInfo` | Called when a timer is created |
| `OnTimerFired` | `hooks.TimerFiredInfo` | Called when a timer fires |
| `OnReplayStart` | `hooks.ReplayStartInfo` | Called when workflow replay starts |
| `OnReplayComplete` | `hooks.ReplayCompleteInfo` | Called when workflow replay completes |

Workflows run concurrently, so hooks are called from many goroutines at once: guard any state your hooks keep.

## Best Practices

### 1. Scrub Sensitive Data

Always remove sensitive information from logs:

```go
var sensitiveFields = map[string]bool{
	"password":    true,
	"api_key":     true,
	"token":       true,
	"ssn":         true,
	"credit_card": true,
}

func scrubData(data map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range data {
		if sensitiveFields[strings.ToLower(k)] {
			result[k] = "***REDACTED***"
		} else {
			result[k] = v
		}
	}
	return result
}

type SecureHooks struct {
	hooks.NoOpHooks
}

func (h *SecureHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	if info.InputData == nil {
		return // A later run of the instance
	}
	// InputData is the workflow's own input type: view it as JSON fields
	raw, err := json.Marshal(info.InputData)
	if err != nil {
		return
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return // Not a JSON object
	}
	log.Printf("workflow.start input_data=%v", scrubData(data))
}
```

### 2. Handle Hook Errors Gracefully

Don't let hook failures break your workflows:

```go
type RobustHooks struct {
	hooks.NoOpHooks
}

func (h *RobustHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	defer func() {
		if r := recover(); r != nil {
			// Log but don't panic (workflow should continue)
			log.Printf("Hook error: %v", r)
		}
	}()

	// Send metrics (might fail)
	sendMetrics(info.InstanceID, info.WorkflowName)
}
```

### 3. Use Sampling in Production

For high-throughput systems, sample traces:

```go
import "math/rand"

type SampledHooks struct {
	hooks.NoOpHooks
	sampleRate float64
}

func NewSampledHooks(sampleRate float64) *SampledHooks {
	return &SampledHooks{sampleRate: sampleRate}
}

func (h *SampledHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	if rand.Float64() < h.sampleRate {
		log.Printf("workflow.start instance_id=%s", info.InstanceID)
	}
}

func (h *SampledHooks) OnWorkflowFailed(ctx context.Context, info hooks.WorkflowFailedInfo) {
	// Always log errors (100% sampling)
	log.Printf("workflow.failed instance_id=%s error=%v", info.InstanceID, info.Error)
}
```

## Integration Examples

### OpenTelemetry

Romancy ships tracing hooks in `github.com/i2y/romancy/hooks/otel`:

```go
package main

import (
	"github.com/i2y/romancy"
	otelromancy "github.com/i2y/romancy/hooks/otel"
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		// nil: use the global TracerProvider
		romancy.WithHooks(otelromancy.NewOTelHooks(nil)),
	)
	_ = app
}
```

**What you get:**

- A span for each workflow run, with its activities as child spans
- Spans for event waits, timers, replays and activity results answered from history
- Error status on failures, and a span event for each activity retry

See [OpenTelemetry Integration](/docs/integrations/opentelemetry) for the spans and their attributes.

### Prometheus

```go
package main

import (
	"context"

	"github.com/i2y/romancy/hooks"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	workflowRuns = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "romancy_workflow_runs_total",
			Help: "Workflow runs started",
		},
		[]string{"workflow_name"},
	)
	activityDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "romancy_activity_duration_seconds",
			Help:    "Duration of activity executions",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"activity_name"},
	)
	activityCacheHits = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "romancy_activity_cache_hits_total",
			Help: "Activity calls answered from history during replay",
		},
		[]string{"activity_name"},
	)
)

type PrometheusHooks struct {
	hooks.NoOpHooks
}

func (h *PrometheusHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	workflowRuns.WithLabelValues(info.WorkflowName).Inc()
}

func (h *PrometheusHooks) OnActivityComplete(ctx context.Context, info hooks.ActivityCompleteInfo) {
	activityDuration.WithLabelValues(info.ActivityName).Observe(info.Duration.Seconds())
}

func (h *PrometheusHooks) OnActivityCacheHit(ctx context.Context, info hooks.ActivityCacheHitInfo) {
	activityCacheHits.WithLabelValues(info.ActivityName).Inc()
}
```

**What you get:**

- Workflow runs and activity durations with label dimensions
- Cache hits, to see how much work replays skip
- Standard `/metrics` endpoint integration (`promhttp.Handler()`)

### Datadog

```go
package main

import (
	"context"
	"fmt"

	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/i2y/romancy/hooks"
)

type DatadogHooks struct {
	hooks.NoOpHooks
	statsd *statsd.Client
}

func NewDatadogHooks(statsdAddr string) (*DatadogHooks, error) {
	client, err := statsd.New(statsdAddr)
	if err != nil {
		return nil, err
	}
	return &DatadogHooks{statsd: client}, nil
}

func (h *DatadogHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	_ = h.statsd.Incr("romancy.workflow.started",
		[]string{fmt.Sprintf("workflow:%s", info.WorkflowName)}, 1)
}

func (h *DatadogHooks) OnActivityComplete(ctx context.Context, info hooks.ActivityCompleteInfo) {
	_ = h.statsd.Timing("romancy.activity.duration", info.Duration,
		[]string{fmt.Sprintf("activity:%s", info.ActivityName)}, 1)
}

func (h *DatadogHooks) OnActivityFailed(ctx context.Context, info hooks.ActivityFailedInfo) {
	_ = h.statsd.Incr("romancy.activity.failed",
		[]string{fmt.Sprintf("activity:%s", info.ActivityName)}, 1)
}
```

For traces, use the [OpenTelemetry hooks](/docs/integrations/opentelemetry) and send OTLP to the Datadog Agent.

### Sentry Error Tracking

```go
package main

import (
	"context"

	"github.com/getsentry/sentry-go"
	"github.com/i2y/romancy/hooks"
)

type SentryHooks struct {
	hooks.NoOpHooks
}

// Hooks run concurrently: each report uses its own clone of the hub.

func (h *SentryHooks) OnWorkflowFailed(ctx context.Context, info hooks.WorkflowFailedInfo) {
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetContext("workflow", sentry.Context{
			"instance_id":   info.InstanceID,
			"workflow_name": info.WorkflowName,
		})
	})
	hub.CaptureException(info.Error)
}

func (h *SentryHooks) OnActivityFailed(ctx context.Context, info hooks.ActivityFailedInfo) {
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetContext("activity", sentry.Context{
			"instance_id":   info.InstanceID,
			"activity_id":   info.ActivityID,
			"activity_name": info.ActivityName,
			"attempt":       info.Attempt,
		})
	})
	hub.CaptureException(info.Error)
}
```

## Complete Example

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks"
)

// Custom hooks implementation
type MyHooks struct {
	hooks.NoOpHooks
}

func (h *MyHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	log.Printf("🚀 Workflow started: %s (%s)", info.WorkflowName, info.InstanceID)
}

func (h *MyHooks) OnWorkflowComplete(ctx context.Context, info hooks.WorkflowCompleteInfo) {
	log.Printf("✅ Workflow completed: %s (%s)", info.WorkflowName, info.InstanceID)
}

func (h *MyHooks) OnWorkflowFailed(ctx context.Context, info hooks.WorkflowFailedInfo) {
	log.Printf("❌ Workflow failed: %s (%s) - %v", info.WorkflowName, info.InstanceID, info.Error)
}

func (h *MyHooks) OnActivityStart(ctx context.Context, info hooks.ActivityStartInfo) {
	log.Printf("▶️ Activity called: %s", info.ActivityName)
}

func (h *MyHooks) OnActivityCacheHit(ctx context.Context, info hooks.ActivityCacheHitInfo) {
	log.Printf("📦 Activity answered from history: %s", info.ActivityName)
}

func (h *MyHooks) OnActivityComplete(ctx context.Context, info hooks.ActivityCompleteInfo) {
	log.Printf("✅ Activity completed: %s", info.ActivityName)
}

// Result type
type GreetingResult struct {
	Message string `json:"message"`
}

// Activity
var greetUser = romancy.DefineActivity("greet_user",
	func(ctx context.Context, name string) (GreetingResult, error) {
		return GreetingResult{Message: "Hello, " + name}, nil
	},
)

// Workflow
var greetingWorkflow = romancy.DefineWorkflow("greeting_workflow",
	func(ctx *romancy.WorkflowContext, name string) (GreetingResult, error) {
		result, err := greetUser.Execute(ctx, name)
		if err != nil {
			return GreetingResult{}, err
		}
		return result, nil
	},
)

func main() {
	app := romancy.NewApp(
		romancy.WithDatabase("workflow.db"),
		romancy.WithWorkerID("worker-1"),
		romancy.WithHooks(&MyHooks{}),
	)

	ctx := context.Background()

	romancy.RegisterWorkflow(app, greetingWorkflow)

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	// Start workflow - hooks will log all events
	instanceID, err := romancy.StartWorkflow(ctx, app, greetingWorkflow, "World")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Workflow instance: %s", instanceID)

	// The workflow runs in the background: give it time to finish
	time.Sleep(time.Second)
}
```

**Output:**

```
Workflow instance: abc-123-def
🚀 Workflow started: greeting_workflow (abc-123-def)
▶️ Activity called: greet_user
✅ Activity completed: greet_user
✅ Workflow completed: greeting_workflow (abc-123-def)
```

## See Also

- **[Workflows and Activities](/docs/core-features/workflows-activities)**: Learn about basic workflow concepts
- **[Durable Execution](/docs/core-features/durable-execution/replay)**: Understand replay and recovery
- **[Examples](/docs/examples/simple)**: See workflows in action
