// Package romancy provides a durable execution framework for Go.
package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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

	// Message data. JSON data decodes into T. Bytes (a message published
	// as a []byte, or bytes from another runtime: SEMANTICS §3) are received
	// with T = []byte, string or any (which holds a []byte).
	Data T `json:"data"`

	// Metadata (optional)
	Metadata map[string]any `json:"metadata,omitempty"`

	// Sender instance ID (if sent via SendTo)
	SenderInstanceID string `json:"sender_instance_id,omitempty"`

	// When the message was created
	CreatedAt time.Time `json:"created_at"`

	// binary tells a message whose data is bytes, which binaryData holds,
	// from one whose data is JSON.
	binary     bool
	binaryData []byte
}

// Subscribe registers the workflow to receive messages from a channel.
// The mode determines how messages are delivered:
// - ModeBroadcast: All subscribers receive every message
// - ModeCompeting: Each message goes to exactly one subscriber
// - ModeDirect: Receives messages sent via SendTo to this instance
//
// Subscriptions persist across workflow restarts and are automatically
// cleaned up when the workflow completes.
//
// Once a channel has subscribers, its mode is locked. Attempting to subscribe
// with a different mode will return a ChannelModeConflictError.
func Subscribe(ctx *WorkflowContext, channelName string, mode ChannelMode) error {
	if err := ctx.abortedErr(); err != nil {
		return err
	}
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
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		if err, isErr := cached.(error); isErr {
			return restoreActivityError(err) // a recorded refusal
		}
		// Still need to record direct subscription on replay
		if mode == ModeDirect {
			ctx.recordDirectSubscription(channelName)
		}
		return nil
	}

	// Another instance's direct channel is its own (SEMANTICS §3). The
	// refusal is recorded, so a replay refuses again.
	if mode != ModeDirect {
		if err := enforceDirectPrivacy(ctx, channelName); err != nil {
			return recordRefusal(ctx, activityID, "subscribe_"+channelName, err)
		}
	}

	// Subscribe to channel
	s := ctx.Storage()
	if s == nil {
		return ctx.abortRun(fmt.Errorf("storage not available"))
	}

	// Check for mode conflict
	existingMode, err := s.GetChannelMode(ctx.Context(), actualChannel)
	if err != nil {
		return ctx.abortRun(fmt.Errorf("failed to check channel mode: %w", err))
	}
	if existingMode != "" && existingMode != actualMode {
		return &ChannelModeConflictError{
			Channel:       channelName,
			ExistingMode:  string(existingMode),
			RequestedMode: string(mode),
		}
	}

	if err := s.SubscribeToChannel(ctx.Context(), ctx.InstanceID(), actualChannel, actualMode); err != nil {
		return ctx.abortRun(fmt.Errorf("failed to subscribe to channel: %w", err))
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

// ErrChannelForbidden is returned when a channel operation lacks the
// required capability. Channels become "sealed" once any capability grant
// of the matching kind exists for them; sealed channels reject callers
// without a valid token.
var ErrChannelForbidden = fmt.Errorf("channel: permission denied")

// enforceChannelCapability implements sealed-channel semantics: while no
// grants of kind exist for the channel it is open to everyone; once the
// first grant is issued the operation requires a valid capability token.
//
// A seal on L also covers every physical name "L:..." — the direct channel
// of instance X for L is "L:X", so publishing to that name is SendTo(X, L)
// and reading it is a direct Receive on L. Without this, naming the physical
// channel would bypass the seal SendTo and Receive enforce on L.
func enforceChannelCapability(ctx *WorkflowContext, kind, channelName, token string) error {
	s := ctx.Storage()
	if s == nil {
		return nil
	}
	return checkChannelSeal(ctx.Context(), s, kind, channelName, token)
}

// checkChannelSeal is enforceChannelCapability for callers outside a
// workflow (the CloudEvents endpoint).
func checkChannelSeal(ctx context.Context, s storage.Storage, kind, channelName, token string) error {
	for _, scope := range channelSealScopes(channelName) {
		sealed, err := s.HasCapabilityGrants(ctx, kind, scope)
		if err != nil {
			return err
		}
		if !sealed {
			continue
		}
		if token == "" {
			return fmt.Errorf("%w: channel %q is sealed for %s", ErrChannelForbidden, scope, kind)
		}
		ok, err := s.CheckCapability(ctx, kind, scope, hashCapabilitySecret(tokenSecret(token)))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: invalid capability for channel %q", ErrChannelForbidden, scope)
		}
	}
	return nil
}

// channelSealScopes returns name followed by each of its prefixes that ends
// before a ':' — the channels whose seal applies to name.
func channelSealScopes(name string) []string {
	scopes := []string{name}
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == ':' {
			scopes = append(scopes, name[:i])
		}
	}
	return scopes
}

// WithPublishCapability presents a channel_pub capability token for a
// sealed channel.
func WithPublishCapability(token string) PublishOption {
	return func(o *publishOptions) {
		o.capabilityToken = token
	}
}

// WithReceiveCapability presents a channel_sub capability token for a
// sealed channel.
func WithReceiveCapability(token string) ReceiveOption {
	return func(o *receiveOptions) {
		o.capabilityToken = token
	}
}

// ReceiveOption configures channel receive behavior.
type ReceiveOption func(*receiveOptions)

type receiveOptions struct {
	timeout         *time.Duration
	capabilityToken string
	// targetedEvents makes the receive also read the instance's own direct
	// channel "{channel}:{instance}", where the CloudEvents targeted at the
	// instance are stored (WaitEvent, SEMANTICS §3c).
	targetedEvents bool
}

// withTargetedEvents makes a receive also take the messages on the
// instance's own direct channel for the channel name: WaitEvent receives the
// events of its type and the events targeted at its instance.
func withTargetedEvents() ReceiveOption {
	return func(o *receiveOptions) {
		o.targetedEvents = true
	}
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
	if err := ctx.abortedErr(); err != nil {
		return nil, err
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
		var record receiveRecord
		recordErr := json.Unmarshal(rawJSON, &record)

		// A record of bytes, in base64, whichever runtime wrote it.
		if recordErr == nil && record.DataType == storage.DataTypeBinary {
			return replayBinaryMessage[T](&record)
		}

		// Try Go format first (ReceivedMessage with channel_name, created_at)
		var msg ReceivedMessage[T]
		if err := json.Unmarshal(rawJSON, &msg); err == nil && msg.ChannelName != "" {
			return &msg, nil
		}

		// Try Python/cross-language format (channel, published_at)
		if recordErr == nil {
			msg := messageFromRecord[T](&record)
			if len(record.Data) > 0 {
				if err := json.Unmarshal(record.Data, &msg.Data); err != nil {
					return nil, fmt.Errorf("failed to deserialize cached message data: %w", err)
				}
			}
			return msg, nil
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
			return nil, restoreActivityError(err)
		}
		if msg, ok := result.(*ReceivedMessage[T]); ok {
			return msg, nil
		}
		return nil, fmt.Errorf("cached result type mismatch for channel receive %s", activityID)
	}

	// Sealed-channel enforcement comes after the cache lookup: replays of a
	// completed receive never re-check, keeping replay deterministic even if
	// the capability is later revoked. A refusal is the receive's outcome and
	// is recorded, so a replay refuses again even if grants changed since.
	if err := enforceChannelCapability(ctx, storage.CapabilityKindChannelSub, channelName, options.capabilityToken); err != nil {
		return nil, recordRefusal(ctx, activityID, "receive_"+channelName, err)
	}
	if !ctx.isDirectSubscription(channelName) {
		if err := enforceDirectPrivacy(ctx, channelName); err != nil {
			return nil, recordRefusal(ctx, activityID, "receive_"+channelName, err)
		}
	}

	// Verify subscription exists
	s := ctx.Storage()
	if s == nil {
		return nil, ctx.abortRun(fmt.Errorf("storage not available"))
	}

	sub, err := s.GetChannelSubscription(ctx.Context(), ctx.InstanceID(), actualChannel)
	if err != nil {
		return nil, ctx.abortRun(fmt.Errorf("failed to read subscription: %w", err))
	}
	if sub == nil {
		return nil, ErrChannelNotSubscribed
	}
	subs := []*storage.ChannelSubscription{sub}
	if options.targetedEvents && !ctx.isDirectSubscription(channelName) {
		mailbox, err := subscribeOwnDirectChannel(ctx, s, channelName)
		if err != nil {
			return nil, err
		}
		subs = append(subs, mailbox)
	}

	// Check for pending messages before suspending (Edda-style), the oldest
	// first when the receive reads two channels.
	sources, err := pendingSources(ctx, s, subs)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		result, err := receivePending[T](ctx, s, src.sub, src.messages, channelName, activityID)
		if err != nil {
			return nil, err
		}
		if result != nil {
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
	}

	// No pending messages - need to wait for message
	// Record the activity ID for tracking
	ctx.RecordActivityID(activityID)

	// Return SuspendSignal that the replay engine will handle
	// Include the activityID so it can be used when recording history on message delivery
	// Use actualChannel so it wakes up on the correct channel (including direct channels)
	sig := NewChannelMessageSuspend(ctx.InstanceID(), actualChannel, activityID, options.timeout)
	for _, extra := range subs[1:] {
		sig.AlsoChannels = append(sig.AlsoChannels, extra.Channel)
	}
	return nil, sig
}

// subscribeOwnDirectChannel returns the instance's subscription to its own
// direct channel for channelName, "{channelName}:{instance}", where the
// CloudEvents targeted at the instance are stored. It subscribes when there
// is none; a direct channel is a mailbox, so the subscription also gets the
// events that arrived before it. An existing subscription is kept as it is.
func subscribeOwnDirectChannel(ctx *WorkflowContext, s storage.Storage, channelName string) (*storage.ChannelSubscription, error) {
	mailbox := fmt.Sprintf("%s:%s", channelName, ctx.InstanceID())
	sub, err := s.GetChannelSubscription(ctx.Context(), ctx.InstanceID(), mailbox)
	if err != nil {
		return nil, ctx.abortRun(fmt.Errorf("failed to read subscription: %w", err))
	}
	if sub != nil {
		return sub, nil
	}
	if err := s.SubscribeToChannel(ctx.Context(), ctx.InstanceID(), mailbox, storage.ChannelModeBroadcast); err != nil {
		return nil, ctx.abortRun(fmt.Errorf("failed to subscribe to channel: %w", err))
	}
	sub, err = s.GetChannelSubscription(ctx.Context(), ctx.InstanceID(), mailbox)
	if err != nil {
		return nil, ctx.abortRun(fmt.Errorf("failed to read subscription: %w", err))
	}
	if sub == nil {
		return nil, ctx.abortRun(fmt.Errorf("subscription to %q not found after subscribing", mailbox))
	}
	return sub, nil
}

// pendingSource is a subscription with the messages pending on it.
type pendingSource struct {
	sub      *storage.ChannelSubscription
	messages []*storage.ChannelMessage
}

// pendingSources returns the subscriptions that have pending messages,
// ordered by their oldest message.
func pendingSources(ctx *WorkflowContext, s storage.Storage, subs []*storage.ChannelSubscription) ([]pendingSource, error) {
	var sources []pendingSource
	for _, sub := range subs {
		messages, err := s.GetPendingChannelMessagesForInstance(ctx.Context(), ctx.InstanceID(), sub.Channel)
		if err != nil {
			return nil, ctx.abortRun(fmt.Errorf("failed to check pending messages: %w", err))
		}
		if len(messages) > 0 {
			sources = append(sources, pendingSource{sub: sub, messages: messages})
		}
	}
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].messages[0].ID < sources[j].messages[0].ID
	})
	return sources, nil
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
	payload, metadataJSON, err := encodeMessage(data, options.metadata)
	if err != nil {
		return err
	}
	// Called from workflow code, a publish is a recorded effect (runEffect):
	// the capability check, the message and the record commit together, and a
	// replay returns the recorded outcome — published or refused — without
	// publishing or checking again, so a seal that changed in between cannot
	// send the replay down another path. Inside an activity body it runs
	// directly; the enclosing activity is the replay unit.
	return runEffectErr(ctx, "publish_"+channelName, func(c *WorkflowContext) error {
		if err := enforceChannelCapability(c, storage.CapabilityKindChannelPub, channelName, options.capabilityToken); err != nil {
			return err
		}
		s := c.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		if err := publishAndWake(c, s, channelName, payload, metadataJSON); err != nil {
			return fmt.Errorf("failed to publish to channel: %w", err)
		}
		return nil
	})
}

// encodeMessage serializes a message's data and metadata. Data of type
// []byte is published as bytes, a binary message that every runtime receives
// as its byte type (SEMANTICS §3); any other data as JSON.
func encodeMessage(data any, metadata map[string]any) (payload messagePayload, metadataJSON []byte, err error) {
	if b, ok := data.([]byte); ok {
		payload = messagePayload{data: b, binary: true}
	} else {
		dataJSON, err := json.Marshal(data)
		if err != nil {
			return messagePayload{}, nil, fmt.Errorf("%w: failed to serialize message data: %w", ErrInvalidArgument, err)
		}
		payload = messagePayload{data: dataJSON}
	}
	if metadata != nil {
		metadataJSON, err = json.Marshal(metadata)
		if err != nil {
			return messagePayload{}, nil, fmt.Errorf("%w: failed to serialize message metadata: %w", ErrInvalidArgument, err)
		}
	}
	return payload, metadataJSON, nil
}

// publishAndWake stores a message on the physical channel and wakes the
// subscribers waiting on it — after commit when ctx is inside a transaction,
// so a rollback delivers nothing.
func publishAndWake(ctx *WorkflowContext, s storage.Storage, channel string, payload messagePayload, metadataJSON []byte) error {
	message, err := payload.publish(ctx.Context(), s, channel, metadataJSON)
	if err != nil {
		return err
	}
	if s.InTransaction(ctx.Context()) {
		_ = s.RegisterPostCommitCallback(ctx.Context(), func() error {
			// The transaction has ended: wake outside it.
			afterCommit := ctx.WithContext(context.WithoutCancel(storage.WithoutTransaction(ctx.Context())))
			wakeWaitingSubscribers(afterCommit, s, message)
			return nil
		})
		return nil
	}
	wakeWaitingSubscribers(ctx, s, message)
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
func wakeWaitingSubscribers(ctx *WorkflowContext, s storage.Storage, message *storage.ChannelMessage) {
	channelName := message.Channel
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

	// Each delivery is its own lock epoch (SEMANTICS §1). The workflow
	// context built by the engine carries no worker id, so it comes from the
	// app; an empty holder would let concurrent deliverers share one "lock".
	workerID := ctx.WorkerID()
	if app := ctx.App(); app != nil {
		workerID = app.config.workerID
	}
	lockTimeoutSec := 300 // 5 minutes

	for _, sub := range waitingSubs {
		// Deliver using Lock-First pattern
		result, err := s.DeliverChannelMessageWithLock(
			ctx.Context(),
			sub.InstanceID,
			channelName,
			message,
			storage.NewLockIdentity(workerID),
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

// receivePending takes the first pending message this subscriber may consume
// and records it as the receive's result. It runs as one fenced transaction
// (see inFencedTransaction): the claim, the cursor, the record and, in
// competing mode, the consume-delete commit together, and only while the run
// holds the instance lock. A claim that went stale meanwhile therefore cannot
// let a second consumer record the same message, and a run that lost its
// lease leaves no claim behind to hide the message from everyone else.
// It answers nil when every pending message belongs to someone else.
func receivePending[T any](
	ctx *WorkflowContext,
	s storage.Storage,
	sub *storage.ChannelSubscription,
	pending []*storage.ChannelMessage,
	channelName, activityID string,
) (*ReceivedMessage[T], error) {
	var received *ReceivedMessage[T]
	err := ctx.inFencedTransaction(func(tx *WorkflowContext) error {
		var msg *storage.ChannelMessage
		if sub.Mode == storage.ChannelModeCompeting {
			// Claim the first claimable message; a fresh claim by another
			// live instance just skips it rather than retrying the scan.
			for _, m := range pending {
				claimed, err := s.ClaimChannelMessage(tx.Context(), m.ID, ctx.InstanceID())
				if err != nil {
					return fmt.Errorf("failed to claim message: %w", err)
				}
				if claimed {
					msg = m
					break
				}
			}
			if msg == nil {
				return errNothingClaimed
			}
		} else {
			msg = pending[0]
			if sub.Mode == storage.ChannelModeBroadcast {
				if err := s.UpdateDeliveryCursor(tx.Context(), ctx.InstanceID(), sub.Channel, msg.ID); err != nil {
					return fmt.Errorf("failed to update cursor: %w", err)
				}
			}
		}

		var data T
		if err := decodeMessageData(msg, &data); err != nil {
			return &messageDecodeError{fmt.Errorf("failed to deserialize message data: %w", err)}
		}
		var metadata map[string]any
		if len(msg.Metadata) > 0 {
			if err := json.Unmarshal(msg.Metadata, &metadata); err != nil {
				return &messageDecodeError{fmt.Errorf("failed to deserialize metadata: %w", err)}
			}
		}
		result := &ReceivedMessage[T]{
			ID:          msg.ID,
			ChannelName: channelName,
			Data:        data,
			Metadata:    metadata,
			CreatedAt:   msg.PublishedAt,
		}
		if msg.IsBinary() {
			result.binary, result.binaryData = true, msg.DataBinary
		}
		// The record holds the message as it is stored, the form a delivery
		// records too: JSON data as it is, bytes in base64.
		record := json.RawMessage(storage.ReceivedMessageRecord(channelName, msg))
		if err := tx.RecordActivityResult(activityID, record, nil); err != nil {
			return fmt.Errorf("failed to record activity result: %w", err)
		}
		// Competing mode consumes the message.
		if sub.Mode == storage.ChannelModeCompeting {
			if err := s.DeleteChannelMessage(tx.Context(), msg.ID); err != nil {
				return fmt.Errorf("failed to delete consumed message: %w", err)
			}
		}
		received = result
		return nil
	})
	if errors.Is(err, errNothingClaimed) {
		return nil, nil
	}
	var decodeErr *messageDecodeError
	if errors.As(err, &decodeErr) {
		// The message does not fit T: nothing was consumed, and a replay
		// meets the same message again.
		return nil, decodeErr.err
	}
	if err != nil {
		return nil, ctx.abortRun(err)
	}
	ctx.SetCachedResult(activityID, received)
	return received, nil
}

// recordRefusal ends a channel operation refused with ErrChannelForbidden:
// the refusal is its recorded outcome, so a replay refuses again whatever
// changed since. Any other error aborts the run (it is no outcome).
func recordRefusal(ctx *WorkflowContext, activityID, name string, err error) error {
	if !errors.Is(err, ErrChannelForbidden) {
		return ctx.abortRun(err)
	}
	refusal := &recordedActivityError{err: err, record: activityFailureRecord(name, err)}
	if recErr := ctx.RecordActivityResult(activityID, nil, refusal); recErr != nil {
		return ctx.abortRun(recErr)
	}
	return err
}

// enforceDirectPrivacy refuses reading a channel named "L:X" where X is the
// ID of another existing instance: that is X's direct channel, the target
// of SendTo, and only X reads it. Sending to it stays open. Instance IDs may
// contain ':', so every split point is checked.
func enforceDirectPrivacy(ctx *WorkflowContext, channel string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	for i := 0; i < len(channel); i++ {
		if channel[i] != ':' {
			continue
		}
		owner := channel[i+1:]
		if owner == "" || owner == ctx.InstanceID() {
			continue
		}
		inst, err := s.GetInstance(ctx.Context(), owner)
		if err != nil {
			return fmt.Errorf("failed to look up the owner of channel %q: %w", channel, err)
		}
		if inst != nil {
			return fmt.Errorf("%w: %q is the direct channel of instance %s", ErrChannelForbidden, channel, owner)
		}
	}
	return nil
}

// errNothingClaimed rolls receivePending back when every pending message is
// claimed by someone else.
var errNothingClaimed = errors.New("no claimable message")

// messageDecodeError rolls receivePending back when the pending message does
// not decode into the receiver's type.
type messageDecodeError struct{ err error }

func (e *messageDecodeError) Error() string { return e.err.Error() }

// PublishOption configures publish behavior.
type PublishOption func(*publishOptions)

type publishOptions struct {
	metadata        map[string]any
	capabilityToken string
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
	payload, metadataJSON, err := encodeMessage(data, options.metadata)
	if err != nil {
		return err
	}
	// A recorded effect from workflow code, as Publish is.
	return runEffectErr(ctx, fmt.Sprintf("send_to_%s_%s", targetInstanceID, channelName), func(c *WorkflowContext) error {
		if err := enforceChannelCapability(c, storage.CapabilityKindChannelPub, channelName, options.capabilityToken); err != nil {
			return err
		}
		s := c.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		// Publish to the direct channel (Edda approach: dynamic channel
		// names), which only the target instance receives from.
		directChannel := fmt.Sprintf("%s:%s", channelName, targetInstanceID)
		if err := publishAndWake(c, s, directChannel, payload, metadataJSON); err != nil {
			return fmt.Errorf("failed to send to instance: %w", err)
		}
		return nil
	})
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
// It archives the current workflow's history and restarts the same
// instance (same instance ID) with the provided input and a fresh history.
//
// This is useful for long-running workflows that need to periodically
// "reset" their history to prevent unbounded growth.
//
// In one transaction that first proves the run still holds the lock, the
// engine:
// 1. Archives the current history to workflow_history_archive
// 2. Cleans up all subscriptions (events, timers, channels) and groups,
// keeping the broadcast delivery cursors: a channel subscribed again
// continues after the last message delivered before the recur
// 3. Drops the registered compensations: a later iteration's failure does
// not undo an earlier iteration
// 4. Stores the new input on the instance
// 5. Marks the instance "recurred"
//
// The app then restarts the recurred instance with the new input. A
// recurred instance counts as alive: monitors get no DOWN for it, and its
// names, capabilities, tasks and files stay its own.
//
// Recur returns its input type; the workflow returns its zero output with
// the error:
//
//	workflow := romancy.DefineWorkflow("counter", func(ctx *romancy.WorkflowContext, input CounterInput) (CounterResult, error) {
//	    // Process batch
//	    newCount := input.Count + 1
//	    if newCount < 1000 {
//	        // Continue with new input (tail recursion)
//	        _, err := romancy.Recur(ctx, CounterInput{Count: newCount})
//	        return CounterResult{}, err
//	    }
//	    return CounterResult{FinalCount: newCount}, nil
//	})
func Recur[T any](ctx *WorkflowContext, input T) (T, error) {
	var zero T
	if err := ctx.abortedErr(); err != nil {
		return zero, err
	}

	// Return SuspendSignal that the replay engine will handle
	return zero, NewRecurSuspend(ctx.InstanceID(), input)
}
