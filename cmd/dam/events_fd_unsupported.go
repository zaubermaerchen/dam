//go:build !(aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows)

package main

import "fmt"

// Unsupported targets retain the CLI option for a consistent parser, but
// report setup failure through the normal observation-plane warning path.
func eventFDSupported() bool { return false }

func openEventFD(fd int) (eventFD, error) {
	return nil, fmt.Errorf("event fd is unsupported on this platform")
}
