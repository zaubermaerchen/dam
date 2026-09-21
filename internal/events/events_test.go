package events

// This file verifies event serialization and warn-once transport behavior.

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSinkDisablesAfterWriteFailureOnce(t *testing.T) {
	var diagnostics bytes.Buffer
	writer := &failingEventFD{err: io.ErrClosedPipe}
	sink := &Sink{writer: writer, diagnostics: &diagnostics}
	sink.EmitOpen()
	sink.EmitOpen()

	if writer.writes != 1 {
		t.Fatalf("event writes = %d, want 1", writer.writes)
	}
	if got := strings.Count(diagnostics.String(), "events disabled:"); got != 1 {
		t.Fatalf("events-disabled warning count = %d, want 1: %q", got, diagnostics.String())
	}
}

func TestSinkDisablesAfterShortWrite(t *testing.T) {
	var diagnostics bytes.Buffer
	writer := &failingEventFD{short: true}
	sink := &Sink{writer: writer, diagnostics: &diagnostics}
	sink.EmitOpen()

	if writer.writes != 1 {
		t.Fatalf("event writes = %d, want 1", writer.writes)
	}
	if !strings.Contains(diagnostics.String(), "events disabled: short write") {
		t.Fatalf("diagnostics = %q, want short-write warning", diagnostics.String())
	}
}

type failingEventFD struct {
	writes int
	err    error
	short  bool
}

func (writer *failingEventFD) Write(data []byte) (int, error) {
	writer.writes++
	if writer.err != nil {
		return 0, writer.err
	}
	if writer.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func (*failingEventFD) Close() error { return nil }
