//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func changesWatchOutputFile(stdout *os.File) (*os.File, func(), error) {
	raw, err := stdout.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var fd uintptr
	var flags, duplicate int
	var operationErr error
	// Fd() can itself switch a Go-owned descriptor to blocking mode. Control
	// observes and duplicates it without changing the caller's poller or flags.
	err = raw.Control(func(descriptor uintptr) {
		fd = descriptor
		flags, operationErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if operationErr == nil {
			duplicate, operationErr = unix.Dup(int(fd))
		}
	})
	if err != nil {
		return nil, nil, err
	}
	if operationErr != nil {
		return nil, nil, operationErr
	}
	unix.CloseOnExec(duplicate)
	if err = unix.SetNonblock(duplicate, true); err != nil {
		_ = unix.Close(duplicate)
		return nil, nil, err
	}
	// Inherited stdout is usually blocking and cannot use Go write deadlines.
	// A nonblocking duplicate is registered with Go's poller; restore the shared
	// descriptor flags after all writes/cancellation callbacks have finished.
	file := os.NewFile(uintptr(duplicate), stdout.Name())
	return file, func() {
		_ = file.Close()
		_, _ = unix.FcntlInt(fd, unix.F_SETFL, flags)
	}, nil
}
