package otel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/i2y/romancy/hooks"
)

// setupTest creates a test tracer provider and returns the hooks and span recorder.
func setupTest() (*OTelHooks, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	h := NewOTelHooks(tp)
	return h, sr
}

func TestNewOTelHooks(t *testing.T) {
	// Test with nil tracer provider (uses global)
	h := NewOTelHooks(nil)
	if h == nil {
		t.Fatal("expected non-nil hooks")
	}
	if h.tracer == nil {
		t.Fatal("expected non-nil tracer")
	}

	// Test with custom tracer provider
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	h = NewOTelHooks(tp)
	if h == nil {
		t.Fatal("expected non-nil hooks")
	}
}

func TestWorkflowLifecycle(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	// Start workflow
	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		InputData:    map[string]string{"order_id": "O-001"},
		StartTime:    time.Now(),
	})

	// Complete workflow
	h.OnWorkflowComplete(ctx, hooks.WorkflowCompleteInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		OutputData:   map[string]string{"status": "completed"},
		Duration:     100 * time.Millisecond,
	})

	// Check spans
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "workflow/order_workflow" {
		t.Errorf("expected span name 'workflow/order_workflow', got %s", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("expected status OK, got %v", span.Status().Code)
	}

	// Check attributes
	attrs := span.Attributes()
	checkAttribute(t, attrs, "romancy.instance_id", "wf-123")
	checkAttribute(t, attrs, "romancy.workflow_name", "order_workflow")
}

func TestWorkflowFailed(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   "wf-456",
		WorkflowName: "payment_workflow",
		StartTime:    time.Now(),
	})

	h.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{
		InstanceID:   "wf-456",
		WorkflowName: "payment_workflow",
		Error:        errors.New("payment failed"),
		Duration:     50 * time.Millisecond,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("expected status Error, got %v", span.Status().Code)
	}
}

func TestWorkflowCancelled(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{
		InstanceID:   "wf-789",
		WorkflowName: "booking_workflow",
		StartTime:    time.Now(),
	})

	h.OnWorkflowCancelled(ctx, hooks.WorkflowCancelledInfo{
		InstanceID:   "wf-789",
		WorkflowName: "booking_workflow",
		Reason:       "user request",
		Duration:     30 * time.Millisecond,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("expected status Error (canceled), got %v", span.Status().Code)
	}

	attrs := span.Attributes()
	checkAttribute(t, attrs, "romancy.cancel_reason", "user request")
}

func TestActivityLifecycle(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	// Start activity
	h.OnActivityStart(ctx, hooks.ActivityStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "reserve_inventory:1",
		ActivityName: "reserve_inventory",
		IsReplay:     false,
	})

	// Complete activity
	h.OnActivityComplete(ctx, hooks.ActivityCompleteInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "reserve_inventory:1",
		ActivityName: "reserve_inventory",
		OutputData:   map[string]bool{"reserved": true},
		Duration:     20 * time.Millisecond,
		IsReplay:     false,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "activity/reserve_inventory" {
		t.Errorf("expected span name 'activity/reserve_inventory', got %s", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("expected status OK, got %v", span.Status().Code)
	}

	attrs := span.Attributes()
	checkAttribute(t, attrs, "romancy.activity_id", "reserve_inventory:1")
}

func TestActivityFailed(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnActivityStart(ctx, hooks.ActivityStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "process_payment:1",
		ActivityName: "process_payment",
		IsReplay:     false,
	})

	h.OnActivityFailed(ctx, hooks.ActivityFailedInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "process_payment:1",
		ActivityName: "process_payment",
		Error:        errors.New("insufficient funds"),
		Duration:     10 * time.Millisecond,
		Attempt:      3,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("expected status Error, got %v", span.Status().Code)
	}
}

func TestActivityRetry(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnActivityStart(ctx, hooks.ActivityStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "fetch_data:1",
		ActivityName: "fetch_data",
		IsReplay:     false,
	})

	// Record retry
	h.OnActivityRetry(ctx, hooks.ActivityRetryInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "fetch_data:1",
		ActivityName: "fetch_data",
		Attempt:      1,
		MaxAttempts:  3,
		NextDelay:    5 * time.Second,
		Error:        errors.New("timeout"),
	})

	// Complete activity
	h.OnActivityComplete(ctx, hooks.ActivityCompleteInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "fetch_data:1",
		ActivityName: "fetch_data",
		Duration:     100 * time.Millisecond,
		IsReplay:     false,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	// Check that retry event was added
	span := spans[0]
	events := span.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event (retry), got %d", len(events))
	}
	if events[0].Name != "activity_retry" {
		t.Errorf("expected event name 'activity_retry', got %s", events[0].Name)
	}
}

func TestActivityCacheHit(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnActivityCacheHit(ctx, hooks.ActivityCacheHitInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		ActivityID:   "reserve_inventory:1",
		ActivityName: "reserve_inventory",
		CachedResult: map[string]bool{"reserved": true},
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "activity_cache_hit/reserve_inventory" {
		t.Errorf("expected span name 'activity_cache_hit/reserve_inventory', got %s", span.Name())
	}
}

func TestEventHandling(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	// Test event received
	h.OnEventReceived(ctx, hooks.EventReceivedInfo{
		EventType:   "payment.completed",
		EventID:     "evt-001",
		EventSource: "payment-service",
		InstanceID:  "wf-123",
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "event_received/payment.completed" {
		t.Errorf("expected span name 'event_received/payment.completed', got %s", span.Name())
	}
	if span.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("expected SpanKindConsumer, got %v", span.SpanKind())
	}
}

func TestEventWait(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	timeout := 30 * time.Second

	// Start waiting for event
	h.OnEventWaitStart(ctx, hooks.EventWaitStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		EventType:    "payment.completed",
		Timeout:      &timeout,
	})

	// Event received
	h.OnEventWaitComplete(ctx, hooks.EventWaitCompleteInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		EventType:    "payment.completed",
		Duration:     5 * time.Second,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "event_wait/payment.completed" {
		t.Errorf("expected span name 'event_wait/payment.completed', got %s", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("expected status OK, got %v", span.Status().Code)
	}
}

func TestEventTimeout(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	timeout := 30 * time.Second

	h.OnEventWaitStart(ctx, hooks.EventWaitStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		EventType:    "payment.completed",
		Timeout:      &timeout,
	})

	h.OnEventTimeout(ctx, hooks.EventTimeoutInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		EventType:    "payment.completed",
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("expected status Error (timeout), got %v", span.Status().Code)
	}
}

func TestTimerHandling(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	expiresAt := time.Now().Add(60 * time.Second)

	h.OnTimerStart(ctx, hooks.TimerStartInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		TimerID:      "payment_timeout",
		ExpiresAt:    expiresAt,
	})

	h.OnTimerFired(ctx, hooks.TimerFiredInfo{
		InstanceID:   "wf-123",
		WorkflowName: "order_workflow",
		TimerID:      "payment_timeout",
		FiredAt:      expiresAt,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "timer/payment_timeout" {
		t.Errorf("expected span name 'timer/payment_timeout', got %s", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("expected status OK, got %v", span.Status().Code)
	}
}

func TestReplayLifecycle(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnReplayStart(ctx, hooks.ReplayStartInfo{
		InstanceID:     "wf-123",
		WorkflowName:   "order_workflow",
		HistoryEvents:  5,
		ResumeActivity: "process_payment:1",
	})

	h.OnReplayComplete(ctx, hooks.ReplayCompleteInfo{
		InstanceID:    "wf-123",
		WorkflowName:  "order_workflow",
		CacheHits:     3,
		NewActivities: 2,
		Duration:      10 * time.Millisecond,
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name() != "replay/order_workflow" {
		t.Errorf("expected span name 'replay/order_workflow', got %s", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("expected status OK, got %v", span.Status().Code)
	}

	attrs := span.Attributes()
	checkAttributeInt(t, attrs, "romancy.cache_hits", 3)
	checkAttributeInt(t, attrs, "romancy.new_activities", 2)
}

func TestImplementsInterface(t *testing.T) {
	var _ hooks.WorkflowHooks = (*OTelHooks)(nil)
}

// Helper functions

func checkAttribute(t *testing.T, attrs []attribute.KeyValue, key, expected string) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if attr.Value.AsString() != expected {
				t.Errorf("expected attribute %s=%s, got %s", key, expected, attr.Value.AsString())
			}
			return
		}
	}
	t.Errorf("attribute %s not found", key)
}

func checkAttributeInt(t *testing.T, attrs []attribute.KeyValue, key string, expected int) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if attr.Value.AsInt64() != int64(expected) {
				t.Errorf("expected attribute %s=%d, got %d", key, expected, attr.Value.AsInt64())
			}
			return
		}
	}
	t.Errorf("attribute %s not found", key)
}

// Workflows run concurrently, so their hooks are called from many goroutines
// at once (run with -race).
func TestConcurrentWorkflows(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	const workflows = 50
	var wg sync.WaitGroup
	for i := 0; i < workflows; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("wf-%d", i)
			h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: id, WorkflowName: "order_workflow"})
			h.OnActivityStart(ctx, hooks.ActivityStartInfo{InstanceID: id, ActivityID: "reserve:1", ActivityName: "reserve"})
			h.OnActivityRetry(ctx, hooks.ActivityRetryInfo{InstanceID: id, ActivityID: "reserve:1", Attempt: 1, Error: errors.New("busy")})
			h.OnActivityComplete(ctx, hooks.ActivityCompleteInfo{InstanceID: id, ActivityID: "reserve:1", ActivityName: "reserve"})
			h.OnEventWaitStart(ctx, hooks.EventWaitStartInfo{InstanceID: id, EventType: "paid"})
			h.OnEventWaitComplete(ctx, hooks.EventWaitCompleteInfo{InstanceID: id, EventType: "paid"})
			h.OnTimerStart(ctx, hooks.TimerStartInfo{InstanceID: id, TimerID: "sleep:1"})
			h.OnTimerFired(ctx, hooks.TimerFiredInfo{InstanceID: id, TimerID: "sleep:1"})
			h.OnReplayStart(ctx, hooks.ReplayStartInfo{InstanceID: id})
			h.OnReplayComplete(ctx, hooks.ReplayCompleteInfo{InstanceID: id})
			h.OnWorkflowComplete(ctx, hooks.WorkflowCompleteInfo{InstanceID: id, WorkflowName: "order_workflow"})
		}(i)
	}
	wg.Wait()

	// Each workflow ends a workflow, activity, event wait, timer and replay span.
	if got := len(sr.Ended()); got != workflows*5 {
		t.Fatalf("expected %d ended spans, got %d", workflows*5, got)
	}
}

// A run that suspended leaves its workflow span open; the next run of the same
// instance ends it before starting its own.
func TestWorkflowStartEndsEarlierRunSpan(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: "wf-1", WorkflowName: "order_workflow"})
	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: "wf-1", WorkflowName: "order_workflow"})
	if got := len(sr.Ended()); got != 1 {
		t.Fatalf("expected the first run's span to end, got %d ended spans", got)
	}

	h.OnWorkflowComplete(ctx, hooks.WorkflowCompleteInfo{InstanceID: "wf-1", WorkflowName: "order_workflow"})
	if got := len(sr.Ended()); got != 2 {
		t.Fatalf("expected 2 ended spans, got %d", got)
	}
}

// OnActivityStart fires for every activity call, including one whose result
// comes from history: the cache hit ends the span it opened.
func TestActivityCacheHitEndsStartedSpan(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()

	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: "wf-1", WorkflowName: "order_workflow"})
	h.OnActivityStart(ctx, hooks.ActivityStartInfo{
		InstanceID: "wf-1", ActivityID: "reserve:1", ActivityName: "reserve", IsReplay: true,
	})
	h.OnActivityCacheHit(ctx, hooks.ActivityCacheHitInfo{
		InstanceID: "wf-1", ActivityID: "reserve:1", ActivityName: "reserve",
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(spans))
	}
	if spans[0].Name() != "activity_cache_hit/reserve" {
		t.Errorf("expected span name 'activity_cache_hit/reserve', got %s", spans[0].Name())
	}
	if !spans[0].Parent().IsValid() {
		t.Error("expected the cache-hit span to be a child of the workflow span")
	}
	if n := len(h.activitySpans); n != 0 {
		t.Errorf("expected no open activity spans, got %d", n)
	}
}

// openRunSpans opens a workflow, replay, activity, event wait and timer span
// for an instance.
func openRunSpans(h *OTelHooks, instanceID string) {
	ctx := context.Background()
	h.OnReplayStart(ctx, hooks.ReplayStartInfo{InstanceID: instanceID, WorkflowName: "wf"})
	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: instanceID, WorkflowName: "wf", StartTime: time.Now()})
	h.OnActivityStart(ctx, hooks.ActivityStartInfo{InstanceID: instanceID, WorkflowName: "wf", ActivityID: "step:1", ActivityName: "step"})
	h.OnEventWaitStart(ctx, hooks.EventWaitStartInfo{InstanceID: instanceID, WorkflowName: "wf", EventType: "approved"})
	h.OnTimerStart(ctx, hooks.TimerStartInfo{InstanceID: instanceID, WorkflowName: "wf", TimerID: "sleep:1", ExpiresAt: time.Now().Add(time.Hour)})
}

// openSpanCount counts the spans the hooks hold open.
func openSpanCount(h *OTelHooks) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.workflowSpans) + len(h.workflowContexts) + len(h.replaySpans) +
		len(h.activitySpans) + len(h.eventWaitSpans) + len(h.timerSpans)
}

// A run that suspends ends every span the process holds for its instance,
// and only for it: a supervisor's child, whose ID starts with the
// supervisor's, keeps its spans.
func TestWorkflowSuspendedEndsInstanceSpans(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()
	openRunSpans(h, "sup")
	openRunSpans(h, "sup:child:g1")

	h.OnWorkflowSuspended(ctx, hooks.WorkflowSuspendedInfo{
		InstanceID: "sup", WorkflowName: "wf", Reason: hooks.SuspendWaiting, Duration: time.Millisecond,
	})

	ended := sr.Ended()
	if len(ended) != 5 {
		t.Fatalf("ended %d spans, want the 5 of the instance", len(ended))
	}
	for _, span := range ended {
		checkAttribute(t, span.Attributes(), "romancy.instance_id", "sup")
		checkAttribute(t, span.Attributes(), "romancy.suspend_reason", "waiting")
		if span.Status().Code != codes.Unset {
			t.Errorf("span %s status = %v, want unset", span.Name(), span.Status().Code)
		}
	}
	if got := openSpanCount(h); got != 6 { // the child's 5 spans and its workflow context
		t.Fatalf("%d entries open, want the child's 6", got)
	}

	// An aborted run records its error on the workflow span.
	h.OnWorkflowSuspended(ctx, hooks.WorkflowSuspendedInfo{
		InstanceID: "sup:child:g1", WorkflowName: "wf", Reason: hooks.SuspendAborted, Error: errors.New("lock lost"),
	})
	ended = sr.Ended()
	if len(ended) != 10 {
		t.Fatalf("ended %d spans, want 10", len(ended))
	}
	var recorded bool
	for _, span := range ended[5:] {
		if span.Name() == "workflow/wf" {
			for _, event := range span.Events() {
				recorded = recorded || event.Name == "exception"
			}
		}
	}
	if !recorded {
		t.Error("the aborted run's workflow span has no error event")
	}
	if got := openSpanCount(h); got != 0 {
		t.Errorf("%d entries still open, want 0", got)
	}
}

// A replay span left open by an earlier run is ended when the next replay
// starts, and a failed run ends its replay span with its workflow span.
func TestReplaySpansAreEnded(t *testing.T) {
	h, sr := setupTest()
	ctx := context.Background()
	h.OnReplayStart(ctx, hooks.ReplayStartInfo{InstanceID: "wf-1", WorkflowName: "wf"})
	h.OnReplayStart(ctx, hooks.ReplayStartInfo{InstanceID: "wf-1", WorkflowName: "wf"})
	if got := len(sr.Ended()); got != 1 {
		t.Fatalf("ended %d spans, want the earlier replay span", got)
	}

	h.OnWorkflowStart(ctx, hooks.WorkflowStartInfo{InstanceID: "wf-1", WorkflowName: "wf", StartTime: time.Now()})
	h.OnWorkflowFailed(ctx, hooks.WorkflowFailedInfo{InstanceID: "wf-1", WorkflowName: "wf", Error: errors.New("boom")})
	if got := len(sr.Ended()); got != 3 {
		t.Fatalf("ended %d spans, want 3", got)
	}
	if got := openSpanCount(h); got != 0 {
		t.Errorf("%d entries still open, want 0", got)
	}
}

// A timer whose span ended with its run is marked by a timer_fired span when
// it fires.
func TestTimerFiredWithoutOpenSpan(t *testing.T) {
	h, sr := setupTest()
	h.OnTimerFired(context.Background(), hooks.TimerFiredInfo{
		InstanceID: "wf-1", WorkflowName: "wf", TimerID: "sleep:1", FiredAt: time.Now(),
	})
	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "timer_fired/sleep:1" {
		t.Fatalf("ended %d spans (%v), want one timer_fired/sleep:1", len(ended), ended)
	}
	checkAttribute(t, ended[0].Attributes(), "romancy.instance_id", "wf-1")
}

// A timeout handled after its wait's run ended is marked by an event_timeout
// span with error status.
func TestEventTimeoutWithoutOpenSpan(t *testing.T) {
	h, sr := setupTest()
	h.OnEventTimeout(context.Background(), hooks.EventTimeoutInfo{
		InstanceID: "wf-1", WorkflowName: "wf", EventType: "payment.done",
	})
	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "event_timeout/payment.done" {
		t.Fatalf("ended %d spans (%v), want one event_timeout/payment.done", len(ended), ended)
	}
	if ended[0].Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", ended[0].Status().Code)
	}
	checkAttribute(t, ended[0].Attributes(), "romancy.event_type", "payment.done")
}
