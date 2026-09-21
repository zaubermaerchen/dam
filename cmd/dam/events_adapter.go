package main

// This file keeps command-level tests independent of the platform-specific
// event transport implementation.

import "github.com/zaubermaerchen/dam/internal/events"

func eventFDSupported() bool {
	return events.Supported()
}
