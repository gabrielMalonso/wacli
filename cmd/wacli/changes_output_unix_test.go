//go:build !windows

package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestChangesWatchOutputPreservesInheritedDescriptorFlags(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	raw, err := w.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	flags := func() int {
		var value int
		var opErr error
		if err := raw.Control(func(fd uintptr) { value, opErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
			t.Fatal(err)
		}
		if opErr != nil {
			t.Fatal(opErr)
		}
		return value
	}
	before := flags()
	if before&unix.O_NONBLOCK == 0 {
		t.Fatal("fixture pipe is not nonblocking")
	}
	_, closeOutput, err := changesWatchOutput(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	closeOutput()
	if after := flags(); after != before {
		t.Fatalf("descriptor flags changed: before=%d after=%d", before, after)
	}
	if _, err = w.Write([]byte("usable")); err != nil {
		t.Fatal(err)
	}
}
