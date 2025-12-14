package llm

import (
	"context"
	"encoding/json"
	"fmt"

	bucephalus "github.com/i2y/bucephalus/llm"
	"github.com/i2y/romancy"
)

// AgentContext holds the state for a durable agent, including dependencies
// and conversation history.
type AgentContext[T any] struct {
	// Deps holds user-defined dependencies (e.g., database connections, config).
	Deps T

	// Messages holds the conversation history.
	Messages []Message
}

// NewAgentContext creates a new agent context with the given dependencies.
func NewAgentContext[T any](deps T) *AgentContext[T] {
	return &AgentContext[T]{
		Deps:     deps,
		Messages: make([]Message, 0),
	}
}

// AddUserMessage appends a user message to the conversation history.
func (c *AgentContext[T]) AddUserMessage(content string) {
	c.Messages = append(c.Messages, UserMessage(content))
}

// AddAssistantMessage appends an assistant message to the conversation history.
func (c *AgentContext[T]) AddAssistantMessage(content string) {
	c.Messages = append(c.Messages, AssistantMessage(content))
}

// AddSystemMessage prepends a system message to the conversation history.
func (c *AgentContext[T]) AddSystemMessage(content string) {
	c.Messages = append([]Message{SystemMessage(content)}, c.Messages...)
}

// DurableAgent provides stateful, multi-turn conversational AI capabilities
// with automatic persistence and replay support.
//
// Example:
//
//	type MyDeps struct {
//	    UserID    string
//	    Documents []string
//	}
//
//	agent := llm.NewDurableAgent[MyDeps]("research_agent",
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-20250514"),
//	).WithBuildPrompt(func(ctx *llm.AgentContext[MyDeps], message string) []llm.Message {
//	    return []llm.Message{
//	        llm.SystemMessage("You are a research assistant with access to: " + strings.Join(ctx.Deps.Documents, ", ")),
//	        // Include conversation history
//	    }
//	})
//
//	// In workflow
//	agentCtx := llm.NewAgentContext(MyDeps{UserID: "user123", Documents: docs})
//	response, err := agent.Chat(wfCtx, agentCtx, "What is quantum computing?")
type DurableAgent[T any] struct {
	name        string
	opts        []Option
	buildPrompt func(ctx *AgentContext[T], message string) []Message
	getTools    func() []Tool
}

// NewDurableAgent creates a new durable agent with the given name and options.
func NewDurableAgent[T any](name string, opts ...Option) *DurableAgent[T] {
	return &DurableAgent[T]{
		name: name,
		opts: opts,
	}
}

// WithBuildPrompt sets the prompt builder function.
// The builder receives the agent context and the user message,
// and should return the full message list to send to the LLM.
func (a *DurableAgent[T]) WithBuildPrompt(fn func(ctx *AgentContext[T], message string) []Message) *DurableAgent[T] {
	a.buildPrompt = fn
	return a
}

// WithGetTools sets the tools provider function.
// The function should return the tools available to the agent.
func (a *DurableAgent[T]) WithGetTools(fn func() []Tool) *DurableAgent[T] {
	a.getTools = fn
	return a
}

// Chat executes a single conversation turn as a durable activity.
// The conversation history in agentCtx is automatically updated.
//
// Example:
//
//	response, err := agent.Chat(wfCtx, agentCtx, "Hello!")
//	// agentCtx.Messages now includes the user message and assistant response
func (a *DurableAgent[T]) Chat(wfCtx *romancy.WorkflowContext, agentCtx *AgentContext[T], message string) (*DurableResponse, error) {
	cfg := a.buildConfig(wfCtx)

	// Generate deterministic activity ID
	activityID := wfCtx.GenerateActivityID(a.name + "_chat")

	// Check cache for replay
	if cached, ok := wfCtx.GetCachedResult(activityID); ok {
		resp, err := cachedToDurableResponse(cached)
		if err != nil {
			return nil, err
		}
		// Update conversation history even on replay
		agentCtx.AddUserMessage(message)
		agentCtx.AddAssistantMessage(resp.Text)
		return resp, nil
	}

	// Build messages
	var messages []Message
	if a.buildPrompt != nil {
		messages = a.buildPrompt(agentCtx, message)
	} else {
		// Default: use existing messages + new user message
		messages = append(messages, agentCtx.Messages...)
		messages = append(messages, UserMessage(message))
	}

	// Add tools if provided
	if a.getTools != nil {
		tools := a.getTools()
		if len(tools) > 0 {
			cfg.tools = tools
		}
	}

	// Execute the LLM call
	resp, err := bucephalus.CallMessages(wfCtx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
		_ = wfCtx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)

	// Record result for replay
	if err := wfCtx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record agent chat result: %w", err)
	}

	wfCtx.RecordActivityID(activityID)

	// Update conversation history
	agentCtx.AddUserMessage(message)
	agentCtx.AddAssistantMessage(result.Text)

	return result, nil
}

// ChatWithToolLoop executes a conversation turn with automatic tool execution.
// The agent will continue calling tools until the model stops requesting them
// or maxIterations is reached.
//
// Example:
//
//	response, err := agent.ChatWithToolLoop(wfCtx, agentCtx, "Search for X",
//	    10, // max iterations
//	    func(ctx context.Context, call llm.ToolCall) (string, error) {
//	        // Execute the tool and return the result
//	        return "Tool result", nil
//	    },
//	)
func (a *DurableAgent[T]) ChatWithToolLoop(
	wfCtx *romancy.WorkflowContext,
	agentCtx *AgentContext[T],
	message string,
	maxIterations int,
	toolExecutor func(ctx context.Context, call ToolCall) (string, error),
) (*DurableResponse, error) {
	// Initial chat
	resp, err := a.Chat(wfCtx, agentCtx, message)
	if err != nil {
		return nil, err
	}

	// Tool calling loop
	iterations := 0
	for resp.HasToolCalls() && iterations < maxIterations {
		iterations++

		// Execute each tool call as a separate activity
		var toolMessages []Message
		for i, call := range resp.ToolCalls {
			toolResult, err := a.executeToolCall(wfCtx, i, call, toolExecutor)
			if err != nil {
				return nil, fmt.Errorf("tool call %s failed: %w", call.Name, err)
			}
			toolMessages = append(toolMessages, ToolMessage(call.ID, toolResult))
		}

		// Continue the conversation with tool results
		resp, err = a.continueWithToolResults(wfCtx, agentCtx, resp, toolMessages)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// executeToolCall executes a single tool call as a durable activity.
func (a *DurableAgent[T]) executeToolCall(
	wfCtx *romancy.WorkflowContext,
	index int,
	call ToolCall,
	executor func(ctx context.Context, call ToolCall) (string, error),
) (string, error) {
	// Generate deterministic activity ID for this tool call
	activityID := wfCtx.GenerateActivityID(fmt.Sprintf("%s_tool_%s_%d", a.name, call.Name, index))

	// Check cache for replay
	if cached, ok := wfCtx.GetCachedResult(activityID); ok {
		if result, ok := cached.(string); ok {
			return result, nil
		}
		// Try to convert from map
		if m, ok := cached.(map[string]any); ok {
			if result, ok := m["result"].(string); ok {
				return result, nil
			}
		}
	}

	// Execute the tool
	result, err := executor(wfCtx.Context(), call)
	if err != nil {
		_ = wfCtx.RecordActivityResult(activityID, nil, err)
		return "", err
	}

	// Record result for replay
	if err := wfCtx.RecordActivityResult(activityID, map[string]any{"result": result}, nil); err != nil {
		return "", fmt.Errorf("failed to record tool call result: %w", err)
	}

	wfCtx.RecordActivityID(activityID)
	return result, nil
}

// continueWithToolResults continues the conversation with tool results.
func (a *DurableAgent[T]) continueWithToolResults(
	wfCtx *romancy.WorkflowContext,
	agentCtx *AgentContext[T],
	lastResp *DurableResponse,
	toolMessages []Message,
) (*DurableResponse, error) {
	cfg := a.buildConfig(wfCtx)

	// Generate deterministic activity ID
	activityID := wfCtx.GenerateActivityID(a.name + "_tool_continue")

	// Check cache for replay
	if cached, ok := wfCtx.GetCachedResult(activityID); ok {
		resp, err := cachedToDurableResponse(cached)
		if err != nil {
			return nil, err
		}
		agentCtx.AddAssistantMessage(resp.Text)
		return resp, nil
	}

	// Build messages: existing history + assistant message with tool calls + tool results
	messages := make([]Message, len(agentCtx.Messages))
	copy(messages, agentCtx.Messages)

	// Add assistant message with tool calls
	toolCalls := make([]bucephalus.ToolCall, len(lastResp.ToolCalls))
	for i, tc := range lastResp.ToolCalls {
		toolCalls[i] = ToolCallToBucephalus(tc)
	}
	messages = append(messages, AssistantMessageWithToolCalls(lastResp.Text, toolCalls))

	// Add tool results
	messages = append(messages, toolMessages...)

	// Add tools if provided
	if a.getTools != nil {
		tools := a.getTools()
		if len(tools) > 0 {
			cfg.tools = tools
		}
	}

	// Execute the LLM call
	resp, err := bucephalus.CallMessages(wfCtx.Context(), messages, cfg.toBucephalusOptions()...)
	if err != nil {
		_ = wfCtx.RecordActivityResult(activityID, nil, err)
		return nil, err
	}

	// Convert to durable response
	result := FromBucephalusResponse(resp, cfg.provider, cfg.model)

	// Record result for replay
	if err := wfCtx.RecordActivityResult(activityID, result, nil); err != nil {
		return nil, fmt.Errorf("failed to record agent continue result: %w", err)
	}

	wfCtx.RecordActivityID(activityID)

	// Update conversation history
	agentCtx.AddAssistantMessage(result.Text)

	return result, nil
}

// buildConfig creates a config by merging app defaults and agent options.
func (a *DurableAgent[T]) buildConfig(ctx *romancy.WorkflowContext) *config {
	cfg := newConfig()

	// Apply app-level defaults first
	if defaults := GetLLMDefaults(ctx); defaults != nil {
		for _, opt := range defaults {
			opt(cfg)
		}
	}

	// Apply agent options
	for _, opt := range a.opts {
		opt(cfg)
	}

	return cfg
}

// MarshalAgentContext serializes an agent context to JSON.
func MarshalAgentContext[T any](ctx *AgentContext[T]) ([]byte, error) {
	return json.Marshal(ctx)
}

// UnmarshalAgentContext deserializes an agent context from JSON.
func UnmarshalAgentContext[T any](data []byte) (*AgentContext[T], error) {
	var ctx AgentContext[T]
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, err
	}
	return &ctx, nil
}
