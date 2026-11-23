package storage

import (
	"context"
	"testing"
	"time"
)

// claimOwner reads the instance_id recorded in channel_message_claims for a
// message, keyed by the message's UUID (not its numeric row id).
func claimOwner(t *testing.T, ctx context.Context, s *SQLiteStorage, messageID int64) string {
	t.Helper()
	var owner string
	err := s.db.QueryRowContext(ctx, `
		SELECT c.instance_id FROM channel_message_claims c
		JOIN channel_messages m ON m.message_id = c.message_id
		WHERE m.id = ?
	`, messageID).Scan(&owner)
	if err != nil {
		t.Fatalf("failed to read claim owner: %v", err)
	}
	return owner
}

// backdateClaim ages the claim on a message so the stale-claim path is
// exercised without waiting out ChannelClaimStaleSeconds.
func backdateClaim(t *testing.T, ctx context.Context, s *SQLiteStorage, messageID int64, ageSeconds int) {
	t.Helper()
	_, err := s.db.ExecContext(ctx, `
		UPDATE channel_message_claims SET claimed_at = ?
		WHERE message_id = (SELECT message_id FROM channel_messages WHERE id = ?)
	`, time.Now().UTC().Add(-time.Duration(ageSeconds)*time.Second).Format("2006-01-02 15:04:05"), messageID)
	if err != nil {
		t.Fatalf("failed to backdate claim: %v", err)
	}
}

func pendingIDs(t *testing.T, ctx context.Context, s *SQLiteStorage, instanceID, channel string) map[int64]bool {
	t.Helper()
	msgs, err := s.GetPendingChannelMessagesForInstance(ctx, instanceID, channel)
	if err != nil {
		t.Fatalf("pending query failed: %v", err)
	}
	ids := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		ids[m.ID] = true
	}
	return ids
}

// TestClaimChannelMessage_UUIDKey is the FK regression: claims must be keyed
// by channel_messages.message_id (UUID), never by the numeric row id.
func TestClaimChannelMessage_UUIDKey(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-1")
	if err != nil || !claimed {
		t.Fatalf("claim failed: claimed=%v err=%v", claimed, err)
	}

	// The stored claim must join back through the UUID column; if the numeric
	// id had been inserted this query would find nothing.
	if owner := claimOwner(t, ctx, s, msgID); owner != "inst-1" {
		t.Fatalf("expected claim owner inst-1, got %q", owner)
	}
}

// TestClaimChannelMessage_Competing asserts exactly-once semantics: a fresh
// claim by a live instance hides the message from other competitors but not
// from the owner itself (replay drain).
func TestClaimChannelMessage_Competing(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	for _, inst := range []string{"inst-1", "inst-2"} {
		createClaimInstance(t, ctx, s, inst, "wf")
		if err := s.SubscribeToChannel(ctx, inst, "ch", ChannelModeCompeting); err != nil {
			t.Fatalf("subscribe %s failed: %v", inst, err)
		}
	}

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-1")
	if err != nil || !claimed {
		t.Fatalf("first claim failed: claimed=%v err=%v", claimed, err)
	}

	claimed, err = s.ClaimChannelMessage(ctx, msgID, "inst-2")
	if err != nil {
		t.Fatalf("second claim errored: %v", err)
	}
	if claimed {
		t.Fatal("fresh claim by a live instance must not be stealable")
	}

	if pendingIDs(t, ctx, s, "inst-2", "ch")[msgID] {
		t.Fatal("claimed message must be invisible to other competitors")
	}
	// The owner still sees its own claim so a crash between claim and
	// receive-history recording lets replay drain the message.
	if !pendingIDs(t, ctx, s, "inst-1", "ch")[msgID] {
		t.Fatal("claimed message must remain visible to its owner")
	}
}

// TestClaimChannelMessage_SameInstanceRetry verifies idempotent reclaim after
// a crash between claiming and recording history.
func TestClaimChannelMessage_SameInstanceRetry(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	for i := 0; i < 2; i++ {
		claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-1")
		if err != nil || !claimed {
			t.Fatalf("reclaim %d failed: claimed=%v err=%v", i, claimed, err)
		}
	}
	if owner := claimOwner(t, ctx, s, msgID); owner != "inst-1" {
		t.Fatalf("expected claim owner inst-1, got %q", owner)
	}
}

// TestClaimChannelMessage_StaleTakeover covers the orphan path: an instance
// that claimed a message and crashed before consuming leaves a claim that
// must become stealable once it ages out.
func TestClaimChannelMessage_StaleTakeover(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	for _, inst := range []string{"inst-1", "inst-2"} {
		createClaimInstance(t, ctx, s, inst, "wf")
		if err := s.SubscribeToChannel(ctx, inst, "ch", ChannelModeCompeting); err != nil {
			t.Fatalf("subscribe %s failed: %v", inst, err)
		}
	}

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if claimed, _ := s.ClaimChannelMessage(ctx, msgID, "inst-1"); !claimed {
		t.Fatal("first claim failed")
	}

	// While fresh, the orphan blocks other competitors.
	if pendingIDs(t, ctx, s, "inst-2", "ch")[msgID] {
		t.Fatal("fresh orphan claim must hide the message")
	}

	// Simulate the crash by aging the claim past the staleness threshold.
	backdateClaim(t, ctx, s, msgID, ChannelClaimStaleSeconds+60)

	if !pendingIDs(t, ctx, s, "inst-2", "ch")[msgID] {
		t.Fatal("stale claim must not hide the message")
	}
	claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-2")
	if err != nil || !claimed {
		t.Fatalf("stale takeover failed: claimed=%v err=%v", claimed, err)
	}
	if owner := claimOwner(t, ctx, s, msgID); owner != "inst-2" {
		t.Fatalf("expected claim owner inst-2, got %q", owner)
	}
}

// TestClaimChannelMessage_TerminalOwner: a claim left behind by an instance
// that completed/failed without consuming must not pin the message.
func TestClaimChannelMessage_TerminalOwner(t *testing.T) {
	for _, status := range []WorkflowStatus{StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			s, ctx := newClaimTestStorage(t)
			for _, inst := range []string{"inst-1", "inst-2"} {
				createClaimInstance(t, ctx, s, inst, "wf")
				if err := s.SubscribeToChannel(ctx, inst, "ch", ChannelModeCompeting); err != nil {
					t.Fatalf("subscribe %s failed: %v", inst, err)
				}
			}

			msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
			if err != nil {
				t.Fatalf("publish failed: %v", err)
			}
			if claimed, _ := s.ClaimChannelMessage(ctx, msgID, "inst-1"); !claimed {
				t.Fatal("first claim failed")
			}
			if err := s.UpdateInstanceStatus(ctx, "inst-1", status, ""); err != nil {
				t.Fatalf("status update failed: %v", err)
			}

			if !pendingIDs(t, ctx, s, "inst-2", "ch")[msgID] {
				t.Fatal("claim by terminal owner must not hide the message")
			}
			claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-2")
			if err != nil || !claimed {
				t.Fatalf("terminal-owner takeover failed: claimed=%v err=%v", claimed, err)
			}
		})
	}
}

// TestDeliverChannelMessage_CompetingClaim ensures the delivery path also
// claims in competing mode: a message delivered to one subscriber must not be
// recorded a second time for another competitor.
func TestDeliverChannelMessage_CompetingClaim(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	for _, inst := range []string{"inst-1", "inst-2"} {
		createClaimInstance(t, ctx, s, inst, "wf")
		if err := s.SubscribeToChannel(ctx, inst, "ch", ChannelModeCompeting); err != nil {
			t.Fatalf("subscribe %s failed: %v", inst, err)
		}
	}

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	msg := &ChannelMessage{ID: msgID, Channel: "ch"}

	res, err := s.DeliverChannelMessageWithLock(ctx, "inst-1", "ch", msg, "w1", 300)
	if err != nil {
		t.Fatalf("deliver to inst-1 failed: %v", err)
	}
	if res == nil {
		t.Fatal("expected delivery result for inst-1")
	}
	// Delivery consumed the message: it and its claim are gone, so the
	// competitor path below must rely on history, not a lingering claim.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_messages WHERE id = ?`, msgID).Scan(&n); err != nil {
		t.Fatalf("message count failed: %v", err)
	}
	if n != 0 {
		t.Fatal("delivered competing message must be consumed")
	}

	// Second competitor's delivery attempt must be skipped, not recorded.
	res, err = s.DeliverChannelMessageWithLock(ctx, "inst-2", "ch", msg, "w2", 300)
	if err != nil {
		t.Fatalf("deliver to inst-2 errored: %v", err)
	}
	if res != nil {
		t.Fatal("delivery to a non-owner competitor must be skipped")
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_history
		WHERE instance_id = 'inst-2' AND event_type = 'ChannelMessageReceived'
	`).Scan(&n); err != nil {
		t.Fatalf("history count failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("non-owner competitor must not record receive history, got %d", n)
	}
}

// TestDeliverChannelMessage_CompetingConsumes verifies the consume-delete:
// after delivery, the message (and its claim via cascade) must be gone so a
// subsequent Receive cannot see it again and the table cannot accumulate
// consumed work items.
func TestDeliverChannelMessage_CompetingConsumes(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")
	if err := s.SubscribeToChannel(ctx, "inst-1", "ch", ChannelModeCompeting); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	res, err := s.DeliverChannelMessageWithLock(ctx, "inst-1", "ch", &ChannelMessage{ID: msgID, Channel: "ch"}, "w1", 300)
	if err != nil || res == nil {
		t.Fatalf("deliver failed: res=%v err=%v", res, err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_messages WHERE id = ?`, msgID).Scan(&n); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if n != 0 {
		t.Fatal("consumed competing message must be deleted")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_message_claims`).Scan(&n); err != nil {
		t.Fatalf("claim count failed: %v", err)
	}
	if n != 0 {
		t.Fatal("consumed competing claim must be deleted via cascade")
	}
}

// TestDeliverChannelMessage_BroadcastRetains: broadcast messages must survive
// delivery — other subscribers still need them.
func TestDeliverChannelMessage_BroadcastRetains(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")
	if err := s.SubscribeToChannel(ctx, "inst-1", "ch", ChannelModeBroadcast); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	res, err := s.DeliverChannelMessageWithLock(ctx, "inst-1", "ch", &ChannelMessage{ID: msgID, Channel: "ch"}, "w1", 300)
	if err != nil || res == nil {
		t.Fatalf("deliver failed: res=%v err=%v", res, err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_messages WHERE id = ?`, msgID).Scan(&n); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if n != 1 {
		t.Fatal("broadcast message must be retained for other subscribers")
	}
}
