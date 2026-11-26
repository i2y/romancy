package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// Supervisor: Erlang-style child supervision as a library workflow.
//
// A supervisor is an ordinary actor (workflow) that starts a fixed set of
// child actors, watches their statuses, and restarts them according to a
// strategy. Being a workflow, it inherits durability for free: if the
// supervisor's worker crashes, the stale-lock sweep resumes it, and its
// restart bookkeeping survives in the Recur-carried state.
//
// Design notes:
//   - Restart means spawning a NEW instance of the same workflow with the
//     same input (the failed instance is terminal). Child instance IDs are
//     deterministic — "{supervisorID}:{childName}:g{generation}" — so a
//     replayed spawn activity is idempotent.
//   - The poll is authoritative: actual instance rows decide who is dead.
//     Monitor/DOWN messages only wake the supervisor early; their content
//     is never trusted beyond "something happened".
//   - 'recurred' counts as ALIVE: a child using Recur on itself is
//     mid-flight, not dead.
//   - Restarting a live child (one_for_all / rest_for_one) cancels the
//     running instance and immediately spawns the replacement. Cancellation
//     is a best-effort kill — the replacement has a distinct generation ID
//     either way.
//   - History stays bounded because each loop iteration ends in Recur.
//
// Supervised children should not themselves use Recur with direct-channel
// subscriptions they cannot re-establish; treat children as plain workers.

// RestartMode controls when a child is restarted after termination.
type RestartMode int

const (
	// RestartPermanent always restarts the child, even on normal completion.
	RestartPermanent RestartMode = iota
	// RestartTransient restarts the child only on abnormal termination
	// (failed or cancelled), not on normal completion.
	RestartTransient
	// RestartTemporary never restarts the child.
	RestartTemporary
)

// Strategy selects which children restart when one child dies.
type Strategy int

const (
	// OneForOne restarts only the dead child.
	OneForOne Strategy = iota
	// OneForAll restarts all non-terminal children when one dies.
	OneForAll
	// RestForOne restarts the dead child and every child started after it.
	RestForOne
)

// ChildSpec describes one supervised child. Input is the workflow input
// passed verbatim on every (re)start; nil means a JSON null input.
type ChildSpec struct {
	Name     string          `json:"name"`
	Workflow string          `json:"workflow"`
	Input    json.RawMessage `json:"input,omitempty"`
	Restart  RestartMode     `json:"restart"`
}

// SuperviseConfig tunes a supervisor. MaxRestarts/WithinSeconds implement
// restart intensity: more than MaxRestarts restarts within WithinSeconds
// fails the supervisor itself (escalation — its own supervisor, if any,
// decides what to do). Zero MaxRestarts means unlimited restarts.
type SuperviseConfig struct {
	Strategy      Strategy `json:"strategy"`
	MaxRestarts   int      `json:"max_restarts"`
	WithinSeconds float64  `json:"within_seconds"`
	// PollSeconds is the fallback wake interval per iteration; DOWN
	// notifications wake the supervisor earlier. Default 30 when zero.
	PollSeconds float64 `json:"poll_seconds,omitempty"`
}

// ChildState is the runtime state of one child, carried across iterations.
type ChildState struct {
	Spec        ChildSpec `json:"spec"`
	InstanceID  string    `json:"instance_id"`
	Generation  int       `json:"generation"`
	Terminal    bool      `json:"terminal,omitempty"`
	FinalStatus string    `json:"final_status,omitempty"`
}

// SupervisorState is the workflow input — and the Recur-carried loop state.
// Start a supervisor with the zero value: the first iteration spawns all
// children.
type SupervisorState struct {
	Children []ChildState `json:"children,omitempty"`
	// Restarts holds unix-nano timestamps of recent restarts, pruned to the
	// intensity window — restart intensity survives supervisor restarts.
	Restarts []int64 `json:"restarts,omitempty"`
}

// SupervisorResult is the supervisor's output when all children have
// reached terminal states without restart (or it is cancelled).
type SupervisorResult struct {
	Children      []ChildState `json:"children"`
	TotalRestarts int          `json:"total_restarts"`
	Outcome       string       `json:"outcome"` // "completed" | "intensity_exceeded"
}

// errIntensityExceeded fails the supervisor when restarts exceed intensity.
var errIntensityExceeded = errors.New("supervisor: restart intensity exceeded")

// supervisorMonitorChannel is the base channel name for DOWN notifications;
// the direct channel is "{base}:{supervisorInstanceID}".
const supervisorMonitorChannel = "supervisor"

// DefineSupervisor builds a workflow that supervises children per specs.
// Register and start it like any other workflow:
//
//	sup := romancy.DefineSupervisor("pipeline-sup", cfg, specs)
//	app.RegisterWorkflow(sup)
//	id, _ := romancy.StartWorkflow(ctx, app, sup, romancy.SupervisorState{})
//
// Children's workflows must be registered on workers sharing the database —
// the supervisor only creates instance rows; the resumable sweep runs them.
func DefineSupervisor(
	name string,
	cfg SuperviseConfig,
	specs []ChildSpec,
) *WorkflowFunc[SupervisorState, SupervisorResult] {
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 30
	}

	syncActivity := DefineActivity[superviseSyncInput, superviseSyncOutput](
		name+"/sync",
		func(ctx context.Context, in superviseSyncInput) (superviseSyncOutput, error) {
			wfCtx := GetWorkflowContext(ctx)
			if wfCtx == nil {
				return superviseSyncOutput{}, fmt.Errorf("sync activity requires workflow context")
			}
			return superviseSync(ctx, wfCtx, cfg, in)
		},
	)

	return DefineWorkflow(name, func(ctx *WorkflowContext, state SupervisorState) (SupervisorResult, error) {
		// First iteration: materialize children from the static specs.
		if len(state.Children) == 0 {
			for _, spec := range specs {
				state.Children = append(state.Children, ChildState{Spec: spec})
			}
		}

		// Authoritative sync FIRST (spawns on the initial iteration):
		// read instance rows, restart per strategy.
		out, err := syncActivity.Execute(ctx, superviseSyncInput{
			SupervisorID: ctx.InstanceID(),
			Children:     state.Children,
		})
		if err != nil {
			return SupervisorResult{}, fmt.Errorf("supervisor sync: %w", err)
		}
		state.Children = out.Children

		// Restart intensity over the sliding window.
		if out.RestartedCount > 0 {
			window := time.Duration(cfg.WithinSeconds * float64(time.Second))
			cutoff := out.Now - int64(window)
			kept := state.Restarts[:0]
			for _, ts := range state.Restarts {
				if ts >= cutoff {
					kept = append(kept, ts)
				}
			}
			for i := 0; i < out.RestartedCount; i++ {
				kept = append(kept, out.Now)
			}
			state.Restarts = kept
			if cfg.MaxRestarts > 0 && len(state.Restarts) > cfg.MaxRestarts {
				return SupervisorResult{
					Children:      state.Children,
					TotalRestarts: len(state.Restarts),
					Outcome:       "intensity_exceeded",
				}, errIntensityExceeded
			}
		}

		if out.AliveCount == 0 {
			return SupervisorResult{
				Children:      state.Children,
				TotalRestarts: len(state.Restarts),
				Outcome:       "completed",
			}, nil
		}

		// Wait for a DOWN notification; the timeout bounds the poll interval.
		// The poll is authoritative — the message content is never trusted.
		if err := Subscribe(ctx, supervisorMonitorChannel, ModeDirect); err != nil {
			return SupervisorResult{}, err
		}
		_, _ = Receive[json.RawMessage](ctx, supervisorMonitorChannel,
			WithReceiveTimeout(time.Duration(cfg.PollSeconds*float64(time.Second))))

		// Tail-recursive loop: archive history, continue with updated state.
		_, err = Recur(ctx, state)
		return SupervisorResult{}, err
	})
}

// superviseSyncInput/Output are the sync activity's wire types.
type superviseSyncInput struct {
	SupervisorID string       `json:"supervisor_id"`
	Children     []ChildState `json:"children"`
}

type superviseSyncOutput struct {
	Children       []ChildState `json:"children"`
	RestartedCount int          `json:"restarted_count"`
	AliveCount     int          `json:"alive_count"`
	Now            int64        `json:"now"`
}

// deadStatus reports whether status is terminal for supervision purposes.
// 'recurred' is deliberately alive — the child is mid-restart of itself.
func deadStatus(status storage.WorkflowStatus) bool {
	switch status {
	case storage.StatusCompleted, storage.StatusFailed, storage.StatusCancelled:
		return true
	}
	return false
}

// needsRestart applies the restart mode to a terminal status.
func needsRestart(mode RestartMode, status storage.WorkflowStatus) bool {
	switch mode {
	case RestartPermanent:
		return true
	case RestartTransient:
		return status == storage.StatusFailed || status == storage.StatusCancelled
	case RestartTemporary:
		return false
	}
	return false
}

// superviseSync performs one authoritative pass over the children inside a
// single transaction: classify dead/alive, apply the restart strategy, and
// spawn replacements.
func superviseSync(
	ctx context.Context,
	wfCtx *WorkflowContext,
	cfg SuperviseConfig,
	in superviseSyncInput,
) (superviseSyncOutput, error) {
	s := wfCtx.Storage()
	now := time.Now()
	out := superviseSyncOutput{Now: now.UnixNano()}

	// 1. Classify children by their actual instance rows. Never-spawned
	// children (initial iteration) are spawn-pending: they are not dead,
	// do not trigger strategy cascades, and do not count as restarts.
	deadIdx := -1 // first dead child needing a restart (spec order)
	restart := make([]bool, len(in.Children))
	statuses := make([]storage.WorkflowStatus, len(in.Children))
	for i := range in.Children {
		c := &in.Children[i]
		if c.Terminal {
			continue
		}
		if c.InstanceID == "" {
			restart[i] = true
			continue
		}
		inst, err := s.GetInstance(ctx, c.InstanceID)
		if err != nil {
			return out, fmt.Errorf("get instance %s: %w", c.InstanceID, err)
		}
		status := storage.StatusFailed // row missing: treat as crashed
		if inst != nil {
			status = inst.Status
		}
		statuses[i] = status
		if !deadStatus(status) {
			continue
		}
		if needsRestart(c.Spec.Restart, status) {
			restart[i] = true
			if deadIdx == -1 {
				deadIdx = i
			}
		} else {
			c.Terminal = true
			c.FinalStatus = string(status)
		}
	}

	// 2. Expand the restart set per strategy.
	if deadIdx >= 0 {
		switch cfg.Strategy {
		case OneForAll:
			for i := range in.Children {
				if !in.Children[i].Terminal {
					restart[i] = true
				}
			}
		case RestForOne:
			for i := deadIdx; i < len(in.Children); i++ {
				if !in.Children[i].Terminal {
					restart[i] = true
				}
			}
		}
	}

	// 3. Restart selected children: cancel if alive, spawn next generation,
	// monitor the replacement.
	downChannel := fmt.Sprintf("%s:%s", supervisorMonitorChannel, in.SupervisorID)
	for i := range in.Children {
		if !restart[i] {
			continue
		}
		c := &in.Children[i]
		if c.InstanceID != "" && !deadStatus(statuses[i]) {
			// Best-effort kill of the live child we are replacing.
			if err := s.CancelInstance(ctx, c.InstanceID, "supervisor restart"); err != nil {
				return out, fmt.Errorf("cancel %s: %w", c.InstanceID, err)
			}
		}

		gen := c.Generation + 1
		newID := fmt.Sprintf("%s:%s:g%d", in.SupervisorID, c.Spec.Name, gen)
		input := c.Spec.Input
		if len(input) == 0 {
			input = json.RawMessage("null")
		}
		inst := &storage.WorkflowInstance{
			InstanceID:       newID,
			WorkflowName:     c.Spec.Workflow,
			Status:           storage.StatusRunning,
			InputData:        input,
			ParentInstanceID: in.SupervisorID,
			StartedAt:        now,
			UpdatedAt:        now,
		}
		if err := s.CreateInstance(ctx, inst); err != nil {
			if !errors.Is(err, storage.ErrInstanceAlreadyExists) {
				return out, fmt.Errorf("spawn %s: %w", newID, err)
			}
		}
		if err := s.MonitorActor(ctx, in.SupervisorID, newID, downChannel); err != nil {
			return out, fmt.Errorf("monitor %s: %w", newID, err)
		}
		if c.InstanceID != "" {
			// Replacing a previously-spawned child — counts toward intensity.
			out.RestartedCount++
		}
		c.InstanceID = newID
		c.Generation = gen
	}

	// 4. Count survivors.
	for i := range in.Children {
		if !in.Children[i].Terminal {
			out.AliveCount++
		}
	}
	out.Children = in.Children
	return out, nil
}
