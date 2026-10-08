//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func changesWatchOutputFile(stdout *os.File) (*os.File, func(), error) {
	fd := stdout.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		return nil, nil, err
	}
	duplicate, err := unix.Dup(int(fd))
	if err != nil {
		return nil, nil, err
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
