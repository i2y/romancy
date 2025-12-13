package romancy_test

import (
	"context"
	"testing"
	"time"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/internal/storage"
)

// Test activity with compensation
var (
	compensationCalled = false
	compensationInput  string
)

func resetCompensationState() {
	compensationCalled = false
	compensationInput = ""
}

var testActivityWithCompensation = romancy.DefineActivity(
	"test_reserve_inventory",
	func(ctx context.Context, itemID string) (map[string]any, error) {
		return map[string]any{"reserved": true, "item_id": itemID}, nil
	},
	romancy.WithCompensation[string, map[string]any](func(ctx context.Context, itemID string) error {
		compensationCalled = true
		compensationInput = itemID
		return nil
	}),
)

func init() {
	// Register the activity for compensation lookup
	romancy.RegisterActivity(testActivityWithCompensation)
}

func TestCancelWorkflowWithCompensation(t *testing.T) {
	resetCompensationState()

	// Create a temporary SQLite database
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
		romancy.WithWorkerID("test-worker"),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Create a workflow instance manually
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-cancel-123",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{"item_id": "ITEM-001"}`),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := app.Storage().CreateInstance(ctx, instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Add a compensation entry
	comp := &storage.CompensationEntry{
		InstanceID:      "test-cancel-123",
		ActivityID:      "test_reserve_inventory:1",
		CompensationFn:  "test_reserve_inventory",
		CompensationArg: []byte(`"ITEM-001"`),
		Order:           1,
		Status:          "pending",
	}

	if err := app.Storage().AddCompensation(ctx, comp); err != nil {
		t.Fatalf("Failed to add compensation: %v", err)
	}

	// Cancel the workflow
	err := romancy.CancelWorkflow(ctx, app, "test-cancel-123", "user request")
	if err != nil {
		t.Fatalf("Failed to cancel workflow: %v", err)
	}

	// Verify compensation was called
	if !compensationCalled {
		t.Error("Expected compensation to be called")
	}
	if compensationInput != "ITEM-001" {
		t.Errorf("Expected compensation input 'ITEM-001', got '%s'", compensationInput)
	}

	// Verify workflow status is cancelled
	result, err := app.GetInstance(ctx, "test-cancel-123")
	if err != nil {
		t.Fatalf("Failed to get instance: %v", err)
	}
	if result.Status != storage.StatusCancelled {
		t.Errorf("Expected status 'cancelled', got '%s'", result.Status)
	}

	// Verify compensation is marked as executed
	compensations, err := app.Storage().GetCompensations(ctx, "test-cancel-123")
	if err != nil {
		t.Fatalf("Failed to get compensations: %v", err)
	}
	if len(compensations) != 1 {
		t.Fatalf("Expected 1 compensation, got %d", len(compensations))
	}
	if compensations[0].Status != "executed" {
		t.Errorf("Expected compensation status 'executed', got '%s'", compensations[0].Status)
	}
}

func TestCancelCompletedWorkflow(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
		romancy.WithWorkerID("test-worker"),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Create a completed workflow instance
	instance := &storage.WorkflowInstance{
		InstanceID:   "completed-123",
		WorkflowName: "test_workflow",
		Status:       storage.StatusCompleted,
		InputData:    []byte(`{}`),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := app.Storage().CreateInstance(ctx, instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Try to cancel - should fail
	err := romancy.CancelWorkflow(ctx, app, "completed-123", "user request")
	if err == nil {
		t.Error("Expected error when canceling completed workflow")
	}
}

func TestCancelIdempotent(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
		romancy.WithWorkerID("test-worker"),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Create an already cancelled workflow instance
	instance := &storage.WorkflowInstance{
		InstanceID:   "cancelled-123",
		WorkflowName: "test_workflow",
		Status:       storage.StatusCancelled,
		InputData:    []byte(`{}`),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := app.Storage().CreateInstance(ctx, instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Cancel again - should succeed (idempotent)
	err := romancy.CancelWorkflow(ctx, app, "cancelled-123", "user request")
	if err != nil {
		t.Errorf("Expected no error when canceling already canceled workflow, got: %v", err)
	}
}

func TestCancelWithMultipleCompensations(t *testing.T) {
	// Track multiple compensation calls
	var compensationOrder []string

	// Create activities with compensations
	activity1 := romancy.DefineActivity(
		"multi_activity_1",
		func(ctx context.Context, input string) (string, error) { return input, nil },
		romancy.WithCompensation[string, string](func(ctx context.Context, input string) error {
			compensationOrder = append(compensationOrder, "activity1")
			return nil
		}),
	)
	romancy.RegisterActivity(activity1)

	activity2 := romancy.DefineActivity(
		"multi_activity_2",
		func(ctx context.Context, input string) (string, error) { return input, nil },
		romancy.WithCompensation[string, string](func(ctx context.Context, input string) error {
			compensationOrder = append(compensationOrder, "activity2")
			return nil
		}),
	)
	romancy.RegisterActivity(activity2)

	activity3 := romancy.DefineActivity(
		"multi_activity_3",
		func(ctx context.Context, input string) (string, error) { return input, nil },
		romancy.WithCompensation[string, string](func(ctx context.Context, input string) error {
			compensationOrder = append(compensationOrder, "activity3")
			return nil
		}),
	)
	romancy.RegisterActivity(activity3)

	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
		romancy.WithWorkerID("test-worker"),
	)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Create a workflow instance
	instance := &storage.WorkflowInstance{
		InstanceID:   "multi-comp-123",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{}`),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := app.Storage().CreateInstance(ctx, instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Add compensations in order (1, 2, 3)
	for i, name := range []string{"multi_activity_1", "multi_activity_2", "multi_activity_3"} {
		comp := &storage.CompensationEntry{
			InstanceID:      "multi-comp-123",
			ActivityID:      name + ":1",
			CompensationFn:  name,
			CompensationArg: []byte(`"test"`),
			Order:           i + 1, // Higher order = execute first (LIFO)
			Status:          "pending",
		}
		if err := app.Storage().AddCompensation(ctx, comp); err != nil {
			t.Fatalf("Failed to add compensation: %v", err)
		}
	}

	// Reset order tracking
	compensationOrder = nil

	// Cancel the workflow
	err := romancy.CancelWorkflow(ctx, app, "multi-comp-123", "user request")
	if err != nil {
		t.Fatalf("Failed to cancel workflow: %v", err)
	}

	// Verify compensations were called in LIFO order (3, 2, 1)
	expected := []string{"activity3", "activity2", "activity1"}
	if len(compensationOrder) != len(expected) {
		t.Fatalf("Expected %d compensations, got %d", len(expected), len(compensationOrder))
	}
	for i, exp := range expected {
		if compensationOrder[i] != exp {
			t.Errorf("Expected compensation %d to be '%s', got '%s'", i, exp, compensationOrder[i])
		}
	}
}
