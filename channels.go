// Package romancy provides a durable execution framework for Go.
package romancy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
)

// ChannelMode defines the message delivery mode for a channel subscription.
type ChannelMode = storage.ChannelMode

const (
	// ModeBroadcast delivers messages to all subscribers.
	// Each subscriber receives every message.
	ModeBroadcast ChannelMode = storage.ChannelModeBroadcast

	// ModeCompeting delivers each message to exactly one subscriber.
	// Multiple subscribers compete for messages (work queue pattern).
	ModeCompeting ChannelMode = storage.ChannelModeCompeting

	// ModeDirect receives messages sent via SendTo to this workflow instance.
	// This is syntactic sugar that subscribes to "channel:instanceID" internally.
	// Use with SendTo for point-to-point messaging.
	//
	// Example:
	//   // Receiver workflow
	//   romancy.Subscribe(ctx, "requests", romancy.ModeDirect)
	//   msg, err := romancy.Receive[Request](ctx, "requests")
	//
	//   // Sender workflow
	//   romancy.SendTo(ctx, targetInstanceID, "requests", request)
	ModeDirect ChannelMode = storage.ChannelModeDirect
)

// ReceivedMessage represents a message received from a channel.
type ReceivedMessage[T any] struct {
	// Message ID
	ID int64 `json:"id"`

	// Channel name
	ChannelName string `json:"channel_name"`

	// Message data
	Data T `json:"data"`

	// Metadata (optional)
	Metadata map[string]any `json:"metadata,omitempty"`

	// Sender instance ID (if sent via SendTo)
	SenderInstanceID string `json:"sender_instance_id,omitempty"`

	// When the message was created
	CreatedAt time.Time `json:"created_at"`
}

// Subscribe registers the workflow to receive messages from a channel.
// The mode determines how messages are delivered:
// - ModeBroadcast: All subscribers receive every message
// - ModeCompeting: Each message goes to exactly one subscriber
// - ModeDirect: Receives messages sent via SendTo to this instance
//
// Subscriptions persist across workflow restarts and are automatically
// cleaned up when the workflow completes.
func Subscribe(ctx *WorkflowContext, channelName string, mode ChannelMode) error {
	// Handle ModeDirect: convert to actual channel name and record for Receive
	actualChannel := channelName
	actualMode := mode
	if mode == ModeDirect {
		// ModeDirect subscribes to "channel:instanceID"
		actualChannel = fmt.Sprintf("%s:%s", channelName, ctx.InstanceID())
		actualMode = ModeBroadcast // Direct channels use broadcast internally
		// Record so Receive knows to use the direct channel
		ctx.recordDirectSubscription(channelName)
	}

	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("subscribe_%s", channelName))

	// Check if already subscribed (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		// Still need to record direct subscription on replay
		if mode == ModeDirect {
			ctx.recordDirectSubscription(channelName)
		}
		return nil
	}

	// Subscribe to channel
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	if err := s.SubscribeToChannel(ctx.Context(), ctx.InstanceID(), actualChannel, actualMode); err != nil {
		return fmt.Errorf("failed to subscribe to channel: %w", err)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// Unsubscribe removes the workflow's subscription to a channel.
func Unsubscribe(ctx *WorkflowContext, channelName string) error {
	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("unsubscribe_%s", channelName))

	// Check if already unsubscribed (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		return nil
	}

	// Unsubscribe from channel
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	if err := s.UnsubscribeFromChannel(ctx.Context(), ctx.InstanceID(), channelName); err != nil {
		return fmt.Errorf("failed to unsubscribe from channel: %w", err)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// ReceiveOption configures channel receive behavior.
type ReceiveOption func(*receiveOptions)

type receiveOptions struct {
	timeout *time.Duration
}

// WithReceiveTimeout sets a timeout for waiting for a message.
func WithReceiveTimeout(d time.Duration) ReceiveOption {
	return func(o *receiveOptions) {
		o.timeout = &d
	}
}

// Receive waits for and receives a message from a channel.
// The workflow must be subscribed to the channel before calling Receive.
//
// This is a blocking operation - the workflow will be suspended until
// a message is available or the optional timeout expires.
//
// During replay, if the message was already received, this returns immediately.
//
// For channels subscribed with ModeDirect, this automatically receives from
// the direct channel (channel:instanceID) without needing to specify it.
func Receive[T any](ctx *WorkflowContext, channelName string, opts ...ReceiveOption) (*ReceivedMessage[T], error) {
	options := &receiveOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Check if this channel was subscribed with ModeDirect
	actualChannel := channelName
	if ctx.isDirectSubscription(channelName) {
		actualChannel = fmt.Sprintf("%s:%s", channelName, ctx.InstanceID())
	}

	startTime := time.Now()

	// Generate activity ID for this receive (use original channelName for consistency)
	activityID := ctx.GenerateActivityID(fmt.Sprintf("receive_%s", channelName))

	// Check if result is cached (replay)
	// First try raw JSON to avoid re-serialization
	if rawJSON, ok := ctx.GetCachedResultRaw(activityID); ok && len(rawJSON) > 0 {
		// Try Go format first (ReceivedMessage with channel_name, created_at)
		var msg ReceivedMessage[T]
		if err := json.Unmarshal(rawJSON, &msg); err == nil && msg.ChannelName != "" {
			return &msg, nil
		}

		// Try Python/cross-language format (channel, published_at)
		var pythonFormat struct {
			ID          any             `json:"id"`
			Channel     string          `json:"channel"`
			ChannelName string          `json:"channel_name"`
			Data        json.RawMessage `json:"data"`
			Metadata    map[string]any  `json:"metadata"`
			PublishedAt string          `json:"published_at"`
			CreatedAt   *time.Time      `json:"created_at"`
		}
		if err := json.Unmarshal(rawJSON, &pythonFormat); err == nil {
			// Resolve channel name (prefer channel_name, fallback to channel)
			resolvedChannel := pythonFormat.ChannelName
			if resolvedChannel == "" {
				resolvedChannel = pythonFormat.Channel
			}

			// Unmarshal data to type T
			var data T
			if len(pythonFormat.Data) > 0 {
				if err := json.Unmarshal(pythonFormat.Data, &data); err != nil {
					return nil, fmt.Errorf("failed to deserialize cached message data: %w", err)
				}
			}

			// Resolve time (prefer created_at, fallback to published_at)
			var createdAt time.Time
			if pythonFormat.CreatedAt != nil {
				createdAt = *pythonFormat.CreatedAt
			} else if pythonFormat.PublishedAt != "" {
				// Try multiple ISO 8601 formats
				for _, layout := range []string{
					time.RFC3339,
					time.RFC3339Nano,
					"2006-01-02T15:04:05.999999",
					"2006-01-02T15:04:05",
				} {
					if t, err := time.Parse(layout, pythonFormat.PublishedAt); err == nil {
						createdAt = t
						break
					}
				}
			}

			// Resolve ID (handle both int64 and string UUID)
			var id int64
			switch v := pythonFormat.ID.(type) {
			case float64:
				id = int64(v)
			case int64:
				id = v
			case string:
				// UUID string - use 0 as fallback (ID is mostly for logging)
				id = 0
			}

			return &ReceivedMessage[T]{
				ID:          id,
				ChannelName: resolvedChannel,
				Data:        data,
				Metadata:    pythonFormat.Metadata,
				CreatedAt:   createdAt,
			}, nil
		}
	}
	// Also check GetCachedResult which handles cached errors
	if result, ok := ctx.GetCachedResult(activityID); ok {
		// Check if it's an error (e.g., timeout)
		if err, isErr := result.(error); isErr {
			// Convert generic timeout error to ChannelMessageTimeoutError
			// so that errors.As works correctly
			if strings.Contains(err.Error(), "timeout waiting for channel message") {
				return nil, &ChannelMessageTimeoutError{
					InstanceID:  ctx.InstanceID(),
					ChannelName: channelName,
				}
			}
			return nil, err
		}
		if msg, ok := result.(*ReceivedMessage[T]); ok {
			return msg, nil
		}
		return nil, fmt.Errorf("cached result type mismatch for channel receive %s", activityID)
	}

	// Verify subscription exists
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}

	sub, err := s.GetChannelSubscription(ctx.Context(), ctx.InstanceID(), actualChannel)
	if err != nil || sub == nil {
		return nil, ErrChannelNotSubscribed
	}

	// Check for pending messages before suspending (Edda-style)
	pendingMessages, err := s.GetPendingChannelMessagesForInstance(ctx.Context(), ctx.InstanceID(), actualChannel)
	if err != nil {
		return nil, fmt.Errorf("failed to check pending messages: %w", err)
	}

	if len(pendingMessages) > 0 {
		// Message is already available - process it immediately
		msg := pendingMessages[0]

		// For competing mode, claim the message
		if sub.Mode == storage.ChannelModeCompeting {
			claimed, err := s.ClaimChannelMessage(ctx.Context(), msg.ID, ctx.InstanceID())
			if err != nil {
				return nil, fmt.Errorf("failed to claim message: %w", err)
			}
			if !claimed {
				// Message was claimed by another instance, try again
				return Receive[T](ctx, channelName, opts...)
			}
		}

		// Update cursor for broadcast mode
		if sub.Mode == storage.ChannelModeBroadcast {
			if err := s.UpdateDeliveryCursor(ctx.Context(), ctx.InstanceID(), actualChannel, msg.ID); err != nil {
				return nil, fmt.Errorf("failed to update cursor: %w", err)
			}
		}

		// Deserialize the message
		var data T
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			return nil, fmt.Errorf("failed to deserialize message data: %w", err)
		}

		var metadata map[string]any
		if len(msg.Metadata) > 0 {
			if err := json.Unmarshal(msg.Metadata, &metadata); err != nil {
				return nil, fmt.Errorf("failed to deserialize metadata: %w", err)
			}
		}

		result := &ReceivedMessage[T]{
			ID:          msg.ID,
			ChannelName: channelName,
			Data:        data,
			Metadata:    metadata,
			CreatedAt:   msg.PublishedAt,
		}

		// Record to history for replay (synchronous message processing)
		if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
			return nil, fmt.Errorf("failed to record activity result: %w", err)
		}

		// Cache for replay
		ctx.SetCachedResult(activityID, result)

		// Call OnEventWaitComplete hook
		if h := ctx.Hooks(); h != nil {
			h.OnEventWaitComplete(ctx.Context(), hooks.EventWaitCompleteInfo{
				InstanceID:   ctx.InstanceID(),
				WorkflowName: ctx.WorkflowName(),
				EventType:    channelName,
				Duration:     time.Since(startTime),
			})
		}

		return result, nil
	}

	// No pending messages - need to wait for message
	// Record the activity ID for tracking
	ctx.RecordActivityID(activityID)

	// Return SuspendSignal that the replay engine will handle
	// Include the activityID so it can be used when recording history on message delivery
	// Use actualChannel so it wakes up on the correct channel (including direct channels)
	return nil, NewChannelMessageSuspend(ctx.InstanceID(), actualChannel, activityID, options.timeout)
}

// Publish sends a message to all subscribers of a channel.
// This is an activity that persists the message to storage.
//
// The message will be delivered to:
// - Broadcast subscribers: All receive the message
// - Competing subscribers: Exactly one receives the message
func Publish[T any](ctx *WorkflowContext, channelName string, data T, opts ...PublishOption) error {
	options := &publishOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("publish_%s", channelName))

	// Check if already published (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		return nil
	}

	// Serialize data
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize message data: %w", err)
	}

	// Serialize metadata
	var metadataJSON []byte
	if options.metadata != nil {
		metadataJSON, err = json.Marshal(options.metadata)
		if err != nil {
			return fmt.Errorf("failed to serialize message metadata: %w", err)
		}
	}

	// Publish to channel
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	messageID, err := s.PublishToChannel(ctx.Context(), channelName, dataJSON, metadataJSON)
	if err != nil {
		return fmt.Errorf("failed to publish to channel: %w", err)
	}

	// Wake up waiting subscribers (Edda-style: delivery + load balancing)
	// If in transaction, defer delivery until commit to ensure consistency
	if s.InTransaction(ctx.Context()) {
		// Capture variables for closure
		wfCtx := ctx
		store := s
		chName := channelName
		msgID := messageID
		dJSON := dataJSON
		mJSON := metadataJSON

		_ = s.RegisterPostCommitCallback(ctx.Context(), func() error {
			wakeWaitingSubscribers(wfCtx, store, chName, msgID, dJSON, mJSON)
			return nil
		})
	} else {
		// Deliver immediately
		wakeWaitingSubscribers(ctx, s, channelName, messageID, dataJSON, metadataJSON)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// wakeWaitingSubscribers delivers a message to waiting subscribers using Lock-First pattern.
// This enables load balancing in multi-worker environments:
// 1. For each waiting subscriber, try to acquire lock
// 2. If lock acquired: record history, set status='running', release lock
// 3. The runWorkflowResumption task will pick up the workflow
// For competing mode, only one subscriber is woken up.
// For broadcast mode, all subscribers are woken up.
// Note: SendTo uses dynamic channel names (e.g., "channel:instance_id") to ensure
// only the target instance receives the message, so no filtering is needed here.
func wakeWaitingSubscribers(
	ctx *WorkflowContext,
	s storage.Storage,
	channelName string,
	messageID int64,
	dataJSON []byte,
	metadataJSON []byte,
) {
	// Find waiting subscribers
	waitingSubs, err := s.GetChannelSubscribersWaiting(ctx.Context(), channelName)
	if err != nil {
		// Log error but don't fail the publish
		slog.Error("error getting waiting subscribers", "error", err)
		return
	}

	if len(waitingSubs) == 0 {
		return
	}

	// Create message for delivery
	message := &storage.ChannelMessage{
		ID:       messageID,
		Channel:  channelName,
		Data:     dataJSON,
		Metadata: metadataJSON,
	}

	workerID := ctx.WorkerID()
	lockTimeoutSec := 300 // 5 minutes

	for _, sub := range waitingSubs {
		// Deliver using Lock-First pattern
		result, err := s.DeliverChannelMessageWithLock(
			ctx.Context(),
			sub.InstanceID,
			channelName,
			message,
			workerID,
			lockTimeoutSec,
		)
		if err != nil {
			slog.Error("error delivering message", "instance_id", sub.InstanceID, "error", err)
			continue
		}

		// For competing mode, stop after first successful delivery
		if result != nil && sub.Mode == storage.ChannelModeCompeting {
			break
		}
	}
}

// PublishOption configures publish behavior.
type PublishOption func(*publishOptions)

type publishOptions struct {
	metadata map[string]any
}

// WithMetadata attaches metadata to the published message.
func WithMetadata(metadata map[string]any) PublishOption {
	return func(o *publishOptions) {
		o.metadata = metadata
	}
}

// SendTo sends a direct message to a specific workflow instance.
// The target instance must be subscribed to the channel.
// Uses dynamic channel names (Edda approach): publishes to "channel:instance_id"
// so only the target instance receives the message.
func SendTo[T any](ctx *WorkflowContext, targetInstanceID, channelName string, data T, opts ...PublishOption) error {
	options := &publishOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("send_to_%s_%s", targetInstanceID, channelName))

	// Check if already sent (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		return nil
	}

	// Serialize data
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize message data: %w", err)
	}

	// Serialize metadata
	var metadataJSON []byte
	if options.metadata != nil {
		metadataJSON, err = json.Marshal(options.metadata)
		if err != nil {
			return fmt.Errorf("failed to serialize message metadata: %w", err)
		}
	}

	// Publish to direct channel (Edda approach: dynamic channel names)
	// This ensures only the target instance can receive the message
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	directChannel := fmt.Sprintf("%s:%s", channelName, targetInstanceID)
	messageID, err := s.PublishToChannel(ctx.Context(), directChannel, dataJSON, metadataJSON)
	if err != nil {
		return fmt.Errorf("failed to send to instance: %w", err)
	}

	// Wake up the target subscriber (Edda-style: delivery + load balancing)
	// If in transaction, defer delivery until commit to ensure consistency
	if s.InTransaction(ctx.Context()) {
		// Capture variables for closure
		wfCtx := ctx
		store := s
		chName := directChannel
		msgID := messageID
		dJSON := dataJSON
		mJSON := metadataJSON

		_ = s.RegisterPostCommitCallback(ctx.Context(), func() error {
			wakeWaitingSubscribers(wfCtx, store, chName, msgID, dJSON, mJSON)
			return nil
		})
	} else {
		// Deliver immediately
		wakeWaitingSubscribers(ctx, s, directChannel, messageID, dataJSON, metadataJSON)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// ========================================
// Group Membership (Erlang pg style)
// ========================================

// JoinGroup adds the workflow to a named group.
// Groups are useful for scenarios where you need to send messages
// to a set of related workflows.
//
// Groups persist across workflow restarts and members are automatically
// removed when the workflow completes.
func JoinGroup(ctx *WorkflowContext, groupName string) error {
	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("join_group_%s", groupName))

	// Check if already joined (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		return nil
	}

	// Join group
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	if err := s.JoinGroup(ctx.Context(), ctx.InstanceID(), groupName); err != nil {
		return fmt.Errorf("failed to join group: %w", err)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// LeaveGroup removes the workflow from a named group.
func LeaveGroup(ctx *WorkflowContext, groupName string) error {
	// Generate activity ID for deterministic replay
	activityID := ctx.GenerateActivityID(fmt.Sprintf("leave_group_%s", groupName))

	// Check if already left (replay)
	if _, ok := ctx.GetCachedResult(activityID); ok {
		return nil
	}

	// Leave group
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}

	if err := s.LeaveGroup(ctx.Context(), ctx.InstanceID(), groupName); err != nil {
		return fmt.Errorf("failed to leave group: %w", err)
	}

	// Record for replay
	ctx.SetCachedResult(activityID, true)

	return nil
}

// GetGroupMembers returns the instance IDs of all workflows in a group.
// This is useful for broadcasting messages to group members.
func GetGroupMembers(ctx context.Context, s storage.Storage, groupName string) ([]string, error) {
	return s.GetGroupMembers(ctx, groupName)
}

// ========================================
// Recur (Erlang-style tail recursion)
// ========================================

// Recur implements Erlang-style tail recursion for workflows.
// It archives the current workflow's history and starts a new instance
// with the provided input, maintaining the same instance ID.
//
// This is useful for long-running workflows that need to periodically
// "reset" their history to prevent unbounded growth.
//
// The workflow will:
// 1. Archive current history to workflow_history_archive
// 2. Clean up all subscriptions (events, timers, channels, groups)
// 3. Mark current instance as "recurred"
// 4. Create a new instance with continued_from set to current instance
// 5. Start executing with the new input
//
// Example:
//
//	workflow := romancy.DefineWorkflow("counter", func(ctx *romancy.WorkflowContext, input CounterInput) (CounterResult, error) {
//	    // Process batch
//	    newCount := input.Count + 1
//	    if newCount < 1000 {
//	        // Continue with new input (tail recursion)
//	        return romancy.Recur(ctx, CounterInput{Count: newCount})
//	    }
//	    return CounterResult{FinalCount: newCount}, nil
//	})
func Recur[T any](ctx *WorkflowContext, input T) (T, error) {
	var zero T

	// Return SuspendSignal that the replay engine will handle
	return zero, NewRecurSuspend(ctx.InstanceID(), input)
}
