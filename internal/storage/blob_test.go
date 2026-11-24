package storage

import (
	"bytes"
	"testing"
	"time"
)

func TestSQLiteStorage_Blob(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	data := []byte("large-payload-bytes-\x00\x01binary")
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
}

func TestSQLiteStorage_BlobInfo(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
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
	if info, err := s.GetBlobInfo(ctx, "nope"); info != nil || err != nil {
		t.Fatalf("missing info = %v, %v", info, err)
	}
}

func TestSQLiteStorage_DeleteExpiredCapabilities(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
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
	n, err := s.DeleteExpiredCapabilities(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deleted = %d, %v", n, err)
	}
	ok, _ := s.CheckCapability(ctx, CapabilityKindChannelPub, "t-c-ok", "h-c-ok")
	if !ok {
		t.Fatal("valid grant swept")
	}
}
