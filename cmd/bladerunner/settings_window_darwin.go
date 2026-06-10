//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#include "settings_window_darwin.h"
*/
import "C"

import (
	"unsafe"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// showSettingsWindow loads the current persisted settings and shows the WKWebView
// settings form. Wired to the menubar's "Settings…" item.
func showSettingsWindow() {
	s, err := config.LoadSettings(config.DefaultStateDir())
	if err != nil {
		// An invalid file shouldn't block editing — start from defaults so the
		// user can fix it via the form.
		s = config.DefaultSettings()
	}
	chtml := C.CString(settingsFormHTML(s))
	defer C.free(unsafe.Pointer(chtml))
	C.brShowSettings(chtml)
}

// goSettingsSave receives the form JSON posted from the web view and applies it
// (parse + validate + persist via the pure-Go applySettingsForm), then either
// closes the window or shows a status line. Called on the main thread by the
// WKScriptMessageHandler.
//
//export goSettingsSave
func goSettingsSave(cjson *C.char) {
	out := applySettingsForm(C.GoString(cjson), config.DefaultStateDir(), vmHealth() != vmStopped)
	if out.Close {
		C.brCloseSettings()
		return
	}
	settingsMessage(out.Message, out.IsError)
}

// settingsMessage shows a status line in the open settings form.
func settingsMessage(msg string, isError bool) {
	c := C.CString(msg)
	defer C.free(unsafe.Pointer(c))
	flag := C.int(0)
	if isError {
		flag = C.int(1)
	}
	C.brSettingsMessage(c, flag)
}
