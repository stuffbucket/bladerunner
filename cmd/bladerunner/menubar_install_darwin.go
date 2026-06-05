//go:build darwin

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	menubarBundleName = "Bladerunner"
	menubarBundleID   = "com.stuffbucket.bladerunner.menubar"
	menubarAgentLabel = "com.stuffbucket.bladerunner.menubar"
	bundleDirPerm     = 0o755
	bundleFilePerm    = 0o644
	bundleExecPerm    = 0o755
)

// menubarPaths resolves the install locations under the user's home.
func menubarPaths() (appDir, execPath, agentPath string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("resolve home: %w", err)
	}
	appDir = filepath.Join(home, "Applications", menubarBundleName+".app")
	execPath = filepath.Join(appDir, "Contents", "MacOS", menubarBundleName)
	agentPath = filepath.Join(home, "Library", "LaunchAgents", menubarAgentLabel+".plist")
	return appDir, execPath, agentPath, nil
}

// installMenubarAgent builds the menubar-only .app bundle (LSUIElement, so no
// dock icon) around a copy of this binary, then registers a per-user LaunchAgent
// that runs it at login. The copied binary is a full 'runner', so the menu's
// Start/Stop/Web/Shell actions work via it.
func installMenubarAgent() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	appDir, execPath, agentPath, err := menubarPaths()
	if err != nil {
		return err
	}

	// Bundle layout: Bladerunner.app/Contents/{Info.plist,MacOS/Bladerunner}.
	if err := os.MkdirAll(filepath.Dir(execPath), bundleDirPerm); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}
	if err := copyExecutable(self, execPath); err != nil {
		return fmt.Errorf("copy binary into bundle: %w", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(infoPlist()), bundleFilePerm); err != nil {
		return fmt.Errorf("write Info.plist: %w", err)
	}

	// Per-user LaunchAgent. Run the bundle's executable (so NSBundle.mainBundle
	// resolves to the .app and LSUIElement takes effect) with the menubar verb.
	if err := os.MkdirAll(filepath.Dir(agentPath), bundleDirPerm); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(agentPath, []byte(launchAgentPlist(execPath)), bundleFilePerm); err != nil {
		return fmt.Errorf("write LaunchAgent: %w", err)
	}

	// Reload: bootout an old instance (ignore errors), then bootstrap + kickstart.
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = launchctl("bootout", domain+"/"+menubarAgentLabel)
	if err := launchctl("bootstrap", domain, agentPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	_ = launchctl("kickstart", "-k", domain+"/"+menubarAgentLabel)

	fmt.Printf("✓ Installed the bladerunner menubar agent\n")
	fmt.Printf("  app:   %s\n", appDir)
	fmt.Printf("  agent: %s (starts at login, no dock icon)\n", agentPath)
	fmt.Printf("  remove with: runner menubar uninstall\n")
	return nil
}

// uninstallMenubarAgent unloads the LaunchAgent and removes it + the .app bundle.
func uninstallMenubarAgent() error {
	appDir, _, agentPath, err := menubarPaths()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = launchctl("bootout", domain+"/"+menubarAgentLabel)
	rmErr := os.Remove(agentPath)
	if rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("remove LaunchAgent: %w", rmErr)
	}
	if err := os.RemoveAll(appDir); err != nil {
		return fmt.Errorf("remove app bundle: %w", err)
	}
	fmt.Printf("✓ Removed the bladerunner menubar agent\n")
	return nil
}

func launchctl(args ...string) error {
	out, err := exec.CommandContext(context.Background(), "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %v: %w: %s", args, err, out)
	}
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, bundleExecPerm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func infoPlist() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>%s</string>
	<key>CFBundleDisplayName</key><string>%s</string>
	<key>CFBundleIdentifier</key><string>%s</string>
	<key>CFBundleExecutable</key><string>%s</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleShortVersionString</key><string>%s</string>
	<key>CFBundleVersion</key><string>%s</string>
	<key>LSUIElement</key><string>1</string>
	<key>NSHighResolutionCapable</key><string>True</string>
	<key>LSMinimumSystemVersion</key><string>13.0</string>
</dict>
</plist>
`, menubarBundleName, menubarBundleName, menubarBundleID, menubarBundleName, version, version)
}

func launchAgentPlist(execPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>menubar</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key>
	<dict><key>SuccessfulExit</key><false/></dict>
	<key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`, menubarAgentLabel, execPath)
}
