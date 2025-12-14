package llm

import (
	"encoding/json"
	"fmt"

	"github.com/i2y/romancy"

	bucephalus "github.com/i2y/bucephalus/llm"
)

// DurableCall represents a reusable LLM call configuration.
// It's similar to Edda's @durable_call decorator - you define the LLM settings
// once and can execute multiple calls with the same configuration.
//
// Example:
//
//	var summarizer = llm.DefineDurableCall("summarize",
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-5-20250929"),
//	    llm.WithSystemMessage("Summarize the given text concisely"),
//	    llm.WithMaxTokens(500),
//	)
//
//	// In workflow
//	response, err := summarizer.Execute(ctx, "Long text to summarize...")
type DurableCall struct {
	name string
	opts []Option
}

// DefineDurableCall creates a new reusable LLM call definition.
// The name is used to generate deterministic activity IDs for replay.
func DefineDurableCall(name string, opts ...Option) *DurableCall {
	return &DurableCall{
		name: name,
		opts: opts,
	}
}

// Name returns the name of the durable call.
func (d *DurableCall) Name() string {
	return d.name
}

// Execute runs the LLM call with the given prompt.
// Additional options can be provided to override the defaults.
//
// Example:
//
//	response, err := summarizer.Execute(ctx, "Text to summarize",
//	    llm.WithMaxTokens(200), // Override the default
//	)
func (d *DurableCall) Execute(ctx *romancy.WorkflowContext, prompt string, opts ...Option) (*DurableResponse, error) {
	cfg := d.buildConfig(ctx, opts...)

	// Generate deterministic activity ID using the durable call name
	activityID := ctx.GenerateActivityID(d.name)

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToDurableResponse(cached)
	}

	// Execute the LLM call
	resp, err := bucephalus.Call(ctx.Context(), prompt, cfg.toBucephalusOptions()...)
	if err != nil {
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

// ExecuteParse runs the LLM call and parses the response into a struct.
//
// Example:
//
//	type Summary struct {
//	    MainPoints []string `json:"main_points"`
//	    Conclusion string   `json:"conclusion"`
//	}
//
//	summary, err := summarizer.ExecuteParse[Summary](ctx, "Long article text...")
func (d *DurableCall) ExecuteParse(ctx *romancy.WorkflowContext, prompt string, target any, opts ...Option) error {
	cfg := d.buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID(d.name + "_parse")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		dr, err := cachedToDurableResponse(cached)
		if err != nil {
			return err
		}
		if len(dr.Structured) == 0 {
			return fmt.Errorf("no structured data in cached result")
		}
		return json.Unmarshal(dr.Structured, target)
	}

	// Execute the LLM call
	resp, err := bucephalus.Call(ctx.Context(), prompt, cfg.toBucephalusOptions()...)
	if err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return err
	}

	// Parse the response
	if err := json.Unmarshal([]byte(resp.Text()), target); err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return fmt.Errorf("failed to parse LLM response: %w", err)
	}

	// Convert to durable response with parsed data
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)
	if data, err := json.Marshal(target); err == nil {
		result.Structured = data
	}

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return nil
}

// ExecuteMessages runs the LLM call with a full message history.
//
// Example:
//
//	messages := []llm.Message{
//	    llm.UserMessage("Hello!"),
//	    llm.AssistantMessage("Hi there!"),
//	    llm.UserMessage("Can you help me?"),
//	}
//	response, err := chatbot.ExecuteMessages(ctx, messages)
func (d *DurableCall) ExecuteMessages(ctx *romancy.WorkflowContext, messages []Message, opts ...Option) (*DurableResponse, error) {
	cfg := d.buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID(d.name + "_messages")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		return cachedToDurableResponse(cached)
	}

	// Execute the LLM call
	resp, err := bucephalus.CallMessages(ctx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
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

// ExecuteMessagesParse runs the LLM call with messages and parses the response.
func (d *DurableCall) ExecuteMessagesParse(ctx *romancy.WorkflowContext, messages []Message, target any, opts ...Option) error {
	cfg := d.buildConfig(ctx, opts...)

	// Generate deterministic activity ID
	activityID := ctx.GenerateActivityID(d.name + "_messages_parse")

	// Check cache for replay
	if cached, ok := ctx.GetCachedResult(activityID); ok {
		dr, err := cachedToDurableResponse(cached)
		if err != nil {
			return err
		}
		if len(dr.Structured) == 0 {
			return fmt.Errorf("no structured data in cached result")
		}
		return json.Unmarshal(dr.Structured, target)
	}

	// Execute the LLM call
	resp, err := bucephalus.CallMessages(ctx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return err
	}

	// Parse the response
	if err := json.Unmarshal([]byte(resp.Text()), target); err != nil {
		_ = ctx.RecordActivityResult(activityID, nil, err)
		return fmt.Errorf("failed to parse LLM response: %w", err)
	}

	// Convert to durable response with parsed data
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)
	if data, err := json.Marshal(target); err == nil {
		result.Structured = data
	}

	// Record result for replay
	if err := ctx.RecordActivityResult(activityID, result, nil); err != nil {
		return fmt.Errorf("failed to record LLM call result: %w", err)
	}

	ctx.RecordActivityID(activityID)
	return nil
}

// buildConfig creates a config by merging app defaults, durable call options, and per-call options.
func (d *DurableCall) buildConfig(ctx *romancy.WorkflowContext, opts ...Option) *config {
	cfg := newConfig()

	// Apply app-level defaults first
	if defaults := GetLLMDefaults(ctx); defaults != nil {
		for _, opt := range defaults {
			opt(cfg)
		}
	}

	// Apply durable call options
	for _, opt := range d.opts {
		opt(cfg)
	}

	// Apply per-call options (override everything)
	cfg.applyOptions(opts...)

	return cfg
}
