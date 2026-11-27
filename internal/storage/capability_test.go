package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestStorage_Capability(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		secret := "sekret-123"
		h := sha256.Sum256([]byte(secret))
		hash := hex.EncodeToString(h[:])

		if sealed, err := s.HasCapabilityGrants(ctx, CapabilityKindChannelPub, "orders"); err != nil || sealed {
			t.Fatalf("grants before grant = %v, %v", sealed, err)
		}
		c := &ActorCapability{
			ID:         "cap-1",
			Kind:       CapabilityKindChannelPub,
			Target:     "orders",
			SecretHash: hash,
			GrantedBy:  "wf-granter",
		}
		if err := s.GrantCapability(ctx, c); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if sealed, err := s.HasCapabilityGrants(ctx, CapabilityKindChannelPub, "orders"); err != nil || !sealed {
			t.Fatalf("grants after grant = %v, %v", sealed, err)
		}

		ok, err := s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", hash)
		if err != nil || !ok {
			t.Fatalf("check valid = %v, %v; want true, nil", ok, err)
		}
		ok, err = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", "deadbeef")
		if err != nil || ok {
			t.Fatalf("wrong hash = %v, %v", ok, err)
		}
		ok, _ = s.CheckCapability(ctx, CapabilityKindChannelSub, "orders", hash)
		if ok {
			t.Fatal("wrong kind accepted")
		}
		ok, _ = s.CheckCapability(ctx, CapabilityKindChannelPub, "other-channel", hash)
		if ok {
			t.Fatal("wrong target accepted")
		}

		// expired grant is rejected; a grant expiring later is accepted
		expSum := sha256.Sum256([]byte("expired-secret"))
		expHash := hex.EncodeToString(expSum[:])
		past := time.Now().Add(-time.Hour)
		if err := s.GrantCapability(ctx, &ActorCapability{
			ID: "cap-2", Kind: CapabilityKindChannelPub, Target: "orders",
			SecretHash: expHash, ExpiresAt: &past,
		}); err != nil {
			t.Fatalf("grant expired: %v", err)
		}
		ok, err = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", expHash)
		if err != nil || ok {
			t.Fatalf("expired grant = %v, %v", ok, err)
		}
		futSum := sha256.Sum256([]byte("future-secret"))
		futHash := hex.EncodeToString(futSum[:])
		future := time.Now().Add(10 * time.Minute)
		if err := s.GrantCapability(ctx, &ActorCapability{
			ID: "cap-3", Kind: CapabilityKindChannelPub, Target: "orders",
			SecretHash: futHash, ExpiresAt: &future,
		}); err != nil {
			t.Fatalf("grant future: %v", err)
		}
		ok, err = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", futHash)
		if err != nil || !ok {
			t.Fatalf("not-yet-expired grant = %v, %v", ok, err)
		}

		if err := s.RevokeCapability(ctx, "cap-1"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		ok, _ = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", hash)
		if ok {
			t.Fatal("revoked capability still valid")
		}
		if err := s.RevokeCapability(ctx, "cap-1"); err != nil {
			t.Fatalf("re-revoke: %v", err)
		}
	})
}
