package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestSQLiteStorage_Capability(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	secret := "sekret-123"
	h := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(h[:])

	cap := &ActorCapability{
		ID:         "cap-1",
		Kind:       CapabilityKindChannelPub,
		Target:     "orders",
		SecretHash: hash,
	}
	if err := s.GrantCapability(ctx, cap); err != nil {
		t.Fatalf("grant: %v", err)
	}

	ok, err := s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", hash)
	if err != nil || !ok {
		t.Fatalf("check valid = %v, %v; want true, nil", ok, err)
	}
	ok, _ = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", "deadbeef")
	if ok {
		t.Fatal("wrong hash accepted")
	}
	ok, _ = s.CheckCapability(ctx, CapabilityKindChannelSub, "orders", hash)
	if ok {
		t.Fatal("wrong kind accepted")
	}
	ok, _ = s.CheckCapability(ctx, CapabilityKindChannelPub, "other-channel", hash)
	if ok {
		t.Fatal("wrong target accepted")
	}

	// expired grant is rejected
	expSum := sha256.Sum256([]byte("expired-secret"))
	expHash := hex.EncodeToString(expSum[:])
	past := time.Now().Add(-time.Hour)
	if err := s.GrantCapability(ctx, &ActorCapability{
		ID: "cap-2", Kind: CapabilityKindChannelPub, Target: "orders",
		SecretHash: expHash, ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("grant expired: %v", err)
	}
	ok, _ = s.CheckCapability(ctx, CapabilityKindChannelPub, "orders", expHash)
	if ok {
		t.Fatal("expired grant accepted")
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
}
