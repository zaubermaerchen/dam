package events

// Package events writes dam's optional JSONL release lifecycle observations.

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type eventFD interface {
	Write([]byte) (int, error)
	Close() error
}

// Sink emits release-selected and stream-open exactly once, while isolating
// transport failures from the primary stdin/stdout data path.
type Sink struct {
	mu              sync.Mutex
	writer          eventFD
	diagnostics     io.Writer
	disabled        bool
	releaseSelected bool
	streamOpen      bool
	warningDone     chan struct{}
	warningWait     sync.Once
}

// New opens a duplicate of fd for event output. The caller retains ownership
// of fd; an unusable descriptor is a configuration error before input starts.
func New(fd *int, diagnostics io.Writer) (*Sink, error) {
	if fd == nil {
		return nil, nil
	}
	writer, err := openEventFD(*fd)
	if err != nil {
		return nil, fmt.Errorf("invalid --events-fd %d: %w", *fd, err)
	}
	return &Sink{writer: writer, diagnostics: diagnostics}, nil
}

// EmitReleaseSelected reports the condition root selection. Writes are
// nonblocking so the observation sink cannot stall the data plane.
func (sink *Sink) EmitReleaseSelected() {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.disabled || sink.releaseSelected {
		return
	}
	sink.releaseSelected = true
	sink.writeEventLocked("release-selected")
}

// EmitStreamOpen reports the runtime gate's OPEN commit after
// EmitReleaseSelected. A disabled sink remains harmless to the data plane.
func (sink *Sink) EmitStreamOpen() {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.disabled || sink.streamOpen {
		return
	}
	sink.streamOpen = true
	sink.writeEventLocked("stream-open")
}

func (sink *Sink) writeEventLocked(name string) bool {
	if sink.writer == nil || sink.disabled {
		return false
	}
	record := struct {
		Event     string `json:"event"`
		Timestamp string `json:"timestamp"`
	}{
		Event:     name,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(record)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = sink.writer.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		sink.disableLocked(err)
		return false
	}
	return true
}

func (sink *Sink) disableLocked(err error) {
	if sink.disabled {
		return
	}
	sink.disabled = true
	if sink.writer != nil {
		_ = sink.writer.Close()
		sink.writer = nil
	}
	if err != nil && sink.diagnostics != nil {
		// A full or blocked stderr must not hold the gate's event lock or the
		// primary data path. Warning delivery is best effort.
		diagnostics := sink.diagnostics
		warning := fmt.Sprintf("events disabled: %v\n", err)
		done := make(chan struct{})
		sink.warningDone = done
		go func() {
			defer close(done)
			_, _ = io.WriteString(diagnostics, warning)
		}()
	}
}

// Close stops event output and releases the duplicate descriptor.
func (sink *Sink) Close() {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.writer != nil {
		_ = sink.writer.Close()
		sink.writer = nil
	}
	sink.disabled = true
}

// WaitWarning gives an outstanding best-effort warning one 10ms timer budget.
// Call it only at the process exit boundary: stream operations and Close must
// never wait for stderr. Delivery is not guaranteed, and repeated calls do not
// accumulate waiting time. Scheduler delays can exceed the timer budget.
func (sink *Sink) WaitWarning() {
	if sink == nil {
		return
	}
	sink.warningWait.Do(func() {
		sink.mu.Lock()
		done := sink.warningDone
		sink.mu.Unlock()
		if done == nil {
			return
		}
		select {
		case <-done:
			return
		default:
		}
		timer := time.NewTimer(10 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	})
}
