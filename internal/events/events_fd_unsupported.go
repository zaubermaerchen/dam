//go:build !(aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows)

package events

import "fmt"

// Supported reports whether this target has an event-FD transport.
func Supported() bool { return false }

func openEventFD(fd int) (eventFD, error) {
	return nil, fmt.Errorf("event fd is unsupported on this platform")
}
