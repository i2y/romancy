package romancy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/i2y/romancy/internal/storage"
)

// BlobRefPrefix marks payload references stored in messages/data in place
// of the payload itself: "blob:sha256:<hex>".
const BlobRefPrefix = "blob:sha256:"

// StorePayload stores data in the content-addressed blob store and returns
// its reference ("blob:sha256:<hex>"). Writes are idempotent; identical
// payloads share one blob.
func StorePayload(ctx *WorkflowContext, data []byte) (string, error) {
	s := ctx.Storage()
	if s == nil {
		return "", fmt.Errorf("storage not available")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := s.PutBlob(ctx.Context(), hash, data, ctx.InstanceID()); err != nil {
		return "", fmt.Errorf("failed to store payload: %w", err)
	}
	return BlobRefPrefix + hash, nil
}

// BlobStat returns size/creator metadata for a blob reference (or bare
// hash). Returns nil, nil when absent.
func BlobStat(ctx *WorkflowContext, ref string) (*storage.BlobInfo, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	hash := ref
	if len(hash) > len(BlobRefPrefix) && hash[:len(BlobRefPrefix)] == BlobRefPrefix {
		hash = hash[len(BlobRefPrefix):]
	}
	return s.GetBlobInfo(ctx.Context(), hash)
}

// LoadPayload resolves a blob reference to its data. Accepts a bare hash or
// the full "blob:sha256:<hash>" reference. Returns an error if absent.
//
// NOTE: blobs are readable by anyone who knows the content hash — the
// store is content-addressed, not permissioned. Do not store secrets in
// plaintext; encrypt sensitive payloads before StorePayload.
func LoadPayload(ctx *WorkflowContext, ref string) ([]byte, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	hash := ref
	if len(hash) > len(BlobRefPrefix) && hash[:len(BlobRefPrefix)] == BlobRefPrefix {
		hash = hash[len(BlobRefPrefix):]
	}
	data, err := s.GetBlob(ctx.Context(), hash)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("blob not found: %s", ref)
	}
	return data, nil
}
