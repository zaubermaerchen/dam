//go:build windows

package main

// This file verifies that Windows pipe mode changes are scoped to one write.

import (
	"errors"
	"testing"
)

func TestWindowsEventFDWriteRestoresCurrentPipeMode(t *testing.T) {
	currentMode := uint32(0x200 | 0x400)
	var modes []uint32
	fd := &windowsEventFD{
		pipe: true,
		getPipeMode: func() (uint32, error) {
			return currentMode, nil
		},
		setPipeMode: func(mode uint32) error {
			modes = append(modes, mode)
			return nil
		},
		writeData: func(data []byte) (int, error) {
			return len(data), nil
		},
	}

	if n, err := fd.Write([]byte("event")); err != nil || n != len("event") {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len("event"))
	}
	if got, want := modes, []uint32{currentMode | pipeNowait, currentMode}; !equalUint32s(got, want) {
		t.Fatalf("pipe mode writes = %#v, want %#v", got, want)
	}
}

func TestWindowsEventFDWriteRestoresCurrentPipeModeAfterPipeWouldBlock(t *testing.T) {
	currentMode := uint32(0x200 | 0x400)
	var modes []uint32
	wouldBlock := errors.New("pipe would block")
	fd := &windowsEventFD{
		pipe: true,
		getPipeMode: func() (uint32, error) {
			return currentMode, nil
		},
		setPipeMode: func(mode uint32) error {
			modes = append(modes, mode)
			return nil
		},
		writeData: func([]byte) (int, error) {
			return 0, wouldBlock
		},
	}

	if _, err := fd.Write([]byte("event")); !errors.Is(err, wouldBlock) {
		t.Fatalf("Write error = %v, want %v", err, wouldBlock)
	}
	if got, want := modes, []uint32{currentMode | pipeNowait, currentMode}; !equalUint32s(got, want) {
		t.Fatalf("pipe mode writes after would-block = %#v, want %#v", got, want)
	}
}

func equalUint32s(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
