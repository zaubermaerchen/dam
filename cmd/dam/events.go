package main

// This file emits the two public release lifecycle events and keeps event
// transport failures isolated from the primary stdin-to-stdout data path.

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

type eventSink struct {
	mu          sync.Mutex
	writer      eventFD
	diagnostics io.Writer
	disabled    bool
	dispatched  bool
}

func newEventSink(fd *int, diagnostics io.Writer) *eventSink {
	if fd == nil {
		return nil
	}
	sink := &eventSink{diagnostics: diagnostics}
	writer, err := openEventFD(*fd)
	if err != nil {
		sink.disableLocked(err)
		return sink
	}
	sink.writer = writer
	return sink
}

// emitOpen reports both externally visible OPEN transitions while the
// coordinator still owns the transition. The event transport is nonblocking,
// so downstream readers cannot observe release before these records are
// dispatched.
func (sink *eventSink) emitOpen() {
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

func (sink *eventSink) writeEventLocked(name string) bool {
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

func (sink *eventSink) disableLocked(err error) {
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

func (sink *eventSink) Close() {
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
