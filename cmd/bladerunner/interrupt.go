package main

import (
	"context"
	"os"
	"syscall"
)

// interruptSignals are the signals that end a foreground command: the one a
// terminal raises for Ctrl-C, and the one a supervisor or a script sends.
var interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// interruptibleContext is the context a foreground command runs its work under.
// It cancels on interruptSignals and names the signal as the cancel cause; the
// returned function releases the registration and must be deferred.
//
// # Why this is not established once in main.go
//
// The obvious central fix is rootCmd.ExecuteContext with a signal-bound context,
// and it is the wrong one. Registering a handler for SIGINT is process-global:
// it takes away the runtime's default "terminate on interrupt" for the WHOLE
// program, not for the command that asked. Only a handful of verbs read
// cmd.Context() at all — watch, vmd, disk bake, cartridge and self-update — so
// every other verb would trade a Ctrl-C that works for a cancellation nothing
// consults, and would stop being interruptible altogether. `br menubar` is the
// clearest case: it hands the main thread to an AppKit run loop that never
// returns to cobra, so a canceled context there is unreachable and Ctrl-C would
// simply do nothing. That converts one uninterruptible command into all of them.
//
// The registration is therefore taken by the commands that act on it, at the
// point they begin the work it cancels, and released when that work ends.
//
// # Ctrl-C in an interactive exec
//
// Canceling on SIGINT and forwarding Ctrl-C to a guest shell look like they
// want the same keystroke. They do not collide, because the TERMINAL MODE
// decides whether that keystroke is ever a signal.
//
// With a PTY requested, configureTTY puts the local terminal in raw mode, which
// clears ISIG. The line discipline then stops mapping 0x03 to SIGINT and passes
// the byte through, so Ctrl-C travels down the stdin relay to the guest PTY and
// interrupts the guest's foreground process there. No host signal is raised, so
// the registration below has nothing to swallow.
//
// Without a PTY the terminal stays cooked, ISIG is intact and Ctrl-C is a real
// SIGINT — and there is no guest PTY to give 0x03 any meaning, so canceling
// the operation is the only thing the keystroke can sensibly do. SIGTERM
// cancels in both modes; it is never a keystroke.
//
// Canceling instead of dying is also what lets a deferred terminal restore
// run, so the local terminal is put back on the interrupted path too.
func interruptibleContext() (context.Context, context.CancelFunc) {
	return signalContext(context.Background(), interruptSignals...)
}
