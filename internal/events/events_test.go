package events

// This file verifies event serialization and warn-once transport behavior.

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestSinkDisablesAfterWriteFailureOnce(t *testing.T) {
	diagnostics := channelDiagnostic{messages: make(chan string, 2)}
	writer := &failingEventFD{err: io.ErrClosedPipe}
	sink := &Sink{writer: writer, diagnostics: &diagnostics}
	sink.EmitOpen()
	sink.EmitOpen()

	if writer.writes != 1 {
		t.Fatalf("event writes = %d, want 1", writer.writes)
	}
	if warning := receiveWarning(t, diagnostics.messages); !strings.Contains(warning, "events disabled:") {
		t.Fatalf("warning = %q", warning)
	}
	select {
	case warning := <-diagnostics.messages:
		t.Fatalf("unexpected second warning %q", warning)
	default:
	}
}

func TestSinkWarningDoesNotBlockDataPath(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	sink := &Sink{writer: &failingEventFD{err: io.ErrClosedPipe}, diagnostics: blockingDiagnostic{started: started, unblock: unblock}}
	go func() {
		sink.EmitOpen()
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(unblock)
		t.Fatal("warning was not attempted")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		close(unblock)
		t.Fatal("blocked warning stalled event emission")
	}
	close(unblock)
}

type blockingDiagnostic struct {
	started chan<- struct{}
	unblock <-chan struct{}
}

func (writer blockingDiagnostic) Write(data []byte) (int, error) {
	close(writer.started)
	<-writer.unblock
	return len(data), nil
}

func TestSinkDisablesAfterShortWrite(t *testing.T) {
	diagnostics := channelDiagnostic{messages: make(chan string, 1)}
	writer := &failingEventFD{short: true}
	sink := &Sink{writer: writer, diagnostics: &diagnostics}
	sink.EmitOpen()

	if writer.writes != 1 {
		t.Fatalf("event writes = %d, want 1", writer.writes)
	}
	if warning := receiveWarning(t, diagnostics.messages); !strings.Contains(warning, "events disabled: short write") {
		t.Fatalf("warning = %q, want short-write warning", warning)
	}
}

type channelDiagnostic struct{ messages chan string }

func (writer *channelDiagnostic) Write(data []byte) (int, error) {
	writer.messages <- string(data)
	return len(data), nil
}

func receiveWarning(t *testing.T, messages <-chan string) string {
	t.Helper()
	select {
	case warning := <-messages:
		return warning
	case <-time.After(time.Second):
		t.Fatal("warning was not delivered")
		return ""
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
