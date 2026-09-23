//go:build windows

package events

// This file accepts only verifiably NOWAIT Windows pipes and duplicates their
// handles without changing the caller's pipe mode.

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	pipeNowait    = 1
	fileWriteData = 0x0002
)

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	getNamedPipeHandleStateProcedure = kernel32.NewProc("GetNamedPipeHandleStateW")
)

type windowsEventFD struct {
	file        *os.File
	handle      syscall.Handle
	getPipeMode func() (uint32, error)
	writeData   func([]byte) (int, error)
}

// Supported reports whether this target has an event-FD transport.
func Supported() bool { return true }

func openEventFD(fd int) (eventFD, error) {
	if err := validateWindowsEventFD(syscall.Handle(fd)); err != nil {
		return nil, err
	}
	if err := checkWindowsPipeWritable(syscall.Handle(fd)); err != nil {
		return nil, err
	}
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, err
	}
	var duplicate syscall.Handle
	if err := syscall.DuplicateHandle(process, syscall.Handle(fd), process, &duplicate, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	event := &windowsEventFD{file: os.NewFile(uintptr(duplicate), "dam events"), handle: duplicate}
	event.writeData = func(data []byte) (int, error) {
		return event.file.Write(data)
	}
	return event, nil
}

// Duplicating with FILE_WRITE_DATA tests the handle's granted rights without
// sending a byte into the pipe; the narrow duplicate is closed immediately.
func checkWindowsPipeWritable(handle syscall.Handle) error {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	var duplicate syscall.Handle
	if err := syscall.DuplicateHandle(process, handle, process, &duplicate, fileWriteData, false, 0); err != nil {
		return err
	}
	return syscall.CloseHandle(duplicate)
}

func (fd *windowsEventFD) Write(data []byte) (written int, err error) {
	getMode := fd.getPipeMode
	if getMode == nil {
		if err := validateWindowsEventFD(fd.handle); err != nil {
			return 0, err
		}
	} else {
		mode, err := getMode()
		if err != nil {
			return 0, err
		}
		if mode&pipeNowait == 0 {
			return 0, syscall.EINVAL
		}
	}
	return fd.write(data)
}

func validateWindowsEventFD(handle syscall.Handle) error {
	fileType, err := syscall.GetFileType(handle)
	if err != nil {
		return err
	}
	if fileType != syscall.FILE_TYPE_PIPE {
		return syscall.EINVAL
	}
	mode, err := getWindowsPipeMode(handle)
	if err != nil {
		return err
	}
	if mode&pipeNowait == 0 {
		return syscall.EINVAL
	}
	return nil
}

func (fd *windowsEventFD) write(data []byte) (int, error) {
	if fd.writeData != nil {
		return fd.writeData(data)
	}
	return fd.file.Write(data)
}

func (fd *windowsEventFD) Close() error {
	if fd == nil || fd.file == nil {
		return nil
	}
	err := fd.file.Close()
	fd.file = nil
	return err
}

func getWindowsPipeMode(handle syscall.Handle) (uint32, error) {
	var mode uint32
	result, _, callErr := getNamedPipeHandleStateProcedure.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, syscall.EINVAL
	}
	return mode, nil
}
