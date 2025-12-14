package llm

import (
	"encoding/json"
	"fmt"

	"github.com/i2y/romancy"

	bucephalus "github.com/i2y/bucephalus/llm"
)

// Message types re-exported from bucephalus for convenience.
type Message = bucephalus.Message

// Message constructors re-exported from bucephalus.
var (
	// SystemMessage creates a system message.
	SystemMessage = bucephalus.SystemMessage

	// UserMessage creates a user message.
	UserMessage = bucephalus.UserMessage

	// AssistantMessage creates an assistant message.
	AssistantMessage = bucephalus.AssistantMessage

	// ToolMessage creates a tool result message.
	ToolMessage = bucephalus.ToolMessage

	// AssistantMessageWithToolCalls creates an assistant message with tool calls.
	AssistantMessageWithToolCalls = bucephalus.AssistantMessageWithToolCalls
)

// Role constants re-exported from bucephalus.
const (
	RoleSystem    = bucephalus.RoleSystem
	RoleUser      = bucephalus.RoleUser
	RoleAssistant = bucephalus.RoleAssistant
	RoleTool      = bucephalus.RoleTool
)

// Call makes an LLM call and returns the response.
// The call is automatically wrapped as a durable activity, so replay will
// return the cached result without re-invoking the LLM API.
//
// Example:
//
//	response, err := llm.Call(ctx, "What is the capital of France?",
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-5-20250929"),
//	)
//	if err != nil {
//	    return err
//	}
//	fmt.Println(response.Text)
func Call(ctx *romancy.WorkflowContext, prompt string, opts ...Option) (*DurableResponse, error) {
	cfg := buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID("llm_call")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToDurableResponse(cached)
	}

	// Execute the LLM call
	resp, err := bucephalus.Call(ctx.Context(), prompt, cfg.toBucephalusOptions()...)
	if err != nil {
		// Record error for replay
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return result, nil
}

// CallParse makes an LLM call and parses the response into a struct.
// The call is automatically wrapped as a durable activity.
//
// Example:
//
//	type BookRecommendation struct {
//	    Title  string `json:"title"`
//	    Author string `json:"author"`
//	    Reason string `json:"reason"`
//	}
//
//	book, err := llm.CallParse[BookRecommendation](ctx,
//	    "Recommend a science fiction book",
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-5-20250929"),
//	)
func CallParse[T any](ctx *romancy.WorkflowContext, prompt string, opts ...Option) (*T, error) {
	cfg := buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID("llm_call_parse")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToParsedResult[T](cached)
	}

	// Execute the LLM call with parsing
	resp, err := bucephalus.CallParse[T](ctx.Context(), prompt, cfg.toBucephalusOptions()...)
	if err != nil {
		// Record error for replay
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Get the parsed result
	parsed, err := resp.Parsed()
	if err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response with parsed data
	result := FromBucephalusResponseWithParsed(resp, cfg.provider, cfg.model, parsed)

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return &parsed, nil
}

// CallMessages makes an LLM call with a full message history.
// The call is automatically wrapped as a durable activity.
//
// Example:
//
//	messages := []llm.Message{
//	    llm.SystemMessage("You are a helpful assistant"),
//	    llm.UserMessage("What is Go?"),
//	    llm.AssistantMessage("Go is a programming language..."),
//	    llm.UserMessage("Tell me more about its concurrency model"),
//	}
//
//	response, err := llm.CallMessages(ctx, messages,
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-5-20250929"),
//	)
func CallMessages(ctx *romancy.WorkflowContext, messages []Message, opts ...Option) (*DurableResponse, error) {
	cfg := buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID("llm_call_messages")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToDurableResponse(cached)
	}

	// Execute the LLM call
	resp, err := bucephalus.CallMessages(ctx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
		// Record error for replay
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return result, nil
}

// CallMessagesParse makes an LLM call with messages and parses the response.
// The call is automatically wrapped as a durable activity.
func CallMessagesParse[T any](ctx *romancy.WorkflowContext, messages []Message, opts ...Option) (*T, error) {
	cfg := buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID("llm_call_messages_parse")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToParsedResult[T](cached)
	}

	// Execute the LLM call with parsing
	resp, err := bucephalus.CallMessagesParse[T](ctx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
		// Record error for replay
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Get the parsed result
	parsed, err := resp.Parsed()
	if err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response with parsed data
	result := FromBucephalusResponseWithParsed(resp, cfg.provider, cfg.model, parsed)

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return &parsed, nil
}

// buildConfig creates a config by merging app defaults with call options.
func buildConfig(ctx *romancy.WorkflowContext, opts ...Option) *config {
	cfg := newConfig()

	// Apply app-level defaults first (if available)
	if defaults := GetLLMDefaults(ctx); defaults != nil {
		for _, opt := range defaults {
			opt(cfg)
		}
	}

	// Apply per-call options (override defaults)
	cfg.applyOptions(opts...)

	return cfg
}

// cachedToDurableResponse converts a cached result to DurableResponse.
func cachedToDurableResponse(cached any) (*DurableResponse, error) {
	switch v := cached.(type) {
	case *DurableResponse:
		return v, nil
	case DurableResponse:
		return &v, nil
	case map[string]any:
		// JSON deserialization case
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal cached result: %w", err)
		}
		var result DurableResponse
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cached result: %w", err)
		}
		return &result, nil
	default:
		return nil, fmt.Errorf("unexpected cached result type: %T", cached)
	}
}

// cachedToParsedResult converts a cached result to a parsed type.
func cachedToParsedResult[T any](cached any) (*T, error) {
	dr, err := cachedToDurableResponse(cached)
	if err != nil {
		return nil, err
	}

	if len(dr.Structured) == 0 {
		return nil, fmt.Errorf("no structured data in cached result")
	}

	var result T
	if err := json.Unmarshal(dr.Structured, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal structured data: %w", err)
	}

	return &result, nil
}

// GetLLMDefaults retrieves LLM defaults from the workflow context.
// Returns nil if no defaults are set.
func GetLLMDefaults(ctx *romancy.WorkflowContext) []Option {
	// Get defaults from the App via WorkflowContext
	app := ctx.App()
	return getAppDefaults(app)
}
