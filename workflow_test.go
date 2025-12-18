package romancy

import (
	"context"
	"testing"
	"time"
)

// SimpleInput is a test input type.
type SimpleInput struct {
	Name string `json:"name"`
}

// SimpleOutput is a test output type.
type SimpleOutput struct {
	Greeting string `json:"greeting"`
}

// TestWorkflowExecution tests basic workflow execution.
func TestWorkflowExecution(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t)
	defer cleanup()

	// Define a simple activity
	greetActivity := DefineActivity[string, string](
		"greet",
		func(ctx context.Context, name string) (string, error) {
			return "Hello, " + name + "!", nil
		},
	)

	// Define a simple workflow
	workflow := DefineWorkflow[SimpleInput, SimpleOutput](
		"simple_workflow",
		func(ctx *WorkflowContext, input SimpleInput) (SimpleOutput, error) {
			greeting, err := greetActivity.Execute(ctx, input.Name)
			if err != nil {
				return SimpleOutput{}, err
			}
			return SimpleOutput{Greeting: greeting}, nil
		},
	)

	// Register workflow
	RegisterWorkflow(app, workflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start a workflow instance
	instanceID, err := StartWorkflow(ctx, app, workflow, SimpleInput{Name: "World"})
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	if instanceID == "" {
		t.Fatal("expected non-empty instance ID")
	}

	// Wait for workflow to complete
	time.Sleep(500 * time.Millisecond)

	// Get result
	result, err := GetWorkflowResult[SimpleOutput](ctx, app, instanceID)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	if result.Status != "completed" {
		t.Errorf("expected status 'completed', got '%s'", result.Status)
	}

	if result.Output.Greeting != "Hello, World!" {
		t.Errorf("expected greeting 'Hello, World!', got '%s'", result.Output.Greeting)
	}
}

// TestWorkflowWithMultipleActivities tests workflow with multiple activities.
func TestWorkflowWithMultipleActivities(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t)
	defer cleanup()

	// Define activities
	addActivity := DefineActivity[int, int](
		"add",
		func(ctx context.Context, x int) (int, error) {
			return x + 10, nil
		},
	)

	multiplyActivity := DefineActivity[int, int](
		"multiply",
		func(ctx context.Context, x int) (int, error) {
			return x * 2, nil
		},
	)

	// Define workflow
	workflow := DefineWorkflow[int, int](
		"math_workflow",
		func(ctx *WorkflowContext, input int) (int, error) {
			// Add 10
			result, err := addActivity.Execute(ctx, input)
			if err != nil {
				return 0, err
			}
			// Multiply by 2
			result, err = multiplyActivity.Execute(ctx, result)
			if err != nil {
				return 0, err
			}
			return result, nil
		},
	)

	// Register workflow
	RegisterWorkflow(app, workflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start workflow with input 5
	// Expected: (5 + 10) * 2 = 30
	instanceID, err := StartWorkflow(ctx, app, workflow, 5)
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	// Wait for workflow to complete
	time.Sleep(500 * time.Millisecond)

	// Get result
	result, err := GetWorkflowResult[int](ctx, app, instanceID)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	if result.Status != "completed" {
		t.Errorf("expected status 'completed', got '%s'", result.Status)
	}

	if result.Output != 30 {
		t.Errorf("expected output 30, got %d", result.Output)
	}
}

// TestWorkflowWithActivityFailure tests workflow with failing activity.
func TestWorkflowWithActivityFailure(t *testing.T) {
	// Create app with pre-initialized test database
	app, cleanup := createTestApp(t)
	defer cleanup()

	// Define failing activity
	failActivity := DefineActivity[string, string](
		"fail",
		func(ctx context.Context, input string) (string, error) {
			return "", NewTerminalErrorf("intentional failure")
		},
	)

	// Define workflow
	workflow := DefineWorkflow[string, string](
		"failing_workflow",
		func(ctx *WorkflowContext, input string) (string, error) {
			return failActivity.Execute(ctx, input)
		},
	)

	// Register workflow
	RegisterWorkflow(app, workflow)

	// Start the app
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Start workflow
	instanceID, err := StartWorkflow(ctx, app, workflow, "test")
	if err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}

	// Wait for workflow to fail
	time.Sleep(500 * time.Millisecond)

	// Get result
	result, err := GetWorkflowResult[string](ctx, app, instanceID)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	if result.Status != "failed" {
		t.Errorf("expected status 'failed', got '%s'", result.Status)
	}

	if result.Error == nil {
		t.Error("expected error to be set")
	}
}
