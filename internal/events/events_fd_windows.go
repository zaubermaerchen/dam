//go:build windows

package events

// This file duplicates Windows event handles so dam never owns the caller's
// descriptor. Named-pipe handles are switched to NOWAIT only during writes.

import (
	"os"
	"syscall"
	"unsafe"
)

const pipeNowait = 1

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	getNamedPipeHandleStateProcedure = kernel32.NewProc("GetNamedPipeHandleStateW")
	setNamedPipeHandleStateProcedure = kernel32.NewProc("SetNamedPipeHandleState")
)

type windowsEventFD struct {
	file        *os.File
	handle      syscall.Handle
	pipe        bool
	getPipeMode func() (uint32, error)
	setPipeMode func(uint32) error
	writeData   func([]byte) (int, error)
}

// Supported reports whether this target has an event-FD transport.
func Supported() bool { return true }

func openEventFD(fd int) (eventFD, error) {
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
	if fileType, typeErr := syscall.GetFileType(duplicate); typeErr == nil && fileType == syscall.FILE_TYPE_PIPE {
		if _, err := getWindowsPipeMode(duplicate); err != nil {
			event.Close()
			return nil, err
		}
		event.pipe = true
		event.getPipeMode = func() (uint32, error) {
			return getWindowsPipeMode(duplicate)
		}
		event.setPipeMode = func(mode uint32) error {
			return setWindowsPipeMode(duplicate, &mode)
		}
	}
	return event, nil
}

func (fd *windowsEventFD) Write(data []byte) (written int, err error) {
	if !fd.pipe {
		return fd.write(data)
	}
	getMode := fd.getPipeMode
	if getMode == nil {
		getMode = func() (uint32, error) {
			return getWindowsPipeMode(fd.handle)
		}
	}
	setMode := fd.setPipeMode
	if setMode == nil {
		setMode = func(mode uint32) error {
			return setWindowsPipeMode(fd.handle, &mode)
		}
	}
	currentMode, err := getMode()
	if err != nil {
		return 0, err
	}
	nowait := currentMode | pipeNowait
	if err := setMode(nowait); err != nil {
		_ = setMode(currentMode)
		return 0, err
	}
	defer func() {
		if restoreErr := setMode(currentMode); err == nil && restoreErr != nil {
			err = restoreErr
		}
	}()
	return fd.write(data)
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

func setWindowsPipeMode(handle syscall.Handle, mode *uint32) error {
	result, _, callErr := setNamedPipeHandleStateProcedure.Call(uintptr(handle), uintptr(unsafe.Pointer(mode)), 0, 0)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return syscall.EINVAL
	}
	return nil
}
