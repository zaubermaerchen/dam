//go:build windows

package main

// This file verifies that Windows keeps the signal option explicit instead of
// silently accepting a release condition it cannot implement.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsSignalReleaseOnWindows(t *testing.T) {
	for _, value := range []string{
		"signal:USR1",
		"signal:USR2",
		"signal:USR1 && file:ready",
	} {
		t.Run(value, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			input := &trackingReader{}
			if status := run([]string{value}, input, &output, &diagnostics); status == 0 {
				t.Fatal("signal release unexpectedly succeeded on Windows")
			}
			if input.reads != 0 {
				t.Fatalf("unsupported signal read stdin %d times", input.reads)
			}
			if output.Len() != 0 {
				t.Fatalf("unsupported signal wrote stdout: %q", output.String())
			}
			if !strings.Contains(diagnostics.String(), "not supported") {
				t.Fatalf("diagnostic = %q, want unsupported-platform error", diagnostics.String())
			}
		})
	}
}
