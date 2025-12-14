// Package main demonstrates LLM integration with Romancy workflows.
//
// This example shows:
//   - Ad-hoc LLM calls with llm.Call()
//   - Structured output with llm.CallParse()
//   - Multi-turn conversations with llm.CallMessages()
//   - Reusable LLM definitions with llm.DefineDurableCall()
//   - App-level LLM defaults with llm.SetAppDefaults()
//
// The key benefit is that all LLM calls are automatically cached as activities,
// so workflow replay will return cached results without re-invoking the LLM API.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/llm"

	// Import the provider you want to use
	_ "github.com/i2y/bucephalus/anthropic"
	// _ "github.com/i2y/bucephalus/openai"
	// _ "github.com/i2y/bucephalus/gemini"
)

// SummaryInput is the input for the summary workflow.
type SummaryInput struct {
	Text string `json:"text"`
}

// SummaryResult is the output of the summary workflow.
type SummaryResult struct {
	Summary string `json:"summary"`
}

// BookRecommendation demonstrates structured output.
type BookRecommendation struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	Reason string `json:"reason"`
}

// Define a reusable LLM call for summarization
var summarizer = llm.DefineDurableCall("summarize",
	llm.WithSystemMessage("You are a helpful assistant that summarizes text concisely. Output only the summary, nothing else."),
	llm.WithMaxTokens(500),
)

// summarizeWorkflow demonstrates ad-hoc LLM calls.
var summarizeWorkflow = romancy.DefineWorkflow("summarize",
	func(ctx *romancy.WorkflowContext, input SummaryInput) (SummaryResult, error) {
		// Option 1: Ad-hoc LLM call
		response, err := llm.Call(ctx, input.Text,
			llm.WithSystemMessage("Summarize the following text concisely in one paragraph."),
		)
		if err != nil {
			return SummaryResult{}, err
		}

		return SummaryResult{Summary: response.Text}, nil
	},
)

// summarizeWithDurableCallWorkflow demonstrates reusable LLM definitions.
var summarizeWithDurableCallWorkflow = romancy.DefineWorkflow("summarize_durable",
	func(ctx *romancy.WorkflowContext, input SummaryInput) (SummaryResult, error) {
		// Option 2: Using a pre-defined DurableCall
		response, err := summarizer.Execute(ctx, input.Text)
		if err != nil {
			return SummaryResult{}, err
		}

		return SummaryResult{Summary: response.Text}, nil
	},
)

// recommendBookWorkflow demonstrates structured output.
var recommendBookWorkflow = romancy.DefineWorkflow("recommend_book",
	func(ctx *romancy.WorkflowContext, genre string) (*BookRecommendation, error) {
		// Use CallParse for structured output
		book, err := llm.CallParse[BookRecommendation](ctx,
			fmt.Sprintf("Recommend a %s book. Respond with JSON containing title, author, and reason fields.", genre),
		)
		if err != nil {
			return nil, err
		}

		return book, nil
	},
)

// conversationWorkflow demonstrates multi-turn conversations.
var conversationWorkflow = romancy.DefineWorkflow("conversation",
	func(ctx *romancy.WorkflowContext, topic string) (string, error) {
		// Build a multi-turn conversation
		messages := []llm.Message{
			llm.SystemMessage("You are a helpful assistant."),
			llm.UserMessage(fmt.Sprintf("What is %s?", topic)),
		}

		// First turn
		response1, err := llm.CallMessages(ctx, messages)
		if err != nil {
			return "", err
		}

		// Add assistant response and follow-up question
		messages = append(messages, llm.AssistantMessage(response1.Text))
		messages = append(messages, llm.UserMessage("Can you give me a simple example?"))

		// Second turn
		response2, err := llm.CallMessages(ctx, messages)
		if err != nil {
			return "", err
		}

		return response2.Text, nil
	},
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Check for API key
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY environment variable is required")
	}

	ctx := context.Background()

	// Create app
	app := romancy.NewApp(
		romancy.WithDatabase("file:llm_example.db"),
	)

	// Set LLM defaults for the app
	// Note: Use claude-sonnet-4-5 for structured output support (CallParse)
	llm.SetAppDefaults(app,
		llm.WithProvider("anthropic"),
		llm.WithModel("claude-sonnet-4-5-20250929"),
		llm.WithMaxTokens(1024),
	)

	// Register workflows
	romancy.RegisterWorkflow(app, summarizeWorkflow)
	romancy.RegisterWorkflow(app, summarizeWithDurableCallWorkflow)
	romancy.RegisterWorkflow(app, recommendBookWorkflow)
	romancy.RegisterWorkflow(app, conversationWorkflow)

	// Start the app
	if err := app.Start(ctx); err != nil {
		return fmt.Errorf("failed to start app: %w", err)
	}
	defer func() {
		if err := app.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	// Example 1: Summarize text
	fmt.Println("=== Example 1: Summarize Text ===")
	sampleText := `
	Go is a statically typed, compiled programming language designed at Google.
	It is syntactically similar to C, but with memory safety, garbage collection,
	structural typing, and CSP-style concurrency. Go was designed by Robert Griesemer,
	Rob Pike, and Ken Thompson. It was announced in 2009 and has since become popular
	for cloud infrastructure, microservices, and command-line tools.
	`

	instanceID, err := romancy.StartWorkflow(ctx, app, summarizeWorkflow, SummaryInput{Text: sampleText})
	if err != nil {
		return fmt.Errorf("failed to start workflow: %w", err)
	}
	fmt.Printf("Started summarize workflow: %s\n", instanceID)

	// Wait for result
	for {
		result, err := romancy.GetWorkflowResult[SummaryResult](ctx, app, instanceID)
		if err != nil {
			return fmt.Errorf("failed to get result: %w", err)
		}
		if result.Status == "completed" {
			fmt.Printf("Summary: %s\n\n", result.Output.Summary)
			break
		}
		if result.Status == "failed" {
			return fmt.Errorf("workflow failed: %v", result.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Example 2: Book recommendation with structured output
	fmt.Println("=== Example 2: Book Recommendation ===")
	instanceID2, err := romancy.StartWorkflow(ctx, app, recommendBookWorkflow, "science fiction")
	if err != nil {
		return fmt.Errorf("failed to start workflow: %w", err)
	}
	fmt.Printf("Started recommend_book workflow: %s\n", instanceID2)

	// Wait for result
	for {
		result, err := romancy.GetWorkflowResult[*BookRecommendation](ctx, app, instanceID2)
		if err != nil {
			return fmt.Errorf("failed to get result: %w", err)
		}
		if result.Status == "completed" {
			fmt.Printf("Recommended Book:\n")
			fmt.Printf("  Title: %s\n", result.Output.Title)
			fmt.Printf("  Author: %s\n", result.Output.Author)
			fmt.Printf("  Reason: %s\n\n", result.Output.Reason)
			break
		}
		if result.Status == "failed" {
			return fmt.Errorf("workflow failed: %v", result.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Example 3: Multi-turn conversation
	fmt.Println("=== Example 3: Multi-turn Conversation ===")
	instanceID3, err := romancy.StartWorkflow(ctx, app, conversationWorkflow, "goroutines in Go")
	if err != nil {
		return fmt.Errorf("failed to start workflow: %w", err)
	}
	fmt.Printf("Started conversation workflow: %s\n", instanceID3)

	// Wait for result
	for {
		result, err := romancy.GetWorkflowResult[string](ctx, app, instanceID3)
		if err != nil {
			return fmt.Errorf("failed to get result: %w", err)
		}
		if result.Status == "completed" {
			fmt.Printf("Final response: %s\n", result.Output)
			break
		}
		if result.Status == "failed" {
			return fmt.Errorf("workflow failed: %v", result.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Println("\nAll examples completed!")
	return nil
}
