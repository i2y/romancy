package mcp

import (
	"context"
	"testing"

	"github.com/i2y/romancy"
)

// ----- Test Workflow -----

type TestInput struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

type TestOutput struct {
	Result string `json:"result"`
}

type TestWorkflow struct{}

func (w *TestWorkflow) Name() string { return "test_workflow" }

func (w *TestWorkflow) Execute(ctx *romancy.WorkflowContext, input TestInput) (TestOutput, error) {
	return TestOutput{
		Result: "Hello, " + input.Name,
	}, nil
}

// ----- Tests -----

func TestNewServer(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
	)

	server := NewServer(app)

	if server.app != app {
		t.Error("Server should reference the provided app")
	}

	if server.mcpServer == nil {
		t.Error("Server should have an MCP server")
	}

	if server.config.name != "romancy-mcp-server" {
		t.Errorf("Expected default name 'romancy-mcp-server', got %s", server.config.name)
	}

	if server.config.version != "1.0.0" {
		t.Errorf("Expected default version '1.0.0', got %s", server.config.version)
	}
}

func TestNewServerWithOptions(t *testing.T) {
	app := romancy.NewApp()

	server := NewServer(app,
		WithServerName("my-service"),
		WithServerVersion("2.0.0"),
		WithTransportMode(TransportHTTP),
	)

	if server.config.name != "my-service" {
		t.Errorf("Expected name 'my-service', got %s", server.config.name)
	}

	if server.config.version != "2.0.0" {
		t.Errorf("Expected version '2.0.0', got %s", server.config.version)
	}

	if server.config.transportMode != TransportHTTP {
		t.Errorf("Expected transport mode HTTP, got %s", server.config.transportMode)
	}
}

func TestServerInitializeAndShutdown(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
	)

	server := NewServer(app)

	ctx := context.Background()

	// Initialize should succeed
	if err := server.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if !server.initialized {
		t.Error("Server should be marked as initialized")
	}

	// Double initialize should be idempotent
	if err := server.Initialize(ctx); err != nil {
		t.Fatalf("Double initialize failed: %v", err)
	}

	// Shutdown should succeed
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	if server.initialized {
		t.Error("Server should not be marked as initialized after shutdown")
	}
}

func TestServerApp(t *testing.T) {
	app := romancy.NewApp()
	server := NewServer(app)

	if server.App() != app {
		t.Error("App() should return the underlying app")
	}
}

func TestServerMCPServer(t *testing.T) {
	app := romancy.NewApp()
	server := NewServer(app)

	if server.MCPServer() == nil {
		t.Error("MCPServer() should return the MCP server")
	}
}

func TestRegisterWorkflow(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
	)

	server := NewServer(app)

	// Register a workflow
	RegisterWorkflow[TestInput, TestOutput](server, &TestWorkflow{})

	// Check that the workflow was stored
	server.workflowsMu.RLock()
	defer server.workflowsMu.RUnlock()

	info, ok := server.workflows["test_workflow"]
	if !ok {
		t.Fatal("Workflow should be registered")
	}

	if info.name != "test_workflow" {
		t.Errorf("Expected workflow name 'test_workflow', got %s", info.name)
	}
}

func TestRegisterWorkflowWithOptions(t *testing.T) {
	app := romancy.NewApp(
		romancy.WithDatabase(":memory:"),
	)

	server := NewServer(app)

	// Register with custom options
	RegisterWorkflow[TestInput, TestOutput](server, &TestWorkflow{},
		WithDescription("Test workflow description"),
		WithNamePrefix("custom_prefix"),
		WithStartDescription("Start the test"),
	)

	server.workflowsMu.RLock()
	defer server.workflowsMu.RUnlock()

	info, ok := server.workflows["test_workflow"]
	if !ok {
		t.Fatal("Workflow should be registered")
	}

	if info.config.description != "Test workflow description" {
		t.Errorf("Expected description 'Test workflow description', got %s", info.config.description)
	}

	if info.config.namePrefix != "custom_prefix" {
		t.Errorf("Expected prefix 'custom_prefix', got %s", info.config.namePrefix)
	}
}

func TestHTTPHandler(t *testing.T) {
	app := romancy.NewApp()
	server := NewServer(app)

	handler := server.Handler()
	if handler == nil {
		t.Error("Handler should return an HTTP handler")
	}
}
