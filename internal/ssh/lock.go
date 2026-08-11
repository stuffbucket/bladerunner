package ssh

import (
	"fmt"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// The ssh config tree is shared mutable state: every bladerunner instance on the
// host writes into the same ~/.config/bladerunner/ssh directory, and the writers
// are separate PROCESSES (a holder per VM), not goroutines. A sync.Mutex is
// therefore not a candidate — it would serialize the goroutines inside one
// binary and leave the interesting race, two holders starting at once,
// completely unguarded.
//
// The claim is util.AcquireLock, the repository's one lock design (AGENTS.md
// section 3). It blocks rather than failing fast, which is what this package
// wants: a second writer aims at the same end state and only has to wait its
// turn, and every critical section it guards is a few file operations long.
// internal/cartridge's boot claim is the opposite case and refuses instead.

// lockFilePerm keeps a lock file readable only by its owner, matching the rest
// of the ssh config tree. ssh refuses a config tree that is group/world
// writable, and the lock files live inside it.
const lockFilePerm = filePerm

// fileLock is a held exclusive flock on the ssh config tree.
type fileLock struct {
	inner *util.FileLock
}

// acquireLock takes an exclusive, blocking flock on path, creating the lock file
// if it does not exist. The caller must call release.
//
// The locks taken through this function MUST NOT nest; see util.AcquireLock.
// Every caller in this package takes exactly one lock and holds it across a
// short critical section.
func acquireLock(path string) (*fileLock, error) {
	l, err := util.AcquireLock(path, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("ssh config: %w", err)
	}
	return &fileLock{inner: l}, nil
}

// release drops the claim.
func (l *fileLock) release() {
	if l == nil {
		return
	}
	l.inner.Release()
}
