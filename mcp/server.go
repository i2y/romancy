package mcp

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/internal/storage"
)

// Server is an MCP server that exposes Romancy workflows as tools.
// It wraps both a Romancy App and an MCP Server, providing a unified
// lifecycle management.
//
// Example usage with stdio transport:
//
//	app := romancy.NewApp(romancy.WithDatabase("workflow.db"))
//	server := mcp.NewServer(app,
//	    mcp.WithServerName("order-service"),
//	    mcp.WithServerVersion("1.0.0"),
//	)
//
//	// Register workflows (auto-generates 4 tools per workflow)
//	mcp.RegisterWorkflow[OrderInput, OrderResult](server, &OrderWorkflow{})
//
//	// Start the server
//	ctx := context.Background()
//	if err := server.Initialize(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer server.Shutdown(ctx)
//
//	if err := server.RunStdio(ctx); err != nil {
//	    log.Fatal(err)
//	}
//
// Example usage with HTTP transport:
//
//	http.Handle("/mcp", server.Handler())
//	http.ListenAndServe(":8080", nil)
type Server struct {
	app       *romancy.App
	mcpServer *mcp.Server
	config    *serverConfig

	// Registered workflows for tool generation
	workflows   map[string]workflowInfo
	workflowsMu sync.RWMutex

	// State
	initialized bool
	mu          sync.Mutex
}

// workflowInfo stores workflow metadata for MCP tool generation.
type workflowInfo struct {
	name     string
	workflow any
	config   *workflowToolConfig
}

// NewServer creates a new MCP server that wraps a Romancy App.
func NewServer(app *romancy.App, opts ...ServerOption) *Server {
	config := defaultServerConfig()
	for _, opt := range opts {
		opt(config)
	}

	// Create the MCP server
	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    config.name,
			Version: config.version,
		},
		nil, // ServerOptions (could add lifecycle hooks here)
	)

	return &Server{
		app:       app,
		mcpServer: mcpServer,
		config:    config,
		workflows: make(map[string]workflowInfo),
	}
}

// Initialize starts the underlying Romancy App.
// This must be called before running the MCP server.
func (s *Server) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized {
		return nil
	}

	if err := s.app.Start(ctx); err != nil {
		return fmt.Errorf("failed to start romancy app: %w", err)
	}

	s.initialized = true
	return nil
}

// Shutdown gracefully shuts down both the Romancy App and MCP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.initialized {
		return nil
	}

	s.initialized = false
	return s.app.Shutdown(ctx)
}

// RunStdio runs the MCP server using stdio transport.
// This is the standard mode for Claude Desktop integration.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &mcp.StdioTransport{})
}

// Handler returns an HTTP handler for the MCP server.
// This can be used for REST API integration.
//
// Example:
//
//	http.Handle("/mcp", server.Handler())
func (s *Server) Handler() http.Handler {
	// The MCP SDK doesn't directly support HTTP transport,
	// so we implement a simple JSON-RPC handler.
	return &httpHandler{server: s}
}

// App returns the underlying Romancy App.
// This can be used for direct access to the workflow engine.
func (s *Server) App() *romancy.App {
	return s.app
}

// Storage returns the storage interface for querying workflow instances.
func (s *Server) Storage() storage.Storage {
	return s.app.Storage()
}

// MCPServer returns the underlying MCP Server.
// This can be used for advanced customization.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcpServer
}

// registerWorkflow stores workflow info for tool generation.
func (s *Server) registerWorkflow(name string, workflow any, config *workflowToolConfig) {
	s.workflowsMu.Lock()
	defer s.workflowsMu.Unlock()

	s.workflows[name] = workflowInfo{
		name:     name,
		workflow: workflow,
		config:   config,
	}
}

// httpHandler implements HTTP transport for MCP.
type httpHandler struct {
	server *Server
}

// ServeHTTP handles HTTP requests for the MCP server.
// It wraps the MCP protocol over HTTP for non-stdio clients.
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// For now, return a simple status response.
	// Full HTTP transport support would require implementing
	// the MCP protocol over HTTP/SSE.
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"name":%q,"version":%q,"status":"running"}`,
			h.server.config.name, h.server.config.version)
		return
	}

	// POST requests would handle MCP JSON-RPC messages
	w.WriteHeader(http.StatusMethodNotAllowed)
}
