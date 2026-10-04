//go:build !windows

package app

import (
	"os"
	"syscall"
)

func outboundReadFlags() int { return os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK }
