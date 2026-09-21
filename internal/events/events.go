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
	mu          sync.Mutex
	writer      eventFD
	diagnostics io.Writer
	disabled    bool
	dispatched  bool
}

// New opens a duplicate of fd for event output. The caller retains ownership
// of fd; setup failures are reported once through diagnostics and disable the
// optional observation stream.
func New(fd *int, diagnostics io.Writer) *Sink {
	if fd == nil {
		return nil
	}
	sink := &Sink{diagnostics: diagnostics}
	writer, err := openEventFD(*fd)
	if err != nil {
		sink.disableLocked(err)
		return sink
	}
	sink.writer = writer
	return sink
}

// EmitOpen reports both externally visible OPEN transitions in order. Writes
// are nonblocking so the observation sink cannot stall the data plane.
func (sink *Sink) EmitOpen() {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.disabled || sink.dispatched {
		return
	}
	sink.dispatched = true
	if !sink.writeEventLocked("release-selected") {
		return
	}
	if !sink.writeEventLocked("stream-open") {
		return
	}
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
		_, _ = fmt.Fprintf(sink.diagnostics, "events disabled: %v\n", err)
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
