//go:build integration

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// testCompetingClaims runs the competing-claim contract against a real
// database. ph is the positional placeholder ("$1"-style for postgres,
// "?" for mysql).
func testCompetingClaims(t *testing.T, s Storage, db *sql.DB, ph func(int) string) {
	t.Helper()
	ctx := context.Background()
	p := ph

	createInst := func(id string) {
		t.Helper()
		now := time.Now()
		if err := s.CreateInstance(ctx, &WorkflowInstance{
			InstanceID:   id,
			WorkflowName: "claim_wf",
			Status:       StatusRunning,
			InputData:    []byte(`{}`),
			StartedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			t.Fatalf("CreateInstance %s failed: %v", id, err)
		}
	}

	for _, inst := range []string{"inst-a", "inst-b"} {
		createInst(inst)
		if err := s.SubscribeToChannel(ctx, inst, "cc", ChannelModeCompeting); err != nil {
			t.Fatalf("SubscribeToChannel %s failed: %v", inst, err)
		}
	}

	msgID, err := s.PublishToChannel(ctx, "cc", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("PublishToChannel failed: %v", err)
	}

	// UUID-key regression: the claim row must join via message_id (UUID).
	claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-a")
	if err != nil || !claimed {
		t.Fatalf("first claim failed: claimed=%v err=%v", claimed, err)
	}
	var owner string
	err = db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT c.instance_id FROM channel_message_claims c
		JOIN channel_messages m ON m.message_id = c.message_id
		WHERE m.id = %s`, p(1)), msgID).Scan(&owner)
	if err != nil {
		t.Fatalf("claim owner lookup (UUID join) failed: %v", err)
	}
	if owner != "inst-a" {
		t.Fatalf("expected owner inst-a, got %q", owner)
	}

	// Fresh claim blocks the competitor but stays visible to the owner.
	if claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-b"); err != nil || claimed {
		t.Fatalf("competitor claim must fail: claimed=%v err=%v", claimed, err)
	}
	pending := func(inst string) map[int64]bool {
		t.Helper()
		msgs, err := s.GetPendingChannelMessagesForInstance(ctx, inst, "cc")
		if err != nil {
			t.Fatalf("pending %s: %v", inst, err)
		}
		ids := map[int64]bool{}
		for _, m := range msgs {
			ids[m.ID] = true
		}
		return ids
	}
	if pending("inst-b")[msgID] {
		t.Fatal("fresh claim must hide message from competitor")
	}
	if !pending("inst-a")[msgID] {
		t.Fatal("own claim must stay visible for replay drain")
	}

	// Terminal-owner takeover.
	if err := s.UpdateInstanceStatus(ctx, "inst-a", StatusCompleted, ""); err != nil {
		t.Fatalf("UpdateInstanceStatus failed: %v", err)
	}
	if claimed, err := s.ClaimChannelMessage(ctx, msgID, "inst-b"); err != nil || !claimed {
		t.Fatalf("terminal-owner takeover failed: claimed=%v err=%v", claimed, err)
	}
	if !pending("inst-b")[msgID] {
		t.Fatal("message must be pending for new owner")
	}

	// Stale takeover on a second message.
	msgID2, err := s.PublishToChannel(ctx, "cc", []byte(`{"x":2}`), nil)
	if err != nil {
		t.Fatalf("PublishToChannel 2 failed: %v", err)
	}
	if claimed, _ := s.ClaimChannelMessage(ctx, msgID2, "inst-b"); !claimed {
		t.Fatal("second claim failed")
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE channel_message_claims SET claimed_at = %s
		WHERE message_id = (SELECT message_id FROM channel_messages WHERE id = %s)`,
		p(1), p(2)), time.Now().UTC().Add(-(ChannelClaimStaleSeconds+60)*time.Second), msgID2)
	if err != nil {
		t.Fatalf("backdate claim failed: %v", err)
	}
	if pending("inst-a")[msgID2] {
		// inst-a is terminal but pending query has no owner-status gate on itself.
	}
	if claimed, err := s.ClaimChannelMessage(ctx, msgID2, "inst-a"); err != nil || !claimed {
		t.Fatalf("stale takeover failed: claimed=%v err=%v", claimed, err)
	}
}
