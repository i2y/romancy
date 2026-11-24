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

func TestSQLiteStorage_FSList(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	put := func(p, owner string) {
		if err := s.FSPut(ctx, p, []byte("x"), owner, 0); err != nil {
			t.Fatal(err)
		}
	}
	put("/actor/wf-1/a.txt", "wf-1")
	put("/actor/wf-1/b.txt", "wf-1")
	put("/actor/wf-2/c.txt", "wf-2")
	put("/shared/pub/x.txt", "wf-1")
	put("/shared/pub/y.txt", "wf-1")

	lst, err := s.FSList(ctx, "/actor/wf-1/")
	if err != nil || len(lst) != 2 {
		t.Fatalf("list = %d, %v", len(lst), err)
	}
	if lst[0].Path != "/actor/wf-1/a.txt" || lst[1].Path != "/actor/wf-1/b.txt" {
		t.Fatalf("paths = %v, %v", lst[0].Path, lst[1].Path)
	}
	if lst[0].Data != nil || lst[0].Version != 1 || lst[0].Owner != "wf-1" {
		t.Fatalf("metadata = %+v", lst[0])
	}
	pub, err := s.FSList(ctx, "/shared/")
	if err != nil || len(pub) != 2 {
		t.Fatalf("shared = %d, %v", len(pub), err)
	}
	if none, _ := s.FSList(ctx, "/actor/nope/"); len(none) != 0 {
		t.Fatalf("nope = %+v", none)
	}
	// LIKE metachars in prefix are matched literally
	if err := s.FSPut(ctx, "/shared/100%/real", []byte("x"), "wf-1", 0); err != nil {
		t.Fatal(err)
	}
	lst, err = s.FSList(ctx, "/shared/100%/")
	if err != nil || len(lst) != 1 || lst[0].Path != "/shared/100%/real" {
		t.Fatalf("escaped = %+v, %v", lst, err)
	}
}
