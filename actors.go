package romancy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/i2y/romancy/internal/storage"
)

// Actor-style API: name registry (Erlang register/whereis) and monitors
// (Erlang monitor/'DOWN' messages) layered on the shared kernel.
//
// A DOWN notification is delivered as a channel message
// {"type":"DOWN","actor":"<instance_id>","status":"<terminal_status>"}
// on the watcher's direct channel "{channelName}:{watcherInstanceID}" —
// subscribe with ModeDirect on channelName to receive it, exactly as with
// SendTo. Emission is performed by the leader's periodic sweep, so a
// notification arrives at most one sweep interval after termination.

// RegisterName binds a unique actor name to this workflow instance.
// Returns an error if the name is already registered by another instance.
func RegisterName(ctx *WorkflowContext, name string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	if err := s.RegisterActorName(ctx.Context(), name, ctx.InstanceID(), "go"); err != nil {
		return fmt.Errorf("failed to register actor name %q: %w", name, err)
	}
	return nil
}

// UnregisterName removes an actor name binding (idempotent).
func UnregisterName(ctx *WorkflowContext, name string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	return s.UnregisterActorName(ctx.Context(), name)
}

// Whereis resolves an actor name to an instance ID. Returns "" if unbound.
func Whereis(ctx *WorkflowContext, name string) (string, error) {
	s := ctx.Storage()
	if s == nil {
		return "", fmt.Errorf("storage not available")
	}
	return s.WhereisActor(ctx.Context(), name)
}

// Monitor registers a monitor on targetInstanceID. When the target reaches a
// terminal status, a DOWN message is published to this instance's direct
// channel "{channelName}:{instanceID}" — the same channel SendTo uses, so
// receive it via Receive on channelName with a ModeDirect subscription.
func Monitor(ctx *WorkflowContext, targetInstanceID, channelName string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	downChannel := fmt.Sprintf("%s:%s", channelName, ctx.InstanceID())
	if err := s.MonitorActor(ctx.Context(), ctx.InstanceID(), targetInstanceID, downChannel); err != nil {
		return fmt.Errorf("failed to monitor %s: %w", targetInstanceID, err)
	}
	return nil
}

// Unmonitor removes a monitor registration (idempotent).
func Unmonitor(ctx *WorkflowContext, targetInstanceID string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	return s.UnmonitorActor(ctx.Context(), ctx.InstanceID(), targetInstanceID)
}

// ========================================
// Capabilities
// ========================================

// Capability token format: "cap:{kind}:{target}:{secret}". The plaintext
// secret appears only in the issued token; the database stores its SHA-256.
// Enforcement integration (Phase B: sandboxed spawn) is intentionally not
// wired into channel operations yet — these are the primitives.

// hashCapabilitySecret returns the SHA-256 hex of a capability secret.
func hashCapabilitySecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// IssueCapability mints a new capability for (kind, target) and stores only
// its hash. Returns a bearer token "cap:{kind}:{target}:{secret}" and the
// capability ID needed for RevokeCapability — the plaintext secret is not
// recoverable afterwards.
func IssueCapability(ctx *WorkflowContext, kind, target string) (token, capabilityID string, err error) {
	s := ctx.Storage()
	if s == nil {
		return "", "", fmt.Errorf("storage not available")
	}
	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", fmt.Errorf("failed to mint capability secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)

	capID := fmt.Sprintf("cap-%s", uuid.NewString())
	cap := &storage.ActorCapability{
		ID:         capID,
		Kind:       kind,
		Target:     target,
		SecretHash: hashCapabilitySecret(secret),
		GrantedBy:  ctx.InstanceID(),
	}
	if err := s.GrantCapability(ctx.Context(), cap); err != nil {
		return "", "", fmt.Errorf("failed to grant capability: %w", err)
	}
	return fmt.Sprintf("cap:%s:%s:%s", kind, target, secret), capID, nil
}

// CheckCapability verifies a capability token's secret for (kind, target).
// Accepts either a raw secret or a full "cap:kind:target:secret" token.
func CheckCapability(ctx *WorkflowContext, kind, target, secret string) (bool, error) {
	s := ctx.Storage()
	if s == nil {
		return false, fmt.Errorf("storage not available")
	}
	// Accept full token form: use the trailing secret segment if present.
	if strings.HasPrefix(secret, "cap:") {
		parts := strings.Split(secret, ":")
		if len(parts) >= 4 {
			secret = parts[len(parts)-1]
		}
	}
	return s.CheckCapability(ctx.Context(), kind, target, hashCapabilitySecret(secret))
}

// RevokeCapability removes a capability grant by its capability ID
// (returned by IssueCapability).
func RevokeCapability(ctx *WorkflowContext, capabilityID string) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	return s.RevokeCapability(ctx.Context(), capabilityID)
}
