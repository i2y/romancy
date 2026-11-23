package storage

import (
	"bytes"
	"testing"
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
