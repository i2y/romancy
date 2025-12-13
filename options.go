package romancy

import (
	"time"

	"github.com/i2y/romancy/hooks"
)

// Option configures an App.
type Option func(*appConfig)

// appConfig holds the configuration for an App.
type appConfig struct {
	// Database
	databaseURL string
	autoMigrate bool

	// Service identity
	serviceName string
	workerID    string

	// Features
	outboxEnabled   bool
	outboxInterval  time.Duration
	outboxBatchSize int
	brokerURL       string

	// PostgreSQL LISTEN/NOTIFY
	useListenNotify      *bool         // nil = auto-detect, true = force enable, false = force disable
	notifyReconnectDelay time.Duration // Delay before reconnecting after connection failure

	// Background task intervals
	staleLockInterval          time.Duration
	staleLockTimeout           time.Duration
	timerCheckInterval         time.Duration
	eventTimeoutInterval       time.Duration
	messageCheckInterval       time.Duration
	recurCheckInterval         time.Duration
	channelCleanupInterval     time.Duration
	channelMessageRetention    time.Duration
	workflowResumptionInterval time.Duration

	// Concurrency control
	maxConcurrentResumptions int
	maxConcurrentTimers      int
	maxConcurrentMessages    int

	// Batch sizes for background tasks
	maxWorkflowsPerBatch int
	maxTimersPerBatch    int
	maxMessagesPerBatch  int

	// Singleton task configuration
	singletonStaleLockCleanup bool
	singletonChannelCleanup   bool

	// Hooks
	hooks hooks.WorkflowHooks

	// Shutdown
	shutdownTimeout time.Duration
}

// defaultConfig returns the default configuration.
func defaultConfig() *appConfig {
	return &appConfig{
		databaseURL:                "file:romancy.db",
		autoMigrate:                true,
		serviceName:                "romancy-service",
		outboxEnabled:              false,
		outboxInterval:             1 * time.Second,
		outboxBatchSize:            100,
		useListenNotify:            nil, // auto-detect
		notifyReconnectDelay:       60 * time.Second,
		staleLockInterval:          60 * time.Second,
		staleLockTimeout:           300 * time.Second, // 5 minutes
		timerCheckInterval:         10 * time.Second,
		eventTimeoutInterval:       30 * time.Second,
		messageCheckInterval:       5 * time.Second,
		recurCheckInterval:         5 * time.Second,
		channelCleanupInterval:     300 * time.Second,
		channelMessageRetention:    24 * time.Hour,
		workflowResumptionInterval: 1 * time.Second, // Same as Edda
		maxConcurrentResumptions:   10,
		maxConcurrentTimers:        10,
		maxConcurrentMessages:      10,
		maxWorkflowsPerBatch:       100,
		maxTimersPerBatch:          100,
		maxMessagesPerBatch:        100,
		singletonStaleLockCleanup:  true,
		singletonChannelCleanup:    true,
		shutdownTimeout:            30 * time.Second,
		hooks:                      &hooks.NoOpHooks{},
	}
}

// WithDatabase sets the database connection URL.
// Supported formats:
//   - SQLite: "file:path/to/db.db" or "sqlite://path/to/db.db"
//   - PostgreSQL: "postgres://user:pass@host:port/dbname"
func WithDatabase(url string) Option {
	return func(c *appConfig) {
		c.databaseURL = url
	}
}

// WithAutoMigrate controls whether migrations run automatically on startup.
// Default is true. Set to false if you want to manage migrations manually
// using the CLI: `romancy migrate up --db <path>`
func WithAutoMigrate(enabled bool) Option {
	return func(c *appConfig) {
		c.autoMigrate = enabled
	}
}

// WithServiceName sets the service name for identification.
func WithServiceName(name string) Option {
	return func(c *appConfig) {
		c.serviceName = name
	}
}

// WithWorkerID sets a custom worker ID.
// If not set, a UUID will be generated.
func WithWorkerID(id string) Option {
	return func(c *appConfig) {
		c.workerID = id
	}
}

// WithOutbox enables the transactional outbox pattern.
func WithOutbox(enabled bool) Option {
	return func(c *appConfig) {
		c.outboxEnabled = enabled
	}
}

// WithOutboxInterval sets the interval for the outbox relayer.
func WithOutboxInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.outboxInterval = d
	}
}

// WithOutboxBatchSize sets the batch size for outbox processing.
func WithOutboxBatchSize(size int) Option {
	return func(c *appConfig) {
		c.outboxBatchSize = size
	}
}

// WithBrokerURL sets the CloudEvents broker URL for outbox event publishing.
// Example: "http://broker-ingress.knative-eventing.svc.cluster.local/default/default"
func WithBrokerURL(url string) Option {
	return func(c *appConfig) {
		c.brokerURL = url
	}
}

// WithStaleLockInterval sets the interval for stale lock cleanup.
func WithStaleLockInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.staleLockInterval = d
	}
}

// WithStaleLockTimeout sets the timeout after which a lock is considered stale.
func WithStaleLockTimeout(d time.Duration) Option {
	return func(c *appConfig) {
		c.staleLockTimeout = d
	}
}

// WithTimerCheckInterval sets the interval for checking expired timers.
func WithTimerCheckInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.timerCheckInterval = d
	}
}

// WithEventTimeoutInterval sets the interval for checking event timeouts.
func WithEventTimeoutInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.eventTimeoutInterval = d
	}
}

// WithHooks sets the workflow lifecycle hooks.
func WithHooks(h hooks.WorkflowHooks) Option {
	return func(c *appConfig) {
		c.hooks = h
	}
}

// WithShutdownTimeout sets the timeout for graceful shutdown.
func WithShutdownTimeout(d time.Duration) Option {
	return func(c *appConfig) {
		c.shutdownTimeout = d
	}
}

// WithMessageCheckInterval sets the interval for checking channel message subscriptions.
func WithMessageCheckInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.messageCheckInterval = d
	}
}

// WithRecurCheckInterval sets the interval for checking recurred workflows.
func WithRecurCheckInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.recurCheckInterval = d
	}
}

// WithChannelCleanupInterval sets the interval for cleaning up old channel messages.
func WithChannelCleanupInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.channelCleanupInterval = d
	}
}

// WithChannelMessageRetention sets how long to keep channel messages before cleanup.
func WithChannelMessageRetention(d time.Duration) Option {
	return func(c *appConfig) {
		c.channelMessageRetention = d
	}
}

// WithWorkflowResumptionInterval sets the interval for the workflow resumption task.
// This task finds workflows with status='running' that don't have an active lock
// and resumes them. This is essential for load balancing in multi-worker environments.
// Default: 1 second (same as Edda).
func WithWorkflowResumptionInterval(d time.Duration) Option {
	return func(c *appConfig) {
		c.workflowResumptionInterval = d
	}
}

// WithMaxConcurrentResumptions sets the maximum number of concurrent workflow resumptions.
// This limits goroutine spawning in background tasks to prevent resource exhaustion.
// Default: 10.
func WithMaxConcurrentResumptions(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxConcurrentResumptions = n
		}
	}
}

// WithMaxConcurrentTimers sets the maximum number of concurrent timer handlers.
// Default: 10.
func WithMaxConcurrentTimers(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxConcurrentTimers = n
		}
	}
}

// WithMaxConcurrentMessages sets the maximum number of concurrent message handlers.
// Default: 10.
func WithMaxConcurrentMessages(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxConcurrentMessages = n
		}
	}
}

// WithSingletonStaleLockCleanup sets whether stale lock cleanup runs as a singleton task.
// When true, only one worker will run the cleanup at a time using system locks.
// Default: true.
func WithSingletonStaleLockCleanup(enabled bool) Option {
	return func(c *appConfig) {
		c.singletonStaleLockCleanup = enabled
	}
}

// WithSingletonChannelCleanup sets whether channel cleanup runs as a singleton task.
// When true, only one worker will run the cleanup at a time using system locks.
// Default: true.
func WithSingletonChannelCleanup(enabled bool) Option {
	return func(c *appConfig) {
		c.singletonChannelCleanup = enabled
	}
}

// WithListenNotify configures PostgreSQL LISTEN/NOTIFY usage.
// - nil (default): auto-detect based on database URL (enabled for PostgreSQL)
// - true: force enable (fails if not PostgreSQL)
// - false: force disable (use polling only)
func WithListenNotify(enabled *bool) Option {
	return func(c *appConfig) {
		c.useListenNotify = enabled
	}
}

// WithNotifyReconnectDelay sets the delay before reconnecting
// after a LISTEN/NOTIFY connection failure. Default: 60 seconds.
func WithNotifyReconnectDelay(d time.Duration) Option {
	return func(c *appConfig) {
		c.notifyReconnectDelay = d
	}
}

// WithMaxWorkflowsPerBatch sets the maximum number of workflows
// to process per polling cycle. Default: 100.
func WithMaxWorkflowsPerBatch(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxWorkflowsPerBatch = n
		}
	}
}

// WithMaxTimersPerBatch sets the maximum number of timers
// to process per polling cycle. Default: 100.
func WithMaxTimersPerBatch(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxTimersPerBatch = n
		}
	}
}

// WithMaxMessagesPerBatch sets the maximum number of channel messages
// to process per polling cycle. Default: 100.
func WithMaxMessagesPerBatch(n int) Option {
	return func(c *appConfig) {
		if n > 0 {
			c.maxMessagesPerBatch = n
		}
	}
}
