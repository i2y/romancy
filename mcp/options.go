// Package mcp provides Model Context Protocol (MCP) integration for Romancy.
// It allows exposing workflows as MCP tools for AI assistants like Claude.
package mcp

// ServerOption configures an MCP Server.
type ServerOption func(*serverConfig)

// serverConfig holds MCP server configuration.
type serverConfig struct {
	// Server identity
	name    string
	version string

	// Transport options
	transportMode TransportMode
}

// TransportMode defines the MCP transport type.
type TransportMode string

const (
	// TransportStdio uses stdio transport (for Claude Desktop).
	TransportStdio TransportMode = "stdio"
	// TransportHTTP uses HTTP transport (for REST clients).
	TransportHTTP TransportMode = "http"
)

// defaultServerConfig returns the default server configuration.
func defaultServerConfig() *serverConfig {
	return &serverConfig{
		name:          "romancy-mcp-server",
		version:       "1.0.0",
		transportMode: TransportStdio,
	}
}

// WithServerName sets the MCP server name.
func WithServerName(name string) ServerOption {
	return func(c *serverConfig) {
		c.name = name
	}
}

// WithServerVersion sets the MCP server version.
func WithServerVersion(version string) ServerOption {
	return func(c *serverConfig) {
		c.version = version
	}
}

// WithTransportMode sets the transport mode (stdio or http).
func WithTransportMode(mode TransportMode) ServerOption {
	return func(c *serverConfig) {
		c.transportMode = mode
	}
}

// WorkflowToolOption configures workflow tool registration.
type WorkflowToolOption func(*workflowToolConfig)

// workflowToolConfig holds workflow tool configuration.
type workflowToolConfig struct {
	// Tool descriptions
	description       string
	startDescription  string
	statusDescription string
	resultDescription string
	cancelDescription string

	// Tool name prefix (defaults to workflow name)
	namePrefix string
}

// defaultWorkflowToolConfig returns the default workflow tool configuration.
func defaultWorkflowToolConfig() *workflowToolConfig {
	return &workflowToolConfig{
		description:       "",
		startDescription:  "Start a new workflow instance",
		statusDescription: "Get the current status of a workflow instance",
		resultDescription: "Get the result of a completed workflow instance",
		cancelDescription: "Cancel a running workflow instance",
	}
}

// WithDescription sets the main description for the workflow tools.
func WithDescription(desc string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.description = desc
	}
}

// WithStartDescription sets the description for the start tool.
func WithStartDescription(desc string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.startDescription = desc
	}
}

// WithStatusDescription sets the description for the status tool.
func WithStatusDescription(desc string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.statusDescription = desc
	}
}

// WithResultDescription sets the description for the result tool.
func WithResultDescription(desc string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.resultDescription = desc
	}
}

// WithCancelDescription sets the description for the cancel tool.
func WithCancelDescription(desc string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.cancelDescription = desc
	}
}

// WithNamePrefix sets a custom name prefix for the generated tools.
// By default, the workflow name is used.
func WithNamePrefix(prefix string) WorkflowToolOption {
	return func(c *workflowToolConfig) {
		c.namePrefix = prefix
	}
}
