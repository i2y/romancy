package romancy

import (
	"fmt"
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
