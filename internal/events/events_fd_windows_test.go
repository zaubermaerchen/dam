//go:build windows

package events

// This file verifies that event writes require NOWAIT without changing pipe mode.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestWindowsEventFDWriteRequiresCurrentNowaitMode(t *testing.T) {
	writes := 0
	mode := uint32(pipeNowait)
	fd := &windowsEventFD{
		getPipeMode: func() (uint32, error) { return mode, nil },
		writeData: func(data []byte) (int, error) {
			writes++
			return len(data), nil
		},
	}
	if n, err := fd.Write([]byte("event")); err != nil || n != 5 {
		t.Fatalf("NOWAIT Write = (%d, %v), want (5, nil)", n, err)
	}
	mode = 0
	if _, err := fd.Write([]byte("event")); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("blocking Write error = %v, want EINVAL", err)
	}
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
}

func TestWindowsEventFDRejectsRegularFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "events-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if writer, err := openEventFD(int(file.Fd())); err == nil {
		writer.Close()
		t.Fatal("regular file unexpectedly accepted")
	}
}

func TestWindowsEventFDChecksPipeWriteAccessWithoutWriting(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()
	if err := checkWindowsPipeWritable(syscall.Handle(readEnd.Fd())); err == nil {
		t.Fatal("read-only pipe unexpectedly passed write-access check")
	}
	if err := checkWindowsPipeWritable(syscall.Handle(writeEnd.Fd())); err != nil {
		t.Fatalf("write pipe failed write-access check: %v", err)
	}
}

func TestWindowsNowaitPipeEmitsOrderedJSONL(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()
	fd := int(writeEnd.Fd())
	mode := uint32(pipeNowait)
	setMode := syscall.NewLazyDLL("kernel32.dll").NewProc("SetNamedPipeHandleState")
	result, _, callErr := setMode.Call(uintptr(fd), uintptr(unsafe.Pointer(&mode)), 0, 0)
	if result == 0 {
		t.Fatalf("set anonymous event pipe NOWAIT: %v", callErr)
	}
	var diagnostics bytes.Buffer
	sink, err := New(&fd, &diagnostics)
	if err != nil {
		t.Fatalf("open NOWAIT event pipe: %v", err)
	}
	sink.EmitOpen()
	sink.Close()
	currentMode, err := getWindowsPipeMode(syscall.Handle(fd))
	if err != nil || currentMode&pipeNowait == 0 {
		t.Fatalf("caller pipe mode after events = %#x, %v; want NOWAIT", currentMode, err)
	}
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("event diagnostics = %q", diagnostics.String())
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("event lines = %q, want two JSONL records", data)
	}
	for index, want := range []string{"release-selected", "stream-open"} {
		var record struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(lines[index]), &record); err != nil {
			t.Fatalf("decode event %d: %v", index, err)
		}
		if record.Event != want {
			t.Fatalf("event %d = %q, want %q", index, record.Event, want)
		}
	}
}
