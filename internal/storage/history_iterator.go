package storage

import (
	"context"
)

// HistoryIteratorOptions configures the history iterator.
type HistoryIteratorOptions struct {
	// BatchSize is the number of events to fetch per batch.
	// Default: 1000
	BatchSize int
}

// HistoryIterator provides streaming access to workflow history events.
// This avoids loading the entire history into memory at once.
type HistoryIterator struct {
	storage    Storage
	ctx        context.Context
	instanceID string
	batchSize  int
	buffer     []*HistoryEvent
	bufferIdx  int
	lastID     int64
	done       bool
	err        error
}

// NewHistoryIterator creates a new history iterator.
func NewHistoryIterator(ctx context.Context, storage Storage, instanceID string, opts *HistoryIteratorOptions) *HistoryIterator {
	batchSize := 1000
	if opts != nil && opts.BatchSize > 0 {
		batchSize = opts.BatchSize
	}

	return &HistoryIterator{
		storage:    storage,
		ctx:        ctx,
		instanceID: instanceID,
		batchSize:  batchSize,
		buffer:     nil,
		bufferIdx:  0,
		lastID:     0,
		done:       false,
		err:        nil,
	}
}

// Next returns the next history event.
// Returns (event, true) if an event is available, (nil, false) if no more events.
// Check Err() after iteration to see if an error occurred.
func (it *HistoryIterator) Next() (*HistoryEvent, bool) {
	if it.done || it.err != nil {
		return nil, false
	}

	// Check if we need to fetch more events
	if it.buffer == nil || it.bufferIdx >= len(it.buffer) {
		if err := it.fetchBatch(); err != nil {
			it.err = err
			return nil, false
		}
		if len(it.buffer) == 0 {
			it.done = true
			return nil, false
		}
	}

	// Return next event from buffer
	event := it.buffer[it.bufferIdx]
	it.bufferIdx++
	it.lastID = event.ID

	return event, true
}

// fetchBatch fetches the next batch of events from storage.
func (it *HistoryIterator) fetchBatch() error {
	events, hasMore, err := it.storage.GetHistoryPaginated(it.ctx, it.instanceID, it.lastID, it.batchSize)
	if err != nil {
		return err
	}

	it.buffer = events
	it.bufferIdx = 0

	if !hasMore && len(events) < it.batchSize {
		// No more events after this batch
		if len(events) == 0 {
			it.done = true
		}
	}

	return nil
}

// Err returns any error that occurred during iteration.
func (it *HistoryIterator) Err() error {
	return it.err
}

// Close releases any resources held by the iterator.
// Currently a no-op, but provided for future compatibility.
func (it *HistoryIterator) Close() error {
	it.done = true
	it.buffer = nil
	return nil
}

// Collect reads all remaining events into a slice.
// Use with caution for large histories - prefer iterating with Next().
func (it *HistoryIterator) Collect() ([]*HistoryEvent, error) {
	var all []*HistoryEvent
	for {
		event, ok := it.Next()
		if !ok {
			break
		}
		all = append(all, event)
	}
	if it.err != nil {
		return nil, it.err
	}
	return all, nil
}
