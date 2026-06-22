//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore -framework UserNotifications
#include <stdlib.h>
#include "ui_bridge_darwin.h"
*/
import "C"

import (
	"sync"
	"time"
	"unsafe"

	"github.com/stuffbucket/bladerunner/internal/bootstage"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/ui"
)

// splashMaxVisible is a safety auto-dismiss: a cold first boot can take minutes
// (image download/convert), but if boot stalls or fails the splash must never
// stick forever — the failure surfaces as a notification instead.
const splashMaxVisible = 5 * time.Minute

// cgoSplash is the real splashController: a borderless floating HUD window drawn
// by the Objective-C bridge. It is driven by the notification state machine
// (Show on Start, Hide on the first healthy edge). All AppKit work happens on
// the main thread inside the bridge (see runOnMain in ui_bridge_darwin.m).
// splashMinVisible guarantees the splash stays up long enough to be seen and to
// let the one-shot shimmer (which begins 1s in) play out, even if the VM goes
// healthy almost immediately.
const splashMinVisible = 3 * time.Second

type cgoSplash struct {
	mu       sync.Mutex
	timer    *time.Timer // safety auto-dismiss
	shownAt  time.Time
	pollStop chan struct{} // closed to stop the boot-phase poller
}

// defaultSplash returns the real cgo splash window. (Overrides the no-op stub
// declared for the pure-Go notification PR.)
func defaultSplash() splashController { return &cgoSplash{} }

func (s *cgoSplash) Show() {
	// The splash renders the CLI "bladerunner" figlet banner with a shimmer; the
	// bridge copies the string synchronously, so freeing right after is safe.
	cbanner := C.CString(ui.BannerPlain())
	C.brShowSplash(cbanner)
	C.free(unsafe.Pointer(cbanner))

	// (Re)arm the safety auto-dismiss so a failed boot can't leave it stuck, and
	// (re)start the boot-phase poller that feeds the live status line.
	s.mu.Lock()
	s.shownAt = time.Now()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(splashMaxVisible, func() { C.brHideSplash() })
	if s.pollStop != nil {
		close(s.pollStop)
	}
	stop := make(chan struct{})
	s.pollStop = stop
	s.mu.Unlock()
	go s.pollBootStage(stop)
}

// SetStatus updates the splash phase line (e.g. "Starting Incus…").
func (s *cgoSplash) SetStatus(msg string) {
	cmsg := C.CString(msg)
	C.brSetSplashStatus(cmsg)
	C.free(unsafe.Pointer(cmsg))
}

// pollBootStage reads the live boot phase published by `br start` and pushes
// the friendly message onto the splash, so its caption tracks the real boot
// (Booting Linux… -> Setting up… -> Starting Incus…) instead of a static line.
// Returns when stop is closed (Hide) or the channel is replaced (a new Show).
func (s *cgoSplash) pollBootStage(stop <-chan struct{}) {
	stateDir := config.DefaultStateDir()
	last := ""
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			st, ok := bootstage.Read(stateDir)
			// Ignore a stale file left by a previous run that wasn't cleared.
			if !ok || time.Since(st.UpdatedAt) > 2*time.Minute {
				continue
			}
			if msg := bootstage.Message(st.Stage); msg != last {
				last = msg
				s.SetStatus(msg)
			}
		}
	}
}

func (s *cgoSplash) Hide() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.pollStop != nil {
		close(s.pollStop)
		s.pollStop = nil
	}
	// Enforce the minimum on-screen time: if the splash has been up for less
	// than splashMinVisible, defer the actual hide so a fast boot doesn't make it
	// flash by before the shimmer plays.
	// gen tags this hide to the current show; if a new Show() lands during the
	// min-visible deferral below, shownAt changes and the stale timer no-ops
	// instead of hiding the freshly-shown splash.
	gen := s.shownAt
	remaining := splashMinVisible - time.Since(s.shownAt)
	s.mu.Unlock()
	if remaining > 0 {
		time.AfterFunc(remaining, func() {
			s.mu.Lock()
			stale := !s.shownAt.Equal(gen)
			s.mu.Unlock()
			if !stale {
				C.brHideSplash()
			}
		})
		return
	}
	C.brHideSplash()
}

// unNotifier delivers branded banners via UNUserNotificationCenter. It is
// selected by defaultNotifier ONLY when running inside the signed Bladerunner
// .app bundle; outside the bundle UN can't deliver, so a no-op is used instead.
type unNotifier struct{}

func (unNotifier) notify(title, body string) {
	ctitle := C.CString(title)
	cbody := C.CString(body)
	defer C.free(unsafe.Pointer(ctitle))
	defer C.free(unsafe.Pointer(cbody))
	C.brPostNotification(ctitle, cbody)
}

// newUNNotifier requests notification authorization once and returns the
// UN-backed notifier.
func newUNNotifier() notifier {
	C.brRequestNotificationAuth()
	return unNotifier{}
}
