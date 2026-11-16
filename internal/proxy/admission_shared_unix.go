//go:build !windows

package proxy

import (
	"errors"
	"os"
	"syscall"
)

func lockAdmissionFile(file *os.File, exclusive, nonblocking bool) (bool, error) {
	flags := syscall.LOCK_SH
	if exclusive {
		flags = syscall.LOCK_EX
	}
	if nonblocking {
		flags |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(file.Fd()), flags)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
