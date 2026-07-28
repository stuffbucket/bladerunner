//go:build darwin

package main

import (
	"strings"
	"testing"
)

// TestInfoPlistDeclaresTCCPurposeStrings pins the two TCC purpose strings the
// flagship cartridge story depends on.
//
// A cartridge normally arrives in ~/Downloads (AirDrop) and is opened as a
// mounted volume. Under macOS TCC, a LaunchAgent that reads either without a
// declared purpose string is DENIED — and denied silently, because the system
// never shows a prompt for a bundle that did not ask. The keys have to be in
// the bundle's Info.plist before the first access, so this is the only place
// they can come from.
//
// infoPlist is the single producer of the bundle's Info.plist: .macos-builder's
// build.sh assembles the app by calling `br menubar bundle` rather than
// carrying a plist of its own.
func TestInfoPlistDeclaresTCCPurposeStrings(t *testing.T) {
	plist := infoPlist()

	for _, key := range []string{
		"NSDownloadsFolderUsageDescription",
		"NSRemovableVolumesUsageDescription",
	} {
		if !strings.Contains(plist, "<key>"+key+"</key>") {
			t.Errorf("Info.plist is missing %s", key)
			continue
		}
		// A key with an empty purpose string is treated as missing by TCC and
		// is rejected by App Review, so the value has to be real prose.
		if !strings.Contains(plist, "<key>"+key+"</key><string>") {
			t.Errorf("%s has no purpose string", key)
		}
	}
}

// TestInfoPlistPurposeStringsAreUserReadable checks the strings say what the app
// does with the access, which is what the user is shown in the TCC prompt.
func TestInfoPlistPurposeStringsAreUserReadable(t *testing.T) {
	plist := infoPlist()
	for _, phrase := range []string{"cartridge", "Bladerunner"} {
		if !strings.Contains(plist, phrase) {
			t.Errorf("Info.plist purpose strings do not mention %q", phrase)
		}
	}
}
