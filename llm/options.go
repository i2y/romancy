// Package llm provides durable LLM integration for Romancy workflows.
// LLM calls are automatically wrapped as activities, enabling deterministic
// replay without re-invoking the LLM API.
package llm

import (
	bucephalus "github.com/i2y/bucephalus/llm"
)

// Option configures an LLM call.
type Option func(*config)

// config holds the LLM call configuration.
type config struct {
	provider      string
	model         string
	temperature   *float64
	maxTokens     *int
	topP          *float64
	topK          *int
	seed          *int
	systemMessage string
	tools         []bucephalus.Tool
	stopSequences []string
}

// newConfig creates a new config with default values.
func newConfig() *config {
	return &config{}
}

// applyOptions applies options to the config.
func (c *config) applyOptions(opts ...Option) {
	for _, opt := range opts {
		opt(c)
	}
}

// toBucephalusOptions converts config to bucephalus options.
func (c *config) toBucephalusOptions() []bucephalus.Option {
	var opts []bucephalus.Option

	if c.provider != "" {
		opts = append(opts, bucephalus.WithProvider(c.provider))
	}
	if c.model != "" {
		opts = append(opts, bucephalus.WithModel(c.model))
	}
	if c.temperature != nil {
		opts = append(opts, bucephalus.WithTemperature(*c.temperature))
	}
	if c.maxTokens != nil {
		opts = append(opts, bucephalus.WithMaxTokens(*c.maxTokens))
	}
	if c.topP != nil {
		opts = append(opts, bucephalus.WithTopP(*c.topP))
	}
	if c.topK != nil {
		opts = append(opts, bucephalus.WithTopK(*c.topK))
	}
	if c.seed != nil {
		opts = append(opts, bucephalus.WithSeed(*c.seed))
	}
	if c.systemMessage != "" {
		opts = append(opts, bucephalus.WithSystemMessage(c.systemMessage))
	}
	if len(c.tools) > 0 {
		opts = append(opts, bucephalus.WithTools(c.tools...))
	}
	if len(c.stopSequences) > 0 {
		opts = append(opts, bucephalus.WithStopSequences(c.stopSequences...))
	}

	return opts
}

// WithProvider sets the LLM provider (e.g., "anthropic", "openai", "gemini").
func WithProvider(provider string) Option {
	return func(c *config) {
		c.provider = provider
	}
}

// WithModel sets the model name (e.g., "claude-sonnet-4-20250514", "gpt-4o").
func WithModel(model string) Option {
	return func(c *config) {
		c.model = model
	}
}

// WithTemperature sets the sampling temperature (typically 0-1 or 0-2).
func WithTemperature(t float64) Option {
	return func(c *config) {
		c.temperature = &t
	}
}

// WithMaxTokens sets the maximum number of tokens to generate.
func WithMaxTokens(n int) Option {
	return func(c *config) {
		c.maxTokens = &n
	}
}

// WithTopP sets nucleus sampling parameter (0-1).
func WithTopP(p float64) Option {
	return func(c *config) {
		c.topP = &p
	}
}

// WithTopK sets top-k sampling parameter.
// Note: Not supported by all providers (e.g., OpenAI).
func WithTopK(k int) Option {
	return func(c *config) {
		c.topK = &k
	}
}

// WithSeed sets the random seed for reproducible outputs.
// Note: Not supported by all providers (e.g., Anthropic).
func WithSeed(seed int) Option {
	return func(c *config) {
		c.seed = &seed
	}
}

// WithSystemMessage sets the system message for the LLM call.
func WithSystemMessage(msg string) Option {
	return func(c *config) {
		c.systemMessage = msg
	}
}

// WithTools sets the tools available for the LLM to call.
func WithTools(tools ...Tool) Option {
	return func(c *config) {
		c.tools = tools
	}
}

// WithStopSequences sets sequences that will stop generation.
func WithStopSequences(seqs ...string) Option {
	return func(c *config) {
		c.stopSequences = seqs
	}
}

// mergeConfigs merges multiple configs, with later configs taking precedence.
func mergeConfigs(configs ...*config) *config {
	result := newConfig()

	for _, c := range configs {
		if c == nil {
			continue
		}
		if c.provider != "" {
			result.provider = c.provider
		}
		if c.model != "" {
			result.model = c.model
		}
		if c.temperature != nil {
			result.temperature = c.temperature
		}
		if c.maxTokens != nil {
			result.maxTokens = c.maxTokens
		}
		if c.topP != nil {
			result.topP = c.topP
		}
		if c.topK != nil {
			result.topK = c.topK
		}
		if c.seed != nil {
			result.seed = c.seed
		}
		if c.systemMessage != "" {
			result.systemMessage = c.systemMessage
		}
		if len(c.tools) > 0 {
			result.tools = c.tools
		}
		if len(c.stopSequences) > 0 {
			result.stopSequences = c.stopSequences
		}
	}

	return result
}
