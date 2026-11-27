package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestStorage_ActorFS(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		p := "/actor/wf-1/doc.txt"

		obj, err := s.FSGet(ctx, p)
		if err != nil || obj != nil {
			t.Fatalf("empty get = %v, %v", obj, err)
		}
		if err := s.FSPut(ctx, p, []byte("v1\x00\xff"), "wf-1", 0); err != nil {
			t.Fatalf("create: %v", err)
		}
		// create over existing → conflict
		if err := s.FSPut(ctx, p, []byte("x"), "wf-1", 0); !errors.Is(err, ErrFSConflict) {
			t.Fatalf("create-existing: %v", err)
		}
		obj, err = s.FSGet(ctx, p)
		if err != nil || obj == nil || obj.Version != 1 || !bytes.Equal(obj.Data, []byte("v1\x00\xff")) || obj.Owner != "wf-1" {
			t.Fatalf("obj = %+v, %v", obj, err)
		}
		// stale version update → conflict
		if err := s.FSPut(ctx, p, []byte("v2"), "wf-1", 9); !errors.Is(err, ErrFSConflict) {
			t.Fatalf("stale update: %v", err)
		}
		if err := s.FSPut(ctx, p, []byte("v2"), "wf-1", 1); err != nil {
			t.Fatalf("update: %v", err)
		}
		obj, _ = s.FSGet(ctx, p)
		if obj == nil || obj.Version != 2 || !bytes.Equal(obj.Data, []byte("v2")) {
			t.Fatalf("obj = %+v", obj)
		}
		// an update that writes identical bytes still bumps the version
		if err := s.FSPut(ctx, p, []byte("v2"), "wf-1", 2); err != nil {
			t.Fatalf("same-content update: %v", err)
		}
		// conditional delete
		if err := s.FSDelete(ctx, p, 1); !errors.Is(err, ErrFSConflict) {
			t.Fatalf("stale delete: %v", err)
		}
		if err := s.FSDelete(ctx, p, 3); err != nil {
			t.Fatalf("delete: %v", err)
		}
		obj, _ = s.FSGet(ctx, p)
		if obj != nil {
			t.Fatal("deleted obj still present")
		}
	})
}

func TestStorage_FSList(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
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
		if none, err := s.FSList(ctx, "/actor/nope/"); err != nil || len(none) != 0 {
			t.Fatalf("nope = %+v, %v", none, err)
		}
		// LIKE metachars in prefix are matched literally
		put("/shared/100%/real", "wf-1")
		put("/shared/100x/decoy", "wf-1")
		put("/shared/a_b/real", "wf-1")
		put("/shared/aXb/decoy", "wf-1")
		lst, err = s.FSList(ctx, "/shared/100%/")
		if err != nil || len(lst) != 1 || lst[0].Path != "/shared/100%/real" {
			t.Fatalf("escaped %% = %+v, %v", lst, err)
		}
		lst, err = s.FSList(ctx, "/shared/a_b/")
		if err != nil || len(lst) != 1 || lst[0].Path != "/shared/a_b/real" {
			t.Fatalf("escaped _ = %+v, %v", lst, err)
		}
	})
}
