package mcp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/i2y/romancy"
)

// RegisterWorkflow registers a workflow with the MCP server and auto-generates
// four tools for interacting with it:
//
//  1. {workflow}_start - Start a new workflow instance
//  2. {workflow}_status - Get the current status of an instance
//  3. {workflow}_result - Get the result of a completed instance
//  4. {workflow}_cancel - Cancel a running instance
//
// Example:
//
//	type OrderInput struct {
//	    OrderID string `json:"order_id" jsonschema:"The order ID to process"`
//	    Amount  float64 `json:"amount" jsonschema:"Order amount in dollars"`
//	}
//
//	type OrderResult struct {
//	    Status     string `json:"status" jsonschema:"Order processing status"`
//	    ConfirmNum string `json:"confirmation_number" jsonschema:"Confirmation number"`
//	}
//
//	mcp.RegisterWorkflow[OrderInput, OrderResult](server, &OrderWorkflow{})
//
// This will generate tools:
//   - order_workflow_start(order_id, amount) -> instance_id
//   - order_workflow_status(instance_id) -> status
//   - order_workflow_result(instance_id) -> OrderResult
//   - order_workflow_cancel(instance_id, reason) -> success
func RegisterWorkflow[I, O any](server *Server, workflow romancy.Workflow[I, O], opts ...WorkflowToolOption) {
	config := defaultWorkflowToolConfig()
	for _, opt := range opts {
		opt(config)
	}

	name := workflow.Name()
	prefix := config.namePrefix
	if prefix == "" {
		prefix = name
	}

	// Store workflow info
	server.registerWorkflow(name, workflow, config)

	// Register with Romancy App
	romancy.RegisterWorkflow(server.app, workflow)

	// Register the four MCP tools
	registerStartTool[I, O](server, workflow, prefix, config)
	registerStatusTool(server, prefix, config)
	registerResultTool[O](server, prefix, config)
	registerCancelTool(server, prefix, config)
}

// StartInput is the input type for the start tool.
type StartInput[I any] struct {
	Input I `json:"input" jsonschema:"The workflow input parameters"`
}

// StartOutput is the output type for the start tool.
type StartOutput struct {
	InstanceID string `json:"instance_id" jsonschema:"The unique ID of the started workflow instance"`
}

// registerStartTool registers the {workflow}_start tool.
func registerStartTool[I, O any](server *Server, workflow romancy.Workflow[I, O], prefix string, config *workflowToolConfig) {
	toolName := prefix + "_start"

	desc := config.startDescription
	if config.description != "" {
		desc = fmt.Sprintf("%s - %s", config.description, config.startDescription)
	}

	// Create the start tool handler
	handler := func(ctx context.Context, req *mcp.CallToolRequest, input I) (*mcp.CallToolResult, StartOutput, error) {
		// Start the workflow
		instanceID, err := romancy.StartWorkflow(ctx, server.app, workflow, input)
		if err != nil {
			return nil, StartOutput{}, fmt.Errorf("failed to start workflow: %w", err)
		}

		return nil, StartOutput{InstanceID: instanceID}, nil
	}

	// Register with MCP server using the generic AddTool
	mcp.AddTool(server.mcpServer, &mcp.Tool{
		Name:        toolName,
		Description: desc,
	}, handler)
}

// StatusInput is the input type for the status tool.
type StatusInput struct {
	InstanceID string `json:"instance_id" jsonschema:"The workflow instance ID to check"`
}

// StatusOutput is the output type for the status tool.
type StatusOutput struct {
	InstanceID   string `json:"instance_id" jsonschema:"The workflow instance ID"`
	Status       string `json:"status" jsonschema:"Current status: pending, running, completed, failed, cancelled"`
	WorkflowName string `json:"workflow_name" jsonschema:"The name of the workflow"`
	StartedAt    string `json:"started_at" jsonschema:"When the workflow was started in RFC3339 format"`
	UpdatedAt    string `json:"updated_at" jsonschema:"When the workflow was last updated in RFC3339 format"`
}

// registerStatusTool registers the {workflow}_status tool.
func registerStatusTool(server *Server, prefix string, config *workflowToolConfig) {
	toolName := prefix + "_status"

	desc := config.statusDescription
	if config.description != "" {
		desc = fmt.Sprintf("%s - %s", config.description, config.statusDescription)
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest, input StatusInput) (*mcp.CallToolResult, StatusOutput, error) {
		instance, err := server.app.GetInstance(ctx, input.InstanceID)
		if err != nil {
			return nil, StatusOutput{}, fmt.Errorf("failed to get instance: %w", err)
		}

		return nil, StatusOutput{
			InstanceID:   instance.InstanceID,
			Status:       string(instance.Status),
			WorkflowName: instance.WorkflowName,
			StartedAt:    instance.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
			UpdatedAt:    instance.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}, nil
	}

	mcp.AddTool(server.mcpServer, &mcp.Tool{
		Name:        toolName,
		Description: desc,
	}, handler)
}

// ResultInput is the input type for the result tool.
type ResultInput struct {
	InstanceID string `json:"instance_id" jsonschema:"The workflow instance ID to get results from"`
}

// ResultOutput is the output type for the result tool.
type ResultOutput[O any] struct {
	InstanceID string `json:"instance_id" jsonschema:"The workflow instance ID"`
	Status     string `json:"status" jsonschema:"Current status of the workflow"`
	Output     O      `json:"output,omitempty" jsonschema:"The workflow output if completed"`
	Error      string `json:"error,omitempty" jsonschema:"Error message if failed"`
}

// registerResultTool registers the {workflow}_result tool.
func registerResultTool[O any](server *Server, prefix string, config *workflowToolConfig) {
	toolName := prefix + "_result"

	desc := config.resultDescription
	if config.description != "" {
		desc = fmt.Sprintf("%s - %s", config.description, config.resultDescription)
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest, input ResultInput) (*mcp.CallToolResult, ResultOutput[O], error) {
		result, err := romancy.GetWorkflowResult[O](ctx, server.app, input.InstanceID)
		if err != nil {
			return nil, ResultOutput[O]{}, fmt.Errorf("failed to get result: %w", err)
		}

		output := ResultOutput[O]{
			InstanceID: result.InstanceID,
			Status:     result.Status,
			Output:     result.Output,
		}

		if result.Error != nil {
			output.Error = result.Error.Error()
		}

		return nil, output, nil
	}

	mcp.AddTool(server.mcpServer, &mcp.Tool{
		Name:        toolName,
		Description: desc,
	}, handler)
}

// CancelInput is the input type for the cancel tool.
type CancelInput struct {
	InstanceID string `json:"instance_id" jsonschema:"The workflow instance ID to cancel"`
	Reason     string `json:"reason,omitempty" jsonschema:"Optional reason for cancellation"`
}

// CancelOutput is the output type for the cancel tool.
type CancelOutput struct {
	Success bool   `json:"success" jsonschema:"Whether the cancellation was successful"`
	Message string `json:"message" jsonschema:"Status message"`
}

// registerCancelTool registers the {workflow}_cancel tool.
func registerCancelTool(server *Server, prefix string, config *workflowToolConfig) {
	toolName := prefix + "_cancel"

	desc := config.cancelDescription
	if config.description != "" {
		desc = fmt.Sprintf("%s - %s", config.description, config.cancelDescription)
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest, input CancelInput) (*mcp.CallToolResult, CancelOutput, error) {
		err := romancy.CancelWorkflow(ctx, server.app, input.InstanceID, input.Reason)
		if err != nil {
			return nil, CancelOutput{
				Success: false,
				Message: err.Error(),
			}, nil
		}

		return nil, CancelOutput{
			Success: true,
			Message: "Workflow cancelled successfully",
		}, nil
	}

	mcp.AddTool(server.mcpServer, &mcp.Tool{
		Name:        toolName,
		Description: desc,
	}, handler)
}
