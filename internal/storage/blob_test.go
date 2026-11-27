package storage

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestStorage_Blob(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		data := []byte("large-payload-bytes-\x00\x01binary\xff")
		if err := s.PutBlob(ctx, "hash-abc", data, "wf-1"); err != nil {
			t.Fatalf("put: %v", err)
		}
		// idempotent re-put
		if err := s.PutBlob(ctx, "hash-abc", data, "wf-1"); err != nil {
			t.Fatalf("re-put: %v", err)
		}
		got, err := s.GetBlob(ctx, "hash-abc")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("get = %v, %v", got, err)
		}
		got, err = s.GetBlob(ctx, "missing")
		if err != nil || got != nil {
			t.Fatalf("missing get = %v, %v; want nil, nil", got, err)
		}
	})
}

func TestStorage_BlobInfo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		if err := s.PutBlob(ctx, "h1", []byte("abcd"), "wf-9"); err != nil {
			t.Fatal(err)
		}
		info, err := s.GetBlobInfo(ctx, "h1")
		if err != nil || info == nil {
			t.Fatalf("info = %v, %v", info, err)
		}
		if info.SizeBytes != 4 || info.CreatedBy != "wf-9" {
			t.Fatalf("info = %+v", info)
		}
		// created_at is reported in the canonical UTC "YYYY-MM-DD HH:MM:SS" form
		ts, err := time.Parse("2006-01-02 15:04:05", info.CreatedAt)
		if err != nil {
			t.Fatalf("created_at %q not canonical: %v", info.CreatedAt, err)
		}
		if d := time.Since(ts); d < -time.Minute || d > time.Hour {
			t.Fatalf("created_at %q is %v away from now (timezone skew?)", info.CreatedAt, d)
		}
		if info, err := s.GetBlobInfo(ctx, "nope"); info != nil || err != nil {
			t.Fatalf("missing info = %v, %v", info, err)
		}
	})
}

func TestStorage_DeleteExpiredCapabilities(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(10 * time.Minute)
		mk := func(id string, exp *time.Time) {
			if err := s.GrantCapability(ctx, &ActorCapability{
				ID: id, Kind: CapabilityKindChannelPub,
				Target: "t-" + id, SecretHash: "h-" + id, ExpiresAt: exp,
			}); err != nil {
				t.Fatal(err)
			}
		}
		mk("c-exp", &past)
		mk("c-ok", &future)
		mk("c-nil", nil)
		// c-exp is the only grant of its channel: it stays, keeping the
		// channel sealed, but no longer validates.
		n, err := s.DeleteExpiredCapabilities(ctx)
		if err != nil || n != 0 {
			t.Fatalf("deleted = %d, %v; want 0", n, err)
		}
		if ok, err := s.CheckCapability(ctx, CapabilityKindChannelPub, "t-c-exp", "h-c-exp"); err != nil || ok {
			t.Fatalf("expired grant validates: %v, %v", ok, err)
		}
		if sealed, err := s.HasCapabilityGrants(ctx, CapabilityKindChannelPub, "t-c-exp"); err != nil || !sealed {
			t.Fatalf("expiry unsealed the channel: %v, %v", sealed, err)
		}
		if ok, err := s.CheckCapability(ctx, CapabilityKindChannelPub, "t-c-ok", "h-c-ok"); err != nil || !ok {
			t.Fatalf("valid grant swept: %v, %v", ok, err)
		}
		if ok, err := s.CheckCapability(ctx, CapabilityKindChannelPub, "t-c-nil", "h-c-nil"); err != nil || !ok {
			t.Fatalf("non-expiring grant swept: %v, %v", ok, err)
		}
	})
}
