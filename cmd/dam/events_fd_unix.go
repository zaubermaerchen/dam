//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file duplicates Unix event descriptors and applies nonblocking mode
// only around each write so an observation sink cannot stall the data plane or
// leave the caller's open file description changed.

import (
	"os"
	"syscall"
)

type unixEventFD struct {
	file *os.File
	fd   int
}

func eventFDSupported() bool { return true }

func openEventFD(fd int) (eventFD, error) {
	duplicate, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(duplicate)
	return &unixEventFD{file: os.NewFile(uintptr(duplicate), "dam events"), fd: duplicate}, nil
}

func (fd *unixEventFD) Write(data []byte) (written int, err error) {
	originalFlags, err := eventFDFlags(fd.fd)
	if err != nil {
		return 0, err
	}
	if err := setEventFDFlags(fd.fd, originalFlags|syscall.O_NONBLOCK); err != nil {
		_ = setEventFDFlags(fd.fd, originalFlags)
		return 0, err
	}
	defer func() {
		if restoreErr := setEventFDFlags(fd.fd, originalFlags); err == nil && restoreErr != nil {
			err = restoreErr
		}
	}()
	return syscall.Write(fd.fd, data)
}

func (fd *unixEventFD) Close() error {
	if fd == nil || fd.file == nil {
		return nil
	}
	err := fd.file.Close()
	fd.file = nil
	return err
}

func eventFDFlags(fd int) (int, error) {
	result, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(result), nil
}

func setEventFDFlags(fd, flags int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFL), uintptr(flags))
	if errno != 0 {
		return errno
	}
	return nil
}
