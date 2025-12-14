package llm

import (
	"encoding/json"

	bucephalus "github.com/i2y/bucephalus/llm"
)

// DurableResponse is a JSON-serializable LLM response for activity caching.
// It captures the essential information from an LLM call for replay.
type DurableResponse struct {
	// Text is the raw text content of the response.
	Text string `json:"text"`

	// Model is the model that generated the response.
	Model string `json:"model,omitempty"`

	// Provider is the LLM provider (e.g., "anthropic", "openai").
	Provider string `json:"provider,omitempty"`

	// Usage contains token usage information.
	Usage *Usage `json:"usage,omitempty"`

	// ToolCalls contains any tool calls requested by the model.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// FinishReason indicates why the model stopped generating.
	FinishReason string `json:"finish_reason,omitempty"`

	// Structured holds parsed structured data as raw JSON.
	// This is populated when using CallParse or CallMessagesParse.
	Structured json.RawMessage `json:"structured,omitempty"`
}

// Usage contains token usage information.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ToolCall represents a tool call requested by the model.
type ToolCall struct {
	// ID is the unique identifier for this tool call.
	ID string `json:"id"`

	// Name is the name of the tool to call.
	Name string `json:"name"`

	// Arguments is the JSON-encoded arguments for the tool.
	Arguments json.RawMessage `json:"arguments"`
}

// HasToolCalls returns true if the response contains tool calls.
func (r *DurableResponse) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// FromBucephalusResponse converts a bucephalus Response to a DurableResponse.
func FromBucephalusResponse[T any](resp bucephalus.Response[T], provider, model string) *DurableResponse {
	dr := &DurableResponse{
		Text:         resp.Text(),
		Model:        model,
		Provider:     provider,
		FinishReason: string(resp.FinishReason()),
	}

	// Copy usage
	usage := resp.Usage()
	if usage.TotalTokens > 0 {
		dr.Usage = &Usage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
		}
	}

	// Copy tool calls
	toolCalls := resp.ToolCalls()
	if len(toolCalls) > 0 {
		dr.ToolCalls = make([]ToolCall, len(toolCalls))
		for i, tc := range toolCalls {
			dr.ToolCalls[i] = ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: json.RawMessage(tc.Arguments),
			}
		}
	}

	return dr
}

// FromBucephalusResponseWithParsed converts a bucephalus Response to a DurableResponse
// and includes the parsed structured data.
func FromBucephalusResponseWithParsed[T any](resp bucephalus.Response[T], provider, model string, parsed T) *DurableResponse {
	dr := FromBucephalusResponse(resp, provider, model)

	// Serialize the parsed value to store in the response
	if data, err := json.Marshal(parsed); err == nil {
		dr.Structured = data
	}

	return dr
}

// ParseStructured parses the structured data into the given type.
func ParseStructured[T any](dr *DurableResponse) (T, error) {
	var result T
	if len(dr.Structured) == 0 {
		return result, nil
	}
	err := json.Unmarshal(dr.Structured, &result)
	return result, err
}
