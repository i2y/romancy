---
title: "LLM Integration"
weight: 3
---

Romancy provides durable LLM calls via the [bucephalus](https://github.com/i2y/bucephalus) library. All LLM calls are automatically cached as activities, enabling deterministic replay without re-invoking the LLM API.

## Overview

The `llm` package provides:

- **Automatic caching**: All LLM calls are recorded as activities for replay
- **Cost savings**: Replay returns cached results without API calls
- **Multiple call styles**: Ad-hoc calls, structured output, multi-turn conversations
- **Reusable definitions**: Define LLM configurations once, use everywhere
- **Stateful agents**: Multi-turn conversational agents with tool support

## Quick Start

### 1. Import the LLM Package

```go
import (
	"github.com/i2y/romancy"
	"github.com/i2y/romancy/llm"

	// Import the provider you want to use
	_ "github.com/i2y/bucephalus/anthropic"
	// _ "github.com/i2y/bucephalus/openai"
	// _ "github.com/i2y/bucephalus/gemini"
)
```

### 2. Set App Defaults (Optional)

```go
app := romancy.NewApp(romancy.WithDatabase("workflow.db"))

// Set LLM defaults for all workflows in this app
llm.SetAppDefaults(app,
	llm.WithProvider("anthropic"),
	llm.WithModel("claude-sonnet-4-5-20250929"),
	llm.WithMaxTokens(1024),
)
```

### 3. Make LLM Calls in Workflows

```go
var summarizeWorkflow = romancy.DefineWorkflow("summarize",
	func(ctx *romancy.WorkflowContext, text string) (string, error) {
		// LLM call is automatically cached for replay
		response, err := llm.Call(ctx, text,
			llm.WithSystemMessage("Summarize the following text concisely."),
		)
		if err != nil {
			return "", err
		}
		return response.Text, nil
	},
)
```

## API Reference

### Ad-hoc Calls

#### llm.Call

Makes an LLM call and returns the response text.

```go
response, err := llm.Call(ctx, "What is Go?",
	llm.WithSystemMessage("You are a helpful assistant"),
	llm.WithMaxTokens(500),
)
fmt.Println(response.Text)
```

#### llm.CallParse

Parses the response into a struct.

```go
type BookRecommendation struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	Reason string `json:"reason"`
}

book, err := llm.CallParse[BookRecommendation](ctx,
	"Recommend a science fiction book. Respond with JSON.",
)
fmt.Printf("Title: %s by %s\n", book.Title, book.Author)
```

#### llm.CallMessages

Makes an LLM call with full message history.

```go
messages := []llm.Message{
	llm.SystemMessage("You are a helpful assistant."),
	llm.UserMessage("What is Go?"),
	llm.AssistantMessage("Go is a programming language..."),
	llm.UserMessage("Tell me more about its concurrency model."),
}

response, err := llm.CallMessages(ctx, messages)
```

#### llm.CallMessagesParse

Parses response from a message-based call.

```go
result, err := llm.CallMessagesParse[MyStruct](ctx, messages)
```

### Reusable Definitions

Define LLM configurations once and reuse them.

```go
// Define a reusable LLM call
var summarizer = llm.DefineDurableCall("summarize",
	llm.WithSystemMessage("You are a helpful assistant that summarizes text."),
	llm.WithMaxTokens(500),
)

// Use in workflows
var workflow = romancy.DefineWorkflow("my_workflow",
	func(ctx *romancy.WorkflowContext, text string) (string, error) {
		response, err := summarizer.Execute(ctx, text)
		if err != nil {
			return "", err
		}
		return response.Text, nil
	},
)
```

**DurableCall methods:**

| Method | Description |
|--------|-------------|
| `Execute(ctx, prompt)` | Execute with a prompt |
| `ExecuteParse[T](ctx, prompt)` | Execute and parse response |
| `ExecuteMessages(ctx, messages)` | Execute with message history |

### Durable Agent

For stateful multi-turn conversations with tool support.

```go
// Define dependencies for the agent
type ResearchDeps struct {
	APIClient *http.Client
	MaxResults int
}

// Create a durable agent
agent := llm.NewDurableAgent[ResearchDeps]("research_agent",
	llm.WithProvider("anthropic"),
	llm.WithModel("claude-sonnet-4-5-20250929"),
).WithBuildPrompt(func(agentCtx *llm.AgentContext[ResearchDeps], message string) []llm.Message {
	return append(agentCtx.Messages,
		llm.SystemMessage("You are a research assistant."),
		llm.UserMessage(message),
	)
})

// Use in a workflow
var researchWorkflow = romancy.DefineWorkflow("research",
	func(ctx *romancy.WorkflowContext, query string) (string, error) {
		agentCtx := llm.NewAgentContext(ResearchDeps{
			APIClient: http.DefaultClient,
			MaxResults: 10,
		})

		// First turn
		response1, err := agent.Chat(ctx, agentCtx, query)
		if err != nil {
			return "", err
		}

		// Add response to history
		agentCtx.Messages = append(agentCtx.Messages,
			llm.AssistantMessage(response1.Text),
		)

		// Follow-up question
		response2, err := agent.Chat(ctx, agentCtx, "Can you elaborate?")
		if err != nil {
			return "", err
		}

		return response2.Text, nil
	},
)
```

## Configuration Options

### Provider & Model

```go
llm.WithProvider("anthropic")  // anthropic, openai, gemini
llm.WithModel("claude-sonnet-4-5-20250929")
```

### Generation Parameters

```go
llm.WithMaxTokens(1024)
llm.WithTemperature(0.7)
llm.WithTopP(0.9)
llm.WithStopSequences([]string{"\n\n"})
```

### System Message

```go
llm.WithSystemMessage("You are a helpful coding assistant.")
```

### Tools

```go
searchTool := llm.MustNewTool("search", "Search the web", searchHandler)
llm.WithTools(searchTool)
```

## App-Level Defaults

Use `llm.SetAppDefaults` to configure defaults for all LLM calls in an app:

```go
app := romancy.NewApp(romancy.WithDatabase("workflow.db"))

llm.SetAppDefaults(app,
	llm.WithProvider("anthropic"),
	llm.WithModel("claude-sonnet-4-5-20250929"),
	llm.WithMaxTokens(1024),
)

// All LLM calls in workflows registered with this app
// will use these defaults unless overridden
```

Per-call options override app defaults:

```go
// Uses app default provider/model, but overrides max tokens
response, err := llm.Call(ctx, "Hello",
	llm.WithMaxTokens(100),  // Overrides app default
)
```

## DurableResponse

All LLM calls return a `DurableResponse`:

```go
type DurableResponse struct {
	Text         string            `json:"text"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	Usage        *Usage            `json:"usage,omitempty"`
	ToolCalls    []ToolCall        `json:"tool_calls,omitempty"`
	FinishReason string            `json:"finish_reason,omitempty"`
	Structured   json.RawMessage   `json:"structured,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
```

## Supported Providers

Via bucephalus, Romancy supports:

| Provider | Import | Models |
|----------|--------|--------|
| Anthropic | `_ "github.com/i2y/bucephalus/anthropic"` | Claude models |
| OpenAI | `_ "github.com/i2y/bucephalus/openai"` | GPT models |
| Google | `_ "github.com/i2y/bucephalus/gemini"` | Gemini models |

## How Caching Works

1. Each LLM call generates a deterministic activity ID (e.g., `llm_call:1`)
2. The result is stored in the workflow history
3. On replay, cached results are returned without API calls
4. This ensures:
   - Deterministic replay
   - Cost savings (no duplicate API calls)
   - Consistent behavior across workflow restarts

## Example: Complete Workflow

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/llm"
	_ "github.com/i2y/bucephalus/anthropic"
)

type AnalysisInput struct {
	Text string `json:"text"`
}

type AnalysisResult struct {
	Summary  string   `json:"summary"`
	Keywords []string `json:"keywords"`
}

type Keywords struct {
	Keywords []string `json:"keywords"`
}

var analyzeWorkflow = romancy.DefineWorkflow("analyze",
	func(ctx *romancy.WorkflowContext, input AnalysisInput) (AnalysisResult, error) {
		// Step 1: Summarize
		summary, err := llm.Call(ctx, input.Text,
			llm.WithSystemMessage("Summarize in one sentence."),
		)
		if err != nil {
			return AnalysisResult{}, err
		}

		// Step 2: Extract keywords
		keywords, err := llm.CallParse[Keywords](ctx,
			fmt.Sprintf("Extract 5 keywords from: %s", input.Text),
		)
		if err != nil {
			return AnalysisResult{}, err
		}

		return AnalysisResult{
			Summary:  summary.Text,
			Keywords: keywords.Keywords,
		}, nil
	},
)

func main() {
	ctx := context.Background()

	app := romancy.NewApp(romancy.WithDatabase("analysis.db"))

	llm.SetAppDefaults(app,
		llm.WithProvider("anthropic"),
		llm.WithModel("claude-sonnet-4-5-20250929"),
	)

	romancy.RegisterWorkflow(app, analyzeWorkflow)

	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(ctx)

	instanceID, _ := romancy.StartWorkflow(ctx, app, analyzeWorkflow, AnalysisInput{
		Text: "Go is a statically typed, compiled programming language...",
	})
	fmt.Println("Started:", instanceID)
}
```

## Related Documentation

- [Workflows and Activities](/docs/core-features/workflows-activities)
- [Durable Execution](/docs/core-features/durable-execution/replay)
- [bucephalus Documentation](https://github.com/i2y/bucephalus)
