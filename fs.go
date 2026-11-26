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
// it back as expectedVersion; a mismatch returns storage.ErrFSConflict.
//
// Permission model:
//   - "/actor/{id}/..." : only the owning instance may read or write
//     (ownership derived from the path — no capability needed)
//   - "/shared/..."     : read is open; write requires an "fs_write"
//     capability token for the target path or for "/shared/"
//
// FS operations are non-deterministic I/O and MUST run inside an
// activity; calls outside an activity (or during replay) fail with
// ErrFSOutsideActivity.

// ErrFSOutsideActivity is returned when an FS op runs outside an activity body.
var ErrFSOutsideActivity = fmt.Errorf("fs operations must run inside an activity")

// ErrFSForbidden is returned when the caller lacks permission for the path.
var ErrFSForbidden = fmt.Errorf("fs: permission denied")

func fsCheckActivity(ctx *WorkflowContext) error {
	if ctx.IsReplaying() || !ctx.InActivity() {
		return ErrFSOutsideActivity
	}
	return nil
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
		ok, err := CheckCapability(ctx, storage.CapabilityKindFSWrite, path, capabilityToken)
		if err != nil {
			return err
		}
		if !ok {
			// a grant scoped to the whole shared space also suffices
			ok, err = CheckCapability(ctx, storage.CapabilityKindFSWrite, "/shared/", capabilityToken)
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
	if rest, ok := strings.CutPrefix(path, "/actor/"); ok {
		owner := strings.SplitN(rest, "/", 2)[0]
		if owner != ctx.InstanceID() {
			return nil, 0, fmt.Errorf("%w: cannot read another actor's private path", ErrFSForbidden)
		}
	}
	obj, err := s.FSGet(ctx.Context(), path)
	if err != nil || obj == nil {
		return nil, 0, err
	}
	return obj.Data, obj.Version, nil
}

// FSWrite stores data at path with optimistic concurrency:
// expectedVersion 0 creates the file (fails if it exists), >0 updates
// only if the stored version matches. Returns storage.ErrFSConflict on
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
	if rest, ok := strings.CutPrefix(prefix, "/actor/"); ok {
		owner := strings.SplitN(rest, "/", 2)[0]
		if owner != ctx.InstanceID() {
			return nil, fmt.Errorf("%w: cannot list another actor's private space", ErrFSForbidden)
		}
	} else if !strings.HasPrefix(prefix, "/shared") {
		return nil, fmt.Errorf("%w: prefix must be under /actor/{id}/ or /shared/", ErrFSForbidden)
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
