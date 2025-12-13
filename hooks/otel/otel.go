// Package otel provides OpenTelemetry integration for Romancy workflow hooks.
package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/i2y/romancy/hooks"
)

const (
	tracerName = "romancy"
)

// OTelHooks implements WorkflowHooks with OpenTelemetry tracing.
// It creates spans for workflow, activity, event, timer, and replay lifecycle events.
type OTelHooks struct {
	hooks.NoOpHooks
	tracer trace.Tracer

	// Map of instance_id -> active span for tracking workflow spans
	workflowSpans map[string]trace.Span

	// Map of instance_id -> context with workflow span for child spans
	workflowContexts map[string]context.Context

	// Map of instance_id:activity_id -> active span for tracking activity spans
	activitySpans map[string]trace.Span

	// Map of instance_id:event_type -> active span for tracking event wait spans
	eventWaitSpans map[string]trace.Span

	// Map of instance_id:timer_id -> active span for tracking timer spans
	timerSpans map[string]trace.Span

	// Map of instance_id -> active span for tracking replay spans
	replaySpans map[string]trace.Span
}

// NewOTelHooks creates a new OpenTelemetry hooks instance.
// If tracerProvider is nil, the global tracer provider is used.
func NewOTelHooks(tracerProvider trace.TracerProvider) *OTelHooks {
	var tracer trace.Tracer
	if tracerProvider != nil {
		tracer = tracerProvider.Tracer(tracerName)
	} else {
		tracer = otel.Tracer(tracerName)
	}

	return &OTelHooks{
		tracer:           tracer,
		workflowSpans:    make(map[string]trace.Span),
		workflowContexts: make(map[string]context.Context),
		activitySpans:    make(map[string]trace.Span),
		eventWaitSpans:   make(map[string]trace.Span),
		timerSpans:       make(map[string]trace.Span),
		replaySpans:      make(map[string]trace.Span),
	}
}

// Workflow lifecycle

// OnWorkflowStart creates a new span when a workflow starts.
func (h *OTelHooks) OnWorkflowStart(ctx context.Context, info hooks.WorkflowStartInfo) {
	spanCtx, span := h.tracer.Start(ctx, fmt.Sprintf("workflow/%s", info.WorkflowName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("romancy.instance_id", info.InstanceID),
			attribute.String("romancy.workflow_name", info.WorkflowName),
			attribute.String("romancy.start_time", info.StartTime.String()),
		),
	)
	h.workflowSpans[info.InstanceID] = span
	h.workflowContexts[info.InstanceID] = spanCtx
}

// OnWorkflowComplete ends the workflow span with success status.
func (h *OTelHooks) OnWorkflowComplete(ctx context.Context, info hooks.WorkflowCompleteInfo) {
	if span, ok := h.workflowSpans[info.InstanceID]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
		)
		span.SetStatus(codes.Ok, "workflow completed")
		span.End()
		delete(h.workflowSpans, info.InstanceID)
		delete(h.workflowContexts, info.InstanceID)
	}
}

// OnWorkflowFailed ends the workflow span with error status.
func (h *OTelHooks) OnWorkflowFailed(ctx context.Context, info hooks.WorkflowFailedInfo) {
	if span, ok := h.workflowSpans[info.InstanceID]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
		)
		span.RecordError(info.Error)
		span.SetStatus(codes.Error, info.Error.Error())
		span.End()
		delete(h.workflowSpans, info.InstanceID)
		delete(h.workflowContexts, info.InstanceID)
	}
}

// OnWorkflowCancelled ends the workflow span with cancellation status.
func (h *OTelHooks) OnWorkflowCancelled(ctx context.Context, info hooks.WorkflowCancelledInfo) {
	if span, ok := h.workflowSpans[info.InstanceID]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
			attribute.String("romancy.cancel_reason", info.Reason),
		)
		span.SetStatus(codes.Error, "workflow canceled: "+info.Reason)
		span.End()
		delete(h.workflowSpans, info.InstanceID)
		delete(h.workflowContexts, info.InstanceID)
	}
}

// Activity lifecycle

func (h *OTelHooks) activityKey(instanceID, activityID string) string {
	return instanceID + ":" + activityID
}

// OnActivityStart creates a new span when an activity starts.
// The activity span is created as a child of the workflow span.
func (h *OTelHooks) OnActivityStart(ctx context.Context, info hooks.ActivityStartInfo) {
	// Use workflow context if available to create child span
	parentCtx := ctx
	if wfCtx, ok := h.workflowContexts[info.InstanceID]; ok {
		parentCtx = wfCtx
	}

	_, span := h.tracer.Start(parentCtx, fmt.Sprintf("activity/%s", info.ActivityName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("romancy.instance_id", info.InstanceID),
			attribute.String("romancy.workflow_name", info.WorkflowName),
			attribute.String("romancy.activity_id", info.ActivityID),
			attribute.String("romancy.activity_name", info.ActivityName),
			attribute.Bool("romancy.is_replay", info.IsReplay),
		),
	)
	h.activitySpans[h.activityKey(info.InstanceID, info.ActivityID)] = span
}

// OnActivityComplete ends the activity span with success status.
func (h *OTelHooks) OnActivityComplete(ctx context.Context, info hooks.ActivityCompleteInfo) {
	key := h.activityKey(info.InstanceID, info.ActivityID)
	if span, ok := h.activitySpans[key]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
			attribute.Bool("romancy.is_replay", info.IsReplay),
		)
		span.SetStatus(codes.Ok, "activity completed")
		span.End()
		delete(h.activitySpans, key)
	}
}

// OnActivityFailed ends the activity span with error status.
func (h *OTelHooks) OnActivityFailed(ctx context.Context, info hooks.ActivityFailedInfo) {
	key := h.activityKey(info.InstanceID, info.ActivityID)
	if span, ok := h.activitySpans[key]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
			attribute.Int("romancy.attempt", info.Attempt),
		)
		span.RecordError(info.Error)
		span.SetStatus(codes.Error, info.Error.Error())
		span.End()
		delete(h.activitySpans, key)
	}
}

// OnActivityRetry records a retry event on the activity span.
func (h *OTelHooks) OnActivityRetry(ctx context.Context, info hooks.ActivityRetryInfo) {
	key := h.activityKey(info.InstanceID, info.ActivityID)
	if span, ok := h.activitySpans[key]; ok {
		span.AddEvent("activity_retry",
			trace.WithAttributes(
				attribute.Int("romancy.attempt", info.Attempt),
				attribute.Int("romancy.max_attempts", info.MaxAttempts),
				attribute.Int64("romancy.next_delay_ms", info.NextDelay.Milliseconds()),
				attribute.String("romancy.error", info.Error.Error()),
			),
		)
	}
}

// OnActivityCacheHit records a cache hit event during replay.
func (h *OTelHooks) OnActivityCacheHit(ctx context.Context, info hooks.ActivityCacheHitInfo) {
	_, span := h.tracer.Start(ctx, fmt.Sprintf("activity_cache_hit/%s", info.ActivityName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("romancy.instance_id", info.InstanceID),
			attribute.String("romancy.workflow_name", info.WorkflowName),
			attribute.String("romancy.activity_id", info.ActivityID),
			attribute.String("romancy.activity_name", info.ActivityName),
		),
	)
	span.SetStatus(codes.Ok, "cache hit")
	span.End()
}

// Event handling

func (h *OTelHooks) eventWaitKey(instanceID, eventType string) string {
	return instanceID + ":" + eventType
}

// OnEventReceived creates a span for a received event.
func (h *OTelHooks) OnEventReceived(ctx context.Context, info hooks.EventReceivedInfo) {
	_, span := h.tracer.Start(ctx, fmt.Sprintf("event_received/%s", info.EventType),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("romancy.event_type", info.EventType),
			attribute.String("romancy.event_id", info.EventID),
			attribute.String("romancy.event_source", info.EventSource),
			attribute.String("romancy.instance_id", info.InstanceID),
		),
	)
	span.SetStatus(codes.Ok, "event received")
	span.End()
}

// OnEventWaitStart creates a span when waiting for an event starts.
func (h *OTelHooks) OnEventWaitStart(ctx context.Context, info hooks.EventWaitStartInfo) {
	attrs := []attribute.KeyValue{
		attribute.String("romancy.instance_id", info.InstanceID),
		attribute.String("romancy.workflow_name", info.WorkflowName),
		attribute.String("romancy.event_type", info.EventType),
	}
	if info.Timeout != nil {
		attrs = append(attrs, attribute.Int64("romancy.timeout_ms", info.Timeout.Milliseconds()))
	}

	_, span := h.tracer.Start(ctx, fmt.Sprintf("event_wait/%s", info.EventType),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	h.eventWaitSpans[h.eventWaitKey(info.InstanceID, info.EventType)] = span
}

// OnEventWaitComplete ends the event wait span with success status.
func (h *OTelHooks) OnEventWaitComplete(ctx context.Context, info hooks.EventWaitCompleteInfo) {
	key := h.eventWaitKey(info.InstanceID, info.EventType)
	if span, ok := h.eventWaitSpans[key]; ok {
		span.SetAttributes(
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
		)
		span.SetStatus(codes.Ok, "event received")
		span.End()
		delete(h.eventWaitSpans, key)
	}
}

// OnEventTimeout ends the event wait span with timeout status.
func (h *OTelHooks) OnEventTimeout(ctx context.Context, info hooks.EventTimeoutInfo) {
	key := h.eventWaitKey(info.InstanceID, info.EventType)
	if span, ok := h.eventWaitSpans[key]; ok {
		span.SetStatus(codes.Error, "event timeout")
		span.End()
		delete(h.eventWaitSpans, key)
	}
}

// Timer handling

func (h *OTelHooks) timerKey(instanceID, timerID string) string {
	return instanceID + ":" + timerID
}

// OnTimerStart creates a span when a timer starts.
func (h *OTelHooks) OnTimerStart(ctx context.Context, info hooks.TimerStartInfo) {
	_, span := h.tracer.Start(ctx, fmt.Sprintf("timer/%s", info.TimerID),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("romancy.instance_id", info.InstanceID),
			attribute.String("romancy.workflow_name", info.WorkflowName),
			attribute.String("romancy.timer_id", info.TimerID),
			attribute.String("romancy.expires_at", info.ExpiresAt.String()),
		),
	)
	h.timerSpans[h.timerKey(info.InstanceID, info.TimerID)] = span
}

// OnTimerFired ends the timer span when the timer fires.
func (h *OTelHooks) OnTimerFired(ctx context.Context, info hooks.TimerFiredInfo) {
	key := h.timerKey(info.InstanceID, info.TimerID)
	if span, ok := h.timerSpans[key]; ok {
		span.SetAttributes(
			attribute.String("romancy.fired_at", info.FiredAt.String()),
		)
		span.SetStatus(codes.Ok, "timer fired")
		span.End()
		delete(h.timerSpans, key)
	}
}

// Replay

// OnReplayStart creates a span when replay starts.
func (h *OTelHooks) OnReplayStart(ctx context.Context, info hooks.ReplayStartInfo) {
	_, span := h.tracer.Start(ctx, fmt.Sprintf("replay/%s", info.WorkflowName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("romancy.instance_id", info.InstanceID),
			attribute.String("romancy.workflow_name", info.WorkflowName),
			attribute.Int("romancy.history_events", info.HistoryEvents),
			attribute.String("romancy.resume_activity", info.ResumeActivity),
		),
	)
	h.replaySpans[info.InstanceID] = span
}

// OnReplayComplete ends the replay span with success status.
func (h *OTelHooks) OnReplayComplete(ctx context.Context, info hooks.ReplayCompleteInfo) {
	if span, ok := h.replaySpans[info.InstanceID]; ok {
		span.SetAttributes(
			attribute.Int("romancy.cache_hits", info.CacheHits),
			attribute.Int("romancy.new_activities", info.NewActivities),
			attribute.Int64("romancy.duration_ms", info.Duration.Milliseconds()),
		)
		span.SetStatus(codes.Ok, "replay completed")
		span.End()
		delete(h.replaySpans, info.InstanceID)
	}
}

// Ensure OTelHooks implements WorkflowHooks interface
var _ hooks.WorkflowHooks = (*OTelHooks)(nil)
