package ssh

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// The ssh config tree is shared mutable state: every bladerunner instance on the
// host writes into the same ~/.config/bladerunner/ssh directory, and the writers
// are separate PROCESSES (a holder per VM), not goroutines. A sync.Mutex is
// therefore not a candidate — it would serialize the goroutines inside one
// binary and leave the interesting race, two holders starting at once,
// completely unguarded.
//
// flock(2) is the claim, for the reason internal/cartridge already documents
// (see the "boot claim" section of internal/cartridge/open.go): the kernel drops
// it when the holder dies, however it died, so a crash leaves a stale lock FILE
// (harmless, reused in place) but never a stale LOCK. AGENTS.md section 3 states
// there must not be a second lock design; this is the same design.
//
// It differs from the cartridge claim in one respect, deliberately: this lock
// BLOCKS instead of failing with LOCK_NB. A cartridge claim that is already
// taken means a second VM would corrupt the first one's disk, so refusing is the
// only correct answer. Here the second writer wants the same end state and only
// has to wait its turn, and every critical section it guards is a few file
// operations long.

// lockFilePerm keeps a lock file readable only by its owner, matching the rest
// of the ssh config tree. ssh refuses a config tree that is group/world
// writable, and the lock files live inside it.
const lockFilePerm = filePerm

// fileLock is a held exclusive flock. The zero value is not usable; obtain one
// from acquireLock.
type fileLock struct {
	file *os.File
}

// acquireLock takes an exclusive, blocking flock on path, creating the lock file
// if it does not exist. The caller must call release.
//
// The locks taken through this function MUST NOT nest. flock is held per open
// file description, so a second acquireLock on the same path from the same
// goroutine opens a second description and deadlocks against the first. Every
// caller in this package takes exactly one lock and holds it across a short
// critical section.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open ssh lock %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return &fileLock{file: f}, nil
		}
		// Go's runtime preempts goroutines with signals, so a blocking syscall
		// can return EINTR with the lock not taken. Retry rather than report a
		// failure that did not happen.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = f.Close()
		return nil, fmt.Errorf("lock ssh config %s: %w", path, err)
	}
}

// release drops the claim. Closing the descriptor is what releases the kernel
// lock. The lock FILE is left in place on purpose: unlinking it would let
// another process create and lock a different inode for the same path while a
// live holder still believes it holds the claim.
func (l *fileLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}
