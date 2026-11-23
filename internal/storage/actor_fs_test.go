package storage

import (
	"bytes"
	"errors"
	"testing"
)

func TestSQLiteStorage_ActorFS(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	p := "/actor/wf-1/doc.txt"

	obj, err := s.FSGet(ctx, p)
	if err != nil || obj != nil {
		t.Fatalf("empty get = %v, %v", obj, err)
	}
	if err := s.FSPut(ctx, p, []byte("v1"), "wf-1", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	// create over existing → conflict
	if err := s.FSPut(ctx, p, []byte("x"), "wf-1", 0); !errors.Is(err, ErrFSConflict) {
		t.Fatalf("create-existing: %v", err)
	}
	obj, _ = s.FSGet(ctx, p)
	if obj == nil || obj.Version != 1 || !bytes.Equal(obj.Data, []byte("v1")) || obj.Owner != "wf-1" {
		t.Fatalf("obj = %+v", obj)
	}
	// stale version update → conflict
	if err := s.FSPut(ctx, p, []byte("v2"), "wf-1", 9); !errors.Is(err, ErrFSConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if err := s.FSPut(ctx, p, []byte("v2"), "wf-1", 1); err != nil {
		t.Fatalf("update: %v", err)
	}
	obj, _ = s.FSGet(ctx, p)
	if obj.Version != 2 || !bytes.Equal(obj.Data, []byte("v2")) {
		t.Fatalf("obj = %+v", obj)
	}
	// conditional delete
	if err := s.FSDelete(ctx, p, 1); !errors.Is(err, ErrFSConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.FSDelete(ctx, p, 2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	obj, _ = s.FSGet(ctx, p)
	if obj != nil {
		t.Fatal("deleted obj still present")
	}
}
