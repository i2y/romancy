package llm

import (
	"context"
	"encoding/json"

	bucephalus "github.com/i2y/bucephalus/llm"
)

// Tool is an interface that represents an LLM tool.
// Re-exported from bucephalus for convenience.
type Tool = bucephalus.Tool

// ToolRegistry manages a collection of tools.
// Re-exported from bucephalus for convenience.
type ToolRegistry = bucephalus.ToolRegistry

// NewToolRegistry creates a new tool registry.
var NewToolRegistry = bucephalus.NewToolRegistry

// NewTool creates a new typed tool with automatic JSON schema generation.
// The input type In should be a struct with json tags for schema generation.
//
// Example:
//
//	type WeatherInput struct {
//	    City string `json:"city" jsonschema:"required,description=City name"`
//	}
//
//	weatherTool, err := llm.NewTool("get_weather", "Get weather for a city",
//	    func(ctx context.Context, in WeatherInput) (string, error) {
//	        return "Sunny, 72°F", nil
//	    },
//	)
func NewTool[In any, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error)) (*bucephalus.TypedTool[In, Out], error) {
	return bucephalus.NewTool(name, description, fn)
}

// MustNewTool creates a new tool and panics on error.
func MustNewTool[In any, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error)) *bucephalus.TypedTool[In, Out] {
	return bucephalus.MustNewTool(name, description, fn)
}

// ExecuteToolCalls executes the given tool calls using the registry
// and returns tool result messages.
//
// This is useful for implementing tool calling loops:
//
//	if resp.HasToolCalls() {
//	    toolMessages, err := llm.ExecuteToolCalls(ctx, resp.ToolCalls, registry)
//	    // Continue conversation with tool results...
//	}
func ExecuteToolCalls(ctx context.Context, calls []ToolCall, registry *ToolRegistry) ([]Message, error) {
	// Convert our ToolCall type to bucephalus ToolCall type
	bucephalusCalls := make([]bucephalus.ToolCall, len(calls))
	for i, call := range calls {
		bucephalusCalls[i] = bucephalus.ToolCall{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: string(call.Arguments),
		}
	}

	return bucephalus.ExecuteToolCalls(ctx, bucephalusCalls, registry)
}

// ToolCallToBucephalus converts a ToolCall to bucephalus.ToolCall.
func ToolCallToBucephalus(call ToolCall) bucephalus.ToolCall {
	return bucephalus.ToolCall{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: string(call.Arguments),
	}
}

// ToolCallFromBucephalus converts a bucephalus.ToolCall to ToolCall.
func ToolCallFromBucephalus(call bucephalus.ToolCall) ToolCall {
	return ToolCall{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: json.RawMessage(call.Arguments),
	}
}
