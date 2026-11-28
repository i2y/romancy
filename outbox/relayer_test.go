package outbox

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// syncBuffer collects what slog's default logger writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// claimBlockingStorage holds ClaimOutboxEvents until its context is
// cancelled, like a query still running when the app shuts down.
type claimBlockingStorage struct {
	*mockStorage
	entered chan struct{}
	once    sync.Once
}

func (s *claimBlockingStorage) ClaimOutboxEvents(ctx context.Context, _ time.Time, _ int) ([]*storage.OutboxEvent, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// A query that the app's shutdown cancels is not a relayer error: it is
// logged at debug level.
func TestRelayerQueryCancelledByShutdownLogsAtDebug(t *testing.T) {
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := &claimBlockingStorage{mockStorage: &mockStorage{}, entered: make(chan struct{})}
	r := NewRelayer(s, RelayerConfig{TargetURL: "http://127.0.0.1:1", PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	<-s.entered
	cancel() // the app's context, cancelled before Shutdown stops the relayer
	r.Stop()

	var errorLines, debugLines int
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, `msg="outbox relayer error"`) {
			continue
		}
		switch {
		case strings.Contains(line, "level=ERROR"):
			errorLines++
		case strings.Contains(line, "level=DEBUG") && strings.Contains(line, `error="context canceled"`):
			debugLines++
		}
	}
	if errorLines != 0 || debugLines != 1 {
		t.Fatalf("want one debug line and no error line for the cancelled query, got:\n%s", logs.String())
	}
}
