//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package events

// This file accepts only already-nonblocking Unix pipes and sockets so event
// writes never change the caller's shared open file description.

import (
	"fmt"
	"os"
	"syscall"
)

type unixEventFD struct {
	file *os.File
	fd   int
}

// Supported reports whether this target has an event-FD transport.
func Supported() bool { return true }

func openEventFD(fd int) (eventFD, error) {
	if err := validateUnixEventFD(fd); err != nil {
		return nil, err
	}
	duplicate, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(duplicate)
	return &unixEventFD{file: os.NewFile(uintptr(duplicate), "dam events"), fd: duplicate}, nil
}

func (fd *unixEventFD) Write(data []byte) (written int, err error) {
	if err := validateUnixEventFD(fd.fd); err != nil {
		return 0, err
	}
	return syscall.Write(fd.fd, data)
}

func validateUnixEventFD(fd int) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return err
	}
	if kind := stat.Mode & syscall.S_IFMT; kind != syscall.S_IFIFO && kind != syscall.S_IFSOCK {
		return fmt.Errorf("event fd must be a pipe, FIFO, or socket")
	}
	flags, err := eventFDFlags(fd)
	if err != nil {
		return err
	}
	if flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return fmt.Errorf("event fd must be writable")
	}
	if flags&syscall.O_NONBLOCK == 0 {
		return fmt.Errorf("event fd must already be nonblocking")
	}
	return nil
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
