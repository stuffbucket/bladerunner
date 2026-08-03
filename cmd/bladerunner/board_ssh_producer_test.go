package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/ui/board"
)

// This file holds the claim made at holderattach.go:371 — "Connect is driven by
// the console tailer (it is the ssh stage the board already parses)".
//
// AGENTS.md section 5.7 requires a test for a comment that makes a claim about
// a different component, because such a claim becomes wrong in silence. This
// particular claim went the other way: it is TRUE, and was believed false for
// most of a day precisely because nothing held it. A test that merely grepped
// for the comment text would have passed throughout that misreading, so these
// assert the behavior instead — that the console tailer really does produce the
// board's SSH stage, and that the boot-stage file really does not.

// consoleSSHReadyLine is a guest sshd startup line of the shape
// internal/boot.patternSSHReady matches (`sshd.*listening`).
const consoleSSHReadyLine = "[   12.345678] sshd[712]: Server listening on 0.0.0.0 port 22."

// boardSettleBudget bounds the wait for the tailer to notice an appended line.
// The tailer re-stats every consoleTailPollInterval, so this is many polls. It
// is generous because this suite also runs under emulation in the Linux
// container, where a tight budget would flake rather than fail honestly.
const boardSettleBudget = 15 * time.Second

// appendRetryInterval is how often the SSH marker is re-appended while waiting.
const appendRetryInterval = 50 * time.Millisecond

// testBoard builds the REAL boot board, forced interactive so stage
// transitions are recorded rather than only logged, and writing nowhere.
func testBoard() *board.Board {
	interactive := true
	return board.New(bootBoardStages(), board.Options{
		Out:         io.Discard,
		Interactive: &interactive,
	})
}

// stageLine returns the rendered line for the stage carrying label, or "".
func stageLine(brd *board.Board, label string) string {
	for _, line := range strings.Split(brd.RenderFrame(), "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}

// stageDone reports whether the stage carrying label renders as complete.
func stageDone(brd *board.Board, label string) bool {
	return strings.Contains(stageLine(brd, label), "✓")
}

// appendLine adds one line to an existing console log, the way a running guest
// does. The tailer opens FromEnd, so a marker only counts if it arrives after
// the tailer is watching.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open console log: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append console line: %v", err)
	}
}

// The console tailer is the producer for the board's SSH stage: real guest
// console text, parsed by internal/boot, advances it. This is what makes a
// separate bootstage.Connect unnecessary for the board.
func TestConsoleTailerDrivesTheBoardsSSHStage(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "console.log")
	if err := os.WriteFile(logPath, []byte("[    0.000000] Linux version 6.1.0\n"), 0o600); err != nil {
		t.Fatalf("seed console log: %v", err)
	}

	brd := testBoard()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailConsoleIntoBoard(ctx, brd, logPath)

	if stageDone(brd, "SSH ready") {
		t.Fatal("the SSH stage was already complete before any sshd line; the test cannot detect a producer")
	}

	// The tailer opens the log FromEnd, so a line written before its first open
	// is seeked past and never seen. Rather than race the goroutine's startup,
	// re-append until the stage advances: the producer is idempotent (it guards
	// on seenSSH) and a broken producer still never completes, so this cannot
	// paper over a real failure — the mutation check on this test confirms it.
	deadline := time.Now().Add(boardSettleBudget)
	for !stageDone(brd, "SSH ready") {
		if time.Now().After(deadline) {
			t.Fatalf("the console tailer did not complete the SSH stage after %v; line was %q",
				boardSettleBudget, stageLine(brd, "SSH ready"))
		}
		appendLine(t, logPath, consoleSSHReadyLine)
		time.Sleep(appendRetryInterval)
	}
}

// The other half of the same claim: the boot-stage file does NOT drive the SSH
// stage, which is why bootstage.Connect has no publisher and needs none. The
// attachment replays Boot/Setup/Incus/Ready onto the runner-fed stages only.
//
// If a future change ever teaches the attachment to drive the SSH stage from
// the file, this fails and the comment at holderattach.go:371 must be revisited
// along with it.
func TestBootStageFileDoesNotDriveTheBoardsSSHStage(t *testing.T) {
	dir := t.TempDir()
	brd := testBoard()
	a := &holderAttachment{spawnedAt: time.Now().Add(-time.Minute)}

	// A boot that has run the whole published vocabulary through to Ready.
	for _, stage := range []bootstage.Stage{bootstage.Boot, bootstage.Setup, bootstage.Incus, bootstage.Ready} {
		if err := bootstage.Write(dir, stage, time.Now()); err != nil {
			t.Fatalf("publish %q: %v", stage, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), boardSettleBudget)
	defer cancel()
	a.driveBoardFromBootStage(ctx, brd, dir)

	// Ready is terminal, so the replay returned on its own having driven every
	// stage the file can drive. Those must be the runner-fed ones only.
	if !stageDone(brd, "VM running") {
		t.Error("the replay did not complete the VM stage; the file should drive that one")
	}
	if !stageDone(brd, "Incus API ready") {
		t.Error("the replay did not complete the Incus stage; the file should drive that one")
	}
	if stageDone(brd, "SSH ready") {
		t.Error("the boot-stage file drove the SSH stage; holderattach.go:371 says the console tailer owns it")
	}
}
