package llm

import (
	"sync"

	"github.com/i2y/romancy"
)

// appDefaults stores LLM defaults per App instance.
// Key: *romancy.App, Value: []Option
var appDefaults sync.Map

// SetAppDefaults sets the default LLM options for an App.
// These defaults are applied to all LLM calls within workflows
// registered with that App, unless overridden by per-call options.
//
// Example:
//
//	app := romancy.NewApp(romancy.WithDatabase("workflow.db"))
//	llm.SetAppDefaults(app,
//	    llm.WithProvider("anthropic"),
//	    llm.WithModel("claude-sonnet-4-5-20250929"),
//	    llm.WithMaxTokens(1024),
//	)
func SetAppDefaults(app *romancy.App, opts ...Option) {
	appDefaults.Store(app, opts)
}

// getAppDefaults retrieves the default LLM options for an App.
// Returns nil if no defaults were set.
func getAppDefaults(app *romancy.App) []Option {
	if app == nil {
		return nil
	}
	if v, ok := appDefaults.Load(app); ok {
		return v.([]Option)
	}
	return nil
}

// ClearAppDefaults removes the default LLM options for an App.
// This is useful for cleanup in tests or when shutting down an App.
func ClearAppDefaults(app *romancy.App) {
	appDefaults.Delete(app)
}
