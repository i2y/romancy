# Romancy Development Environment
# ================================
# Usage:
#   tilt up                    # Start all examples
#   tilt up simple             # Start only simple example
#   tilt up demo booking       # Start demo and booking examples
#   tilt down                  # Stop all
#
# Services:
#   - PostgreSQL:   localhost:5433
#   - Jaeger UI:    http://localhost:16686
#   - OTLP gRPC:    localhost:4317
#   - Tilt UI:      http://localhost:10350

# Load docker-compose for infrastructure services
docker_compose("docker-compose.yml")

# Configure infrastructure resources
dc_resource("postgres", labels=["infrastructure"])
dc_resource("jaeger", labels=["tracing"], links=["http://localhost:16686"])

# Common environment variables for all examples
common_env = {
    "DATABASE_URL": "postgres://romancy:romancy@127.0.0.1:5433/romancy_test?sslmode=disable",
    "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317",
    "ROMANCY_SERVE": "true",  # Keep one-shot examples running for Tilt
    # Suppress verbose gRPC logging (gRPC v1.74+ uses log/slog by default)
    "GRPC_GO_LOG_SEVERITY_LEVEL": "ERROR",
}

# Example definitions
# Each example can be enabled/disabled independently
examples = {
    "simple": {
        "dir": "examples/simple",
        "port": None,
        "description": "Basic workflow without events",
    },
    "demo": {
        "dir": "examples/demo",
        "port": 8080,
        "description": "Order processing with events",
    },
    "saga": {
        "dir": "examples/saga",
        "port": None,
        "description": "Saga pattern with compensation",
    },
    "booking": {
        "dir": "examples/booking",
        "port": 8083,
        "description": "Human-in-the-loop approval",
    },
    "event_handler": {
        "dir": "examples/event_handler",
        "port": 8082,
        "description": "Event-driven workflows",
    },
    "retry": {
        "dir": "examples/retry",
        "port": None,
        "description": "Retry policies demo",
    },
    "timer": {
        "dir": "examples/timer",
        "port": 8081,
        "description": "Sleep and timer workflows",
    },
    "mcp": {
        "dir": "examples/mcp",
        "port": None,
        "description": "MCP server integration (stdio)",
    },
    "channel": {
        "dir": "examples/channel",
        "port": 8084,
        "description": "Channel-based message passing",
    },
}

# Common source files to watch for all examples
common_deps = [
    "*.go",
    "go.mod",
    "go.sum",
    "internal",
    "hooks",
    "retry",
    "compensation",
    "outbox",
    "mcp",
]

# Define resources for each example
for name, cfg in examples.items():
    # Build environment with service name
    env = dict(common_env)
    env["OTEL_SERVICE_NAME"] = "romancy-" + name

    # Dependencies: example directory + common source files
    deps = [cfg["dir"]] + common_deps

    # Links for HTTP examples
    links = []
    if cfg["port"]:
        links.append("http://localhost:" + str(cfg["port"]))

    # Create local resource for the example
    local_resource(
        name,
        serve_cmd="go run ./" + cfg["dir"],
        deps=deps,
        serve_env=env,
        labels=["examples"],
        resource_deps=["postgres"],
        links=links,
        allow_parallel=True,
    )

# Print helpful information on startup
local_resource(
    "info",
    cmd="echo '✓ Romancy development environment ready'",
    labels=["infrastructure"],
    resource_deps=["postgres", "jaeger"],
    auto_init=True,
)
