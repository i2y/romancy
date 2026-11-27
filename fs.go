package romancy

import (
	"fmt"
	"strings"

	"github.com/i2y/romancy/internal/storage"
)

// Shared actor filesystem: versioned files under "/actor/{id}/" (private
// to that actor) and "/shared/" (common space).
//
// Writes are CAS on version — FSRead returns the version, FSWrite passes
// it back as expectedVersion; a mismatch returns ErrFSConflict.
//
// Permission model:
//   - "/actor/{id}/..." : only the owning instance may read or write
//     (ownership derived from the path — no capability needed)
//   - "/shared/..."     : read is open; write requires an "fs_write"
//     capability token for the target path or for "/shared/"
//
// FS operations are non-deterministic I/O and MUST run inside an
// activity; calls outside an executing activity body fail with
// ErrFSOutsideActivity. (A replay never runs activity bodies, so inside
// one the FS is always live.)

// ErrFSOutsideActivity is returned when an FS op runs outside an activity body.
var ErrFSOutsideActivity = fmt.Errorf("fs operations must run inside an activity")

// ErrFSForbidden is returned when the caller lacks permission for the path.
var ErrFSForbidden = fmt.Errorf("fs: permission denied")

func fsCheckActivity(ctx *WorkflowContext) error {
	// An activity body only ever executes for real — a replayed activity
	// returns its recorded result without running the body — so being inside
	// one is the whole condition. (IsReplaying is per run: it stays true for
	// fresh activities that run after the replayed prefix of a resumed run.)
	if !ctx.InActivity() {
		return ErrFSOutsideActivity
	}
	return nil
}

// fsReadScope checks that the caller may read path (or list it, as a
// prefix): /actor/{id}/... only for its owner, /shared/... for everyone.
// The prefixes are matched exactly — storage collations may compare paths
// case-insensitively, so anything outside the two namespaces is rejected
// rather than treated as open.
func fsReadScope(ctx *WorkflowContext, path string) error {
	if rest, ok := strings.CutPrefix(path, "/actor/"); ok {
		if owner, _, _ := strings.Cut(rest, "/"); owner != ctx.InstanceID() {
			return fmt.Errorf("%w: /actor/%s is private to its owner", ErrFSForbidden, owner)
		}
		return nil
	}
	if strings.HasPrefix(path, "/shared/") {
		return nil
	}
	return fmt.Errorf("%w: path must be under /actor/{id}/ or /shared/", ErrFSForbidden)
}

// fsAuthorize returns nil when ctx may write path, evaluating the path
// grammar: "/actor/{id}/..." → id must equal ctx.InstanceID();
// "/shared/..." → capabilityToken must be a valid fs_write token.
func fsAuthorize(ctx *WorkflowContext, path, capabilityToken string) error {
	rest, ok := strings.CutPrefix(path, "/actor/")
	if ok {
		owner := strings.SplitN(rest, "/", 2)[0]
		if owner == ctx.InstanceID() {
			return nil
		}
		return fmt.Errorf("%w: /actor/%s is owned by another instance", ErrFSForbidden, owner)
	}
	if strings.HasPrefix(path, "/shared/") {
		if capabilityToken == "" {
			return fmt.Errorf("%w: /shared/ writes require an fs_write capability", ErrFSForbidden)
		}
		ok, err := checkCapabilityToken(ctx, storage.CapabilityKindFSWrite, path, capabilityToken)
		if err != nil {
			return err
		}
		if !ok {
			// a grant scoped to the whole shared space also suffices
			ok, err = checkCapabilityToken(ctx, storage.CapabilityKindFSWrite, "/shared/", capabilityToken)
			if err != nil {
				return err
			}
		}
		if !ok {
			return fmt.Errorf("%w: invalid fs_write capability for %s", ErrFSForbidden, path)
		}
		return nil
	}
	return fmt.Errorf("%w: path must be under /actor/{id}/ or /shared/", ErrFSForbidden)
}

// FSRead returns the data and version at path. Private actor paths may
// only be read by their owner; /shared/ is open. Returns (nil, 0, nil)
// when absent.
func FSRead(ctx *WorkflowContext, path string) (data []byte, version int64, err error) {
	if err := fsCheckActivity(ctx); err != nil {
		return nil, 0, err
	}
	s := ctx.Storage()
	if s == nil {
		return nil, 0, fmt.Errorf("storage not available")
	}
	if err := fsReadScope(ctx, path); err != nil {
		return nil, 0, err
	}
	obj, err := s.FSGet(ctx.Context(), path)
	if err != nil || obj == nil {
		return nil, 0, err
	}
	return obj.Data, obj.Version, nil
}

// FSWrite stores data at path with optimistic concurrency:
// expectedVersion 0 creates the file (fails if it exists), >0 updates
// only if the stored version matches. Returns ErrFSConflict on
// mismatch.
func FSWrite(ctx *WorkflowContext, path string, data []byte, expectedVersion int64, capabilityToken string) error {
	if err := fsCheckActivity(ctx); err != nil {
		return err
	}
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	if err := fsAuthorize(ctx, path, capabilityToken); err != nil {
		return err
	}
	return s.FSPut(ctx.Context(), path, data, ctx.InstanceID(), expectedVersion)
}

// FSList returns the objects whose path starts with prefix, ordered by
// path. Listing is metadata-only (path, owner, version — no data).
// Permission rules follow FSRead: an actor may only list its own
// /actor/{id}/ space; /shared/ is open.
func FSList(ctx *WorkflowContext, prefix string) ([]*storage.FSObject, error) {
	if err := fsCheckActivity(ctx); err != nil {
		return nil, err
	}
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	// A prefix that ends exactly at a namespace root lists that root: without
	// the slash, "/actor/{id}" would also match "/actor/{id}x/..." — another
	// actor whose id merely starts with ours (sup:w:g1 vs sup:w:g10).
	if prefix == "/actor/"+ctx.InstanceID() || prefix == "/shared" {
		prefix += "/"
	}
	if err := fsReadScope(ctx, prefix); err != nil {
		return nil, err
	}
	return s.FSList(ctx.Context(), prefix)
}

// FSDelete removes path. expectedVersion follows FSWrite semantics
// (0 deletes unconditionally). Permission rules are the same as FSWrite.
func FSDelete(ctx *WorkflowContext, path string, expectedVersion int64, capabilityToken string) error {
	if err := fsCheckActivity(ctx); err != nil {
		return err
	}
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	if err := fsAuthorize(ctx, path, capabilityToken); err != nil {
		return err
	}
	return s.FSDelete(ctx.Context(), path, expectedVersion)
}
