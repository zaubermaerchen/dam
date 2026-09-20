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
	originalNonblock := originalFlags&syscall.O_NONBLOCK != 0
	if err := setEventFDNonblock(fd.fd, true); err != nil {
		_ = setEventFDNonblock(fd.fd, originalNonblock)
		return 0, err
	}
	defer func() {
		if restoreErr := setEventFDNonblock(fd.fd, originalNonblock); err == nil && restoreErr != nil {
			err = restoreErr
		}
	}()
	return syscall.Write(fd.fd, data)
}

// setEventFDNonblock changes only the mode bit owned by this writer. In
// particular, restoring the complete F_GETFL result would ask the kernel to
// restore status bits such as Darwin's write marker that are not settable.
func setEventFDNonblock(fd int, nonblocking bool) error {
	flags, err := eventFDFlags(fd)
	if err != nil {
		return err
	}
	if (flags&syscall.O_NONBLOCK != 0) == nonblocking {
		return nil
	}
	if nonblocking {
		flags |= syscall.O_NONBLOCK
	} else {
		flags &^= syscall.O_NONBLOCK
	}
	return setEventFDFlags(fd, flags)
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
