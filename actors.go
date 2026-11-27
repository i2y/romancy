package romancy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

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

// Every primitive below is a recorded step when called from workflow code
// (see runStep): a replay returns the recorded outcome instead of touching
// the registry again.

// RegisterName binds a unique actor name to this workflow instance.
// Registering a name this instance already holds succeeds; a name bound to
// another instance answers an error wrapping ErrActorNameTaken.
func RegisterName(ctx *WorkflowContext, name string) error {
	return runEffectErr(ctx, "romancy.register_name", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		if err := s.RegisterActorName(ctx.Context(), name, ctx.InstanceID(), "go"); err != nil {
			return fmt.Errorf("failed to register actor name %q: %w", name, err)
		}
		return nil
	})
}

// UnregisterName removes this instance's binding of name. Unbinding a name
// another instance holds fails with ErrNotOwner; an unbound name is a no-op.
func UnregisterName(ctx *WorkflowContext, name string) error {
	return runEffectErr(ctx, "romancy.unregister_name", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		if err := s.UnregisterOwnActorName(ctx.Context(), name, ctx.InstanceID()); err != nil {
			return fmt.Errorf("failed to unregister actor name %q: %w", name, err)
		}
		return nil
	})
}

// Whereis resolves an actor name to an instance ID. Returns "" if unbound.
func Whereis(ctx *WorkflowContext, name string) (string, error) {
	return runStep(ctx, "romancy.whereis", func(ctx *WorkflowContext) (string, error) {
		s := ctx.Storage()
		if s == nil {
			return "", fmt.Errorf("storage not available")
		}
		return s.WhereisActor(ctx.Context(), name)
	})
}

// Monitor registers a monitor on targetInstanceID. When the target reaches a
// terminal status, a DOWN message is published to this instance's direct
// channel "{channelName}:{instanceID}" — the same channel SendTo uses, so
// receive it via Receive on channelName with a ModeDirect subscription.
func Monitor(ctx *WorkflowContext, targetInstanceID, channelName string) error {
	return runEffectErr(ctx, "romancy.monitor", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		downChannel := fmt.Sprintf("%s:%s", channelName, ctx.InstanceID())
		if err := s.MonitorActor(ctx.Context(), ctx.InstanceID(), targetInstanceID, downChannel); err != nil {
			return fmt.Errorf("failed to monitor %s: %w", targetInstanceID, err)
		}
		return nil
	})
}

// Unmonitor removes a monitor registration (idempotent).
func Unmonitor(ctx *WorkflowContext, targetInstanceID string) error {
	return runEffectErr(ctx, "romancy.unmonitor", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		return s.UnmonitorActor(ctx.Context(), ctx.InstanceID(), targetInstanceID)
	})
}

// ========================================
// Capabilities
// ========================================

// Capability token format: "cap:{kind}:{target}:{secret}". The plaintext
// secret appears only in the issued token; the database stores its SHA-256.

// hashCapabilitySecret returns the SHA-256 hex of a capability secret.
func hashCapabilitySecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// issuedCapability is the recorded outcome of IssueCapability. The
// capability table only ever holds the secret's hash. Without a capability
// key the token itself is recorded, so it is part of workflow history (as
// delegated tokens are part of a child instance's input, SEMANTICS §3b);
// with a key (WithCapabilityKey) the record is derived: it holds the ID,
// kind and target, and the token is derived from the key again.
type issuedCapability struct {
	Token   string `json:"token,omitempty"`
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Target  string `json:"target,omitempty"`
	Derived bool   `json:"derived,omitempty"`
}

// CapabilityOption configures IssueCapability.
type CapabilityOption func(*capabilityOptions)

type capabilityOptions struct {
	ttl time.Duration
}

// WithCapabilityTTL makes the capability expire ttl after it is issued. An
// expired token no longer validates, but its grant keeps the channel sealed
// (the expiry sweep never removes a channel's last grant): only
// RevokeCapability re-opens a channel.
func WithCapabilityTTL(ttl time.Duration) CapabilityOption {
	return func(o *capabilityOptions) {
		o.ttl = ttl
	}
}

// IssueCapability mints a new capability for (kind, target) and stores only
// its hash. Returns a bearer token "cap:{kind}:{target}:{secret}" and the
// capability ID needed for RevokeCapability — the plaintext secret is not
// recoverable from the capability table afterwards.
func IssueCapability(ctx *WorkflowContext, kind, target string, opts ...CapabilityOption) (token, capabilityID string, err error) {
	var options capabilityOptions
	for _, opt := range opts {
		opt(&options)
	}
	issued, err := runEffect(ctx, "romancy.issue_capability", func(ctx *WorkflowContext) (issuedCapability, error) {
		return issueCapability(ctx, kind, target, options)
	})
	if err != nil {
		return "", "", err
	}
	if issued.Derived {
		key := ctx.capabilityKey()
		if key == nil {
			// Configuration, not an outcome: the run stops and is retried
			// once the key is back.
			return "", "", ctx.abortRun(errors.New("capability key not configured: the token of a derived capability cannot be rebuilt"))
		}
		issued.Token = capabilityToken(issued.Kind, issued.Target, deriveCapabilitySecret(key, issued.ID))
	}
	return issued.Token, issued.ID, nil
}

// capabilityToken renders the bearer token of a capability.
func capabilityToken(kind, target, secret string) string {
	return fmt.Sprintf("cap:%s:%s:%s", kind, target, secret)
}

// deriveCapabilitySecret is the secret of capability id under key:
// lowercase hex of HMAC-SHA256(key, id), as in edda and shikibu.
func deriveCapabilitySecret(key []byte, id string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))
}

// capabilityKey returns the app's capability key, or nil.
func (c *WorkflowContext) capabilityKey() []byte {
	if c.app == nil {
		return nil
	}
	return c.app.config.capabilityKey
}

func issueCapability(ctx *WorkflowContext, kind, target string, options capabilityOptions) (issuedCapability, error) {
	if kind == "" || target == "" {
		return issuedCapability{}, fmt.Errorf("%w: a capability needs a kind and a target", ErrInvalidArgument)
	}
	if options.ttl < 0 {
		return issuedCapability{}, fmt.Errorf("%w: negative capability TTL %v", ErrInvalidArgument, options.ttl)
	}
	s := ctx.Storage()
	if s == nil {
		return issuedCapability{}, fmt.Errorf("storage not available")
	}
	instance, err := s.GetInstance(ctx.Context(), ctx.InstanceID())
	if err != nil {
		return issuedCapability{}, fmt.Errorf("failed to check sandbox state: %w", err)
	}
	if instance == nil {
		return issuedCapability{}, fmt.Errorf("workflow instance %s not found", ctx.InstanceID())
	}
	if instance.Sandboxed {
		return issuedCapability{}, fmt.Errorf("cannot issue capability %q for %q: %w", kind, target, ErrSandboxed)
	}
	capID := fmt.Sprintf("cap-%s", uuid.NewString())
	key := ctx.capabilityKey()
	var secret string
	if key != nil {
		secret = deriveCapabilitySecret(key, capID)
	} else {
		secretBytes := make([]byte, 24)
		if _, err := rand.Read(secretBytes); err != nil {
			return issuedCapability{}, fmt.Errorf("failed to mint capability secret: %w", err)
		}
		secret = hex.EncodeToString(secretBytes)
	}

	capability := &storage.ActorCapability{
		ID:         capID,
		Kind:       kind,
		Target:     target,
		SecretHash: hashCapabilitySecret(secret),
		GrantedBy:  ctx.InstanceID(),
	}
	if options.ttl > 0 {
		expires := time.Now().UTC().Add(options.ttl)
		capability.ExpiresAt = &expires
	}
	if err := s.GrantCapability(ctx.Context(), capability); err != nil {
		return issuedCapability{}, fmt.Errorf("failed to grant capability: %w", err)
	}
	if key != nil {
		return issuedCapability{ID: capID, Kind: kind, Target: target, Derived: true}, nil
	}
	return issuedCapability{Token: capabilityToken(kind, target, secret), ID: capID}, nil
}

// CheckCapability verifies a capability token's secret for (kind, target).
// Accepts either a raw secret or a full "cap:kind:target:secret" token.
func CheckCapability(ctx *WorkflowContext, kind, target, secret string) (bool, error) {
	return runStep(ctx, "romancy.check_capability", func(ctx *WorkflowContext) (bool, error) {
		return checkCapabilityToken(ctx, kind, target, secret)
	})
}

// checkCapabilityToken is CheckCapability without step recording, for the
// kernel's own enforcement (sealed channels, /shared/ writes): those run
// only when the guarded operation itself executes, never on its replay.
func checkCapabilityToken(ctx *WorkflowContext, kind, target, secret string) (bool, error) {
	s := ctx.Storage()
	if s == nil {
		return false, fmt.Errorf("storage not available")
	}
	return s.CheckCapability(ctx.Context(), kind, target, hashCapabilitySecret(tokenSecret(secret)))
}

// tokenSecret returns the secret of a "cap:kind:target:secret" token (the
// trailing segment; targets may contain ':'), or the argument itself when
// it is a raw secret.
func tokenSecret(token string) string {
	if strings.HasPrefix(token, "cap:") {
		parts := strings.Split(token, ":")
		if len(parts) >= 4 {
			return parts[len(parts)-1]
		}
	}
	return token
}

// RevokeCapability removes a capability grant this instance issued, by its
// capability ID (returned by IssueCapability). Revoking another instance's
// grant fails with ErrNotOwner; an unknown ID is a no-op. Revoking the last
// grant of a channel re-opens it.
func RevokeCapability(ctx *WorkflowContext, capabilityID string) error {
	return runEffectErr(ctx, "romancy.revoke_capability", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		if err := s.RevokeOwnCapability(ctx.Context(), capabilityID, ctx.InstanceID()); err != nil {
			return fmt.Errorf("failed to revoke capability %s: %w", capabilityID, err)
		}
		return nil
	})
}
