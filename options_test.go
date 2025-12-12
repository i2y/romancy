package romancy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/i2y/romancy/hooks"
)

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()

	// Database defaults
	assert.Equal(t, "file:romancy.db", cfg.databaseURL)
	assert.True(t, cfg.autoMigrate)

	// Service identity defaults
	assert.Equal(t, "romancy-service", cfg.serviceName)
	assert.Empty(t, cfg.workerID) // UUID generated at runtime

	// Feature defaults
	assert.False(t, cfg.outboxEnabled)
	assert.Equal(t, 1*time.Second, cfg.outboxInterval)
	assert.Equal(t, 100, cfg.outboxBatchSize)
	assert.Empty(t, cfg.brokerURL)

	// Background task interval defaults
	assert.Equal(t, 60*time.Second, cfg.staleLockInterval)
	assert.Equal(t, 300*time.Second, cfg.staleLockTimeout)
	assert.Equal(t, 10*time.Second, cfg.timerCheckInterval)
	assert.Equal(t, 30*time.Second, cfg.eventTimeoutInterval)
	assert.Equal(t, 5*time.Second, cfg.messageCheckInterval)
	assert.Equal(t, 5*time.Second, cfg.recurCheckInterval)
	assert.Equal(t, 300*time.Second, cfg.channelCleanupInterval)
	assert.Equal(t, 24*time.Hour, cfg.channelMessageRetention)
	assert.Equal(t, 1*time.Second, cfg.workflowResumptionInterval)

	// Concurrency defaults
	assert.Equal(t, 10, cfg.maxConcurrentResumptions)
	assert.Equal(t, 10, cfg.maxConcurrentTimers)
	assert.Equal(t, 10, cfg.maxConcurrentMessages)

	// Singleton defaults
	assert.True(t, cfg.singletonStaleLockCleanup)
	assert.True(t, cfg.singletonChannelCleanup)

	// Hooks default
	assert.IsType(t, &hooks.NoOpHooks{}, cfg.hooks)

	// Shutdown default
	assert.Equal(t, 30*time.Second, cfg.shutdownTimeout)
}

func TestWithDatabase(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			name: "sqlite file path",
			url:  "file:test.db",
		},
		{
			name: "sqlite simple path",
			url:  "test.db",
		},
		{
			name: "sqlite memory",
			url:  ":memory:",
		},
		{
			name: "postgres",
			url:  "postgres://user:pass@localhost:5432/dbname",
		},
		{
			name: "mysql",
			url:  "mysql://user:pass@localhost:3306/dbname",
		},
		{
			name: "empty string",
			url:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithDatabase(tt.url)(cfg)
			assert.Equal(t, tt.url, cfg.databaseURL)
		})
	}
}

func TestWithAutoMigrate(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "enabled",
			enabled: true,
		},
		{
			name:    "disabled",
			enabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithAutoMigrate(tt.enabled)(cfg)
			assert.Equal(t, tt.enabled, cfg.autoMigrate)
		})
	}
}

func TestWithServiceName(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{
			name:        "custom service name",
			serviceName: "my-workflow-service",
		},
		{
			name:        "empty string",
			serviceName: "",
		},
		{
			name:        "with special characters",
			serviceName: "service-v1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithServiceName(tt.serviceName)(cfg)
			assert.Equal(t, tt.serviceName, cfg.serviceName)
		})
	}
}

func TestWithWorkerID(t *testing.T) {
	tests := []struct {
		name     string
		workerID string
	}{
		{
			name:     "custom worker ID",
			workerID: "worker-1",
		},
		{
			name:     "UUID style",
			workerID: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:     "empty string",
			workerID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithWorkerID(tt.workerID)(cfg)
			assert.Equal(t, tt.workerID, cfg.workerID)
		})
	}
}

func TestWithOutbox(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "enabled",
			enabled: true,
		},
		{
			name:    "disabled",
			enabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithOutbox(tt.enabled)(cfg)
			assert.Equal(t, tt.enabled, cfg.outboxEnabled)
		})
	}
}

func TestWithOutboxInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "1 second",
			interval: 1 * time.Second,
		},
		{
			name:     "500 milliseconds",
			interval: 500 * time.Millisecond,
		},
		{
			name:     "5 seconds",
			interval: 5 * time.Second,
		},
		{
			name:     "zero duration",
			interval: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithOutboxInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.outboxInterval)
		})
	}
}

func TestWithOutboxBatchSize(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{
			name: "default size",
			size: 100,
		},
		{
			name: "small batch",
			size: 10,
		},
		{
			name: "large batch",
			size: 1000,
		},
		{
			name: "zero",
			size: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithOutboxBatchSize(tt.size)(cfg)
			assert.Equal(t, tt.size, cfg.outboxBatchSize)
		})
	}
}

func TestWithBrokerURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			name: "knative broker",
			url:  "http://broker-ingress.knative-eventing.svc.cluster.local/default/default",
		},
		{
			name: "local broker",
			url:  "http://localhost:8080/events",
		},
		{
			name: "empty string",
			url:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithBrokerURL(tt.url)(cfg)
			assert.Equal(t, tt.url, cfg.brokerURL)
		})
	}
}

func TestWithStaleLockInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "30 seconds",
			interval: 30 * time.Second,
		},
		{
			name:     "1 minute",
			interval: 1 * time.Minute,
		},
		{
			name:     "5 minutes",
			interval: 5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithStaleLockInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.staleLockInterval)
		})
	}
}

func TestWithStaleLockTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{
			name:    "1 minute",
			timeout: 1 * time.Minute,
		},
		{
			name:    "5 minutes",
			timeout: 5 * time.Minute,
		},
		{
			name:    "10 minutes",
			timeout: 10 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithStaleLockTimeout(tt.timeout)(cfg)
			assert.Equal(t, tt.timeout, cfg.staleLockTimeout)
		})
	}
}

func TestWithTimerCheckInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "5 seconds",
			interval: 5 * time.Second,
		},
		{
			name:     "10 seconds",
			interval: 10 * time.Second,
		},
		{
			name:     "1 minute",
			interval: 1 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithTimerCheckInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.timerCheckInterval)
		})
	}
}

func TestWithEventTimeoutInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "15 seconds",
			interval: 15 * time.Second,
		},
		{
			name:     "30 seconds",
			interval: 30 * time.Second,
		},
		{
			name:     "1 minute",
			interval: 1 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithEventTimeoutInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.eventTimeoutInterval)
		})
	}
}

func TestWithMessageCheckInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "1 second",
			interval: 1 * time.Second,
		},
		{
			name:     "5 seconds",
			interval: 5 * time.Second,
		},
		{
			name:     "10 seconds",
			interval: 10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithMessageCheckInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.messageCheckInterval)
		})
	}
}

func TestWithRecurCheckInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "1 second",
			interval: 1 * time.Second,
		},
		{
			name:     "5 seconds",
			interval: 5 * time.Second,
		},
		{
			name:     "10 seconds",
			interval: 10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithRecurCheckInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.recurCheckInterval)
		})
	}
}

func TestWithChannelCleanupInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "1 minute",
			interval: 1 * time.Minute,
		},
		{
			name:     "5 minutes",
			interval: 5 * time.Minute,
		},
		{
			name:     "1 hour",
			interval: 1 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithChannelCleanupInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.channelCleanupInterval)
		})
	}
}

func TestWithChannelMessageRetention(t *testing.T) {
	tests := []struct {
		name      string
		retention time.Duration
	}{
		{
			name:      "1 hour",
			retention: 1 * time.Hour,
		},
		{
			name:      "24 hours",
			retention: 24 * time.Hour,
		},
		{
			name:      "7 days",
			retention: 7 * 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithChannelMessageRetention(tt.retention)(cfg)
			assert.Equal(t, tt.retention, cfg.channelMessageRetention)
		})
	}
}

func TestWithWorkflowResumptionInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{
			name:     "500 milliseconds",
			interval: 500 * time.Millisecond,
		},
		{
			name:     "1 second",
			interval: 1 * time.Second,
		},
		{
			name:     "5 seconds",
			interval: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithWorkflowResumptionInterval(tt.interval)(cfg)
			assert.Equal(t, tt.interval, cfg.workflowResumptionInterval)
		})
	}
}

func TestWithMaxConcurrentResumptions(t *testing.T) {
	tests := []struct {
		name         string
		n            int
		wantN        int
		shouldChange bool
	}{
		{
			name:         "positive value",
			n:            20,
			wantN:        20,
			shouldChange: true,
		},
		{
			name:         "value of 1",
			n:            1,
			wantN:        1,
			shouldChange: true,
		},
		{
			name:         "zero value ignored",
			n:            0,
			wantN:        10, // default
			shouldChange: false,
		},
		{
			name:         "negative value ignored",
			n:            -5,
			wantN:        10, // default
			shouldChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithMaxConcurrentResumptions(tt.n)(cfg)
			assert.Equal(t, tt.wantN, cfg.maxConcurrentResumptions)
		})
	}
}

func TestWithMaxConcurrentTimers(t *testing.T) {
	tests := []struct {
		name         string
		n            int
		wantN        int
		shouldChange bool
	}{
		{
			name:         "positive value",
			n:            15,
			wantN:        15,
			shouldChange: true,
		},
		{
			name:         "value of 1",
			n:            1,
			wantN:        1,
			shouldChange: true,
		},
		{
			name:         "zero value ignored",
			n:            0,
			wantN:        10, // default
			shouldChange: false,
		},
		{
			name:         "negative value ignored",
			n:            -3,
			wantN:        10, // default
			shouldChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithMaxConcurrentTimers(tt.n)(cfg)
			assert.Equal(t, tt.wantN, cfg.maxConcurrentTimers)
		})
	}
}

func TestWithMaxConcurrentMessages(t *testing.T) {
	tests := []struct {
		name         string
		n            int
		wantN        int
		shouldChange bool
	}{
		{
			name:         "positive value",
			n:            25,
			wantN:        25,
			shouldChange: true,
		},
		{
			name:         "value of 1",
			n:            1,
			wantN:        1,
			shouldChange: true,
		},
		{
			name:         "zero value ignored",
			n:            0,
			wantN:        10, // default
			shouldChange: false,
		},
		{
			name:         "negative value ignored",
			n:            -10,
			wantN:        10, // default
			shouldChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithMaxConcurrentMessages(tt.n)(cfg)
			assert.Equal(t, tt.wantN, cfg.maxConcurrentMessages)
		})
	}
}

func TestWithSingletonStaleLockCleanup(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "enabled",
			enabled: true,
		},
		{
			name:    "disabled",
			enabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithSingletonStaleLockCleanup(tt.enabled)(cfg)
			assert.Equal(t, tt.enabled, cfg.singletonStaleLockCleanup)
		})
	}
}

func TestWithSingletonChannelCleanup(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{
			name:    "enabled",
			enabled: true,
		},
		{
			name:    "disabled",
			enabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithSingletonChannelCleanup(tt.enabled)(cfg)
			assert.Equal(t, tt.enabled, cfg.singletonChannelCleanup)
		})
	}
}

func TestWithShutdownTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{
			name:    "10 seconds",
			timeout: 10 * time.Second,
		},
		{
			name:    "30 seconds",
			timeout: 30 * time.Second,
		},
		{
			name:    "1 minute",
			timeout: 1 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithShutdownTimeout(tt.timeout)(cfg)
			assert.Equal(t, tt.timeout, cfg.shutdownTimeout)
		})
	}
}

type mockHooks struct {
	hooks.NoOpHooks
}

func TestWithHooks(t *testing.T) {
	tests := []struct {
		name  string
		hooks hooks.WorkflowHooks
	}{
		{
			name:  "NoOpHooks",
			hooks: &hooks.NoOpHooks{},
		},
		{
			name:  "custom hooks",
			hooks: &mockHooks{},
		},
		{
			name:  "nil hooks",
			hooks: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			WithHooks(tt.hooks)(cfg)
			assert.Equal(t, tt.hooks, cfg.hooks)
		})
	}
}

func TestOptionChaining(t *testing.T) {
	tests := []struct {
		name    string
		options []Option
		check   func(t *testing.T, cfg *appConfig)
	}{
		{
			name: "database and worker",
			options: []Option{
				WithDatabase("postgres://localhost/test"),
				WithWorkerID("worker-1"),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.Equal(t, "postgres://localhost/test", cfg.databaseURL)
				assert.Equal(t, "worker-1", cfg.workerID)
			},
		},
		{
			name: "outbox configuration",
			options: []Option{
				WithOutbox(true),
				WithBrokerURL("http://broker:8080"),
				WithOutboxInterval(2 * time.Second),
				WithOutboxBatchSize(50),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.True(t, cfg.outboxEnabled)
				assert.Equal(t, "http://broker:8080", cfg.brokerURL)
				assert.Equal(t, 2*time.Second, cfg.outboxInterval)
				assert.Equal(t, 50, cfg.outboxBatchSize)
			},
		},
		{
			name: "timing configuration",
			options: []Option{
				WithStaleLockInterval(2 * time.Minute),
				WithStaleLockTimeout(10 * time.Minute),
				WithTimerCheckInterval(5 * time.Second),
				WithEventTimeoutInterval(1 * time.Minute),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.Equal(t, 2*time.Minute, cfg.staleLockInterval)
				assert.Equal(t, 10*time.Minute, cfg.staleLockTimeout)
				assert.Equal(t, 5*time.Second, cfg.timerCheckInterval)
				assert.Equal(t, 1*time.Minute, cfg.eventTimeoutInterval)
			},
		},
		{
			name: "concurrency configuration",
			options: []Option{
				WithMaxConcurrentResumptions(20),
				WithMaxConcurrentTimers(15),
				WithMaxConcurrentMessages(25),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.Equal(t, 20, cfg.maxConcurrentResumptions)
				assert.Equal(t, 15, cfg.maxConcurrentTimers)
				assert.Equal(t, 25, cfg.maxConcurrentMessages)
			},
		},
		{
			name: "singleton configuration",
			options: []Option{
				WithSingletonStaleLockCleanup(false),
				WithSingletonChannelCleanup(false),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.False(t, cfg.singletonStaleLockCleanup)
				assert.False(t, cfg.singletonChannelCleanup)
			},
		},
		{
			name: "full configuration",
			options: []Option{
				WithDatabase("mysql://localhost/db"),
				WithAutoMigrate(false),
				WithServiceName("test-service"),
				WithWorkerID("worker-test"),
				WithOutbox(true),
				WithBrokerURL("http://broker"),
				WithShutdownTimeout(1 * time.Minute),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.Equal(t, "mysql://localhost/db", cfg.databaseURL)
				assert.False(t, cfg.autoMigrate)
				assert.Equal(t, "test-service", cfg.serviceName)
				assert.Equal(t, "worker-test", cfg.workerID)
				assert.True(t, cfg.outboxEnabled)
				assert.Equal(t, "http://broker", cfg.brokerURL)
				assert.Equal(t, 1*time.Minute, cfg.shutdownTimeout)
			},
		},
		{
			name: "override same option",
			options: []Option{
				WithDatabase("first.db"),
				WithDatabase("second.db"),
				WithDatabase("final.db"),
			},
			check: func(t *testing.T, cfg *appConfig) {
				assert.Equal(t, "final.db", cfg.databaseURL)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			for _, opt := range tt.options {
				opt(cfg)
			}
			tt.check(t, cfg)
		})
	}
}
