package util

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// A lock here is an flock(2) claim, which is the one lock design this repository
// has (AGENTS.md section 3). The kernel drops it when the holder dies, however
// it died, so a crash leaves a stale lock FILE -- harmless, reused in place --
// but never a stale LOCK. That property is why the alternative, a PID file or a
// lock directory, is not used: both need a liveness check that flock gets from
// the kernel for free.
//
// This claim BLOCKS. It guards critical sections whose competing callers all
// want the same end state and only have to wait their turn. A caller that must
// REFUSE when the resource is already held wants a non-blocking claim instead --
// internal/cartridge's boot claim is that case, because a second VM booting the
// same disk would corrupt the first one's data, and waiting is not an answer.

// FileLock is a held exclusive flock. The zero value is not usable; obtain one
// from AcquireLock.
type FileLock struct {
	file *os.File
}

// AcquireLock takes an exclusive, blocking flock on path, creating the lock file
// with perm if it does not exist. The caller must call Release.
//
// Locks taken through this function MUST NOT nest. flock is held per open file
// description, so a second AcquireLock on the same path from the same goroutine
// opens a second description and deadlocks against the first.
func AcquireLock(path string, perm os.FileMode) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, perm)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return &FileLock{file: f}, nil
		}
		// Go's runtime preempts goroutines with signals, so a blocking syscall
		// can return EINTR with the lock not taken. Retry rather than report a
		// failure that did not happen.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
}

// Release drops the claim. Closing the descriptor is what releases the kernel
// lock. The lock FILE is left in place on purpose: unlinking it would let
// another process create and lock a different inode for the same path while a
// live holder still believes it holds the claim.
func (l *FileLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}
