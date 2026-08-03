package imagebuild

import (
	"fmt"
	"path/filepath"
)

// Guest-side paths and endpoints for the Incus web UI.
//
// The UI ships as incus-ui-canonical, which is not in Debian main. It is NOT
// apt-installed: the Zabbly package declares "Depends: incus", so installing it
// would pull Zabbly's incus and swap out Debian's. The files are extracted from
// the .deb instead, and incusd is pointed at them.
const (
	// zabblyKeyURL is the signing key for the Zabbly archive.
	zabblyKeyURL = "https://pkgs.zabbly.com/key.asc"
	// zabblyKeyPath is where that key is written while the UI is fetched.
	zabblyKeyPath = "/etc/apt/keyrings/zabbly.asc"
	// zabblySourcePath is the apt source added for the same window.
	zabblySourcePath = "/etc/apt/sources.list.d/zabbly-incus-stable.sources"
	// zabblySuite is the Zabbly suite matching the Debian release baked here.
	zabblySuite = "trixie"
	// uiPackage is the package whose payload is extracted.
	uiPackage = "incus-ui-canonical"
	// uiRoot is where the extracted UI lands, matching the package's own layout.
	uiRoot = "/opt/incus/ui"
	// uiDropInPath points incusd at uiRoot.
	uiDropInPath = "/etc/systemd/system/incus.service.d/10-bladerunner-ui.conf"
	// uiDropInContent is that drop-in's body.
	uiDropInContent = "[Service]\nEnvironment=INCUS_UI=" + uiRoot + "\n"
)

// webUISteps bake the Incus web UI into the image.
//
// Every step that reaches Zabbly is optional. Zabbly is a third party outside
// Debian main, and a mirror outage there must not block a guest image release
// for a component the guest can live without: cloud-init re-attempts the same
// install on first boot, so a skipped bake costs a little first-boot network
// time rather than a broken guest. The workflow makes this sharper still —
// fail-fast is off and the release needs both architectures, so one arch
// failing on a third-party outage would block the other.
//
// The cleanup steps are NOT optional, and they run whether or not the bake
// happened. Leaving the apt source behind is the serious failure: a guest's
// routine `apt upgrade` would then pull Zabbly's incus to satisfy its
// "Depends: incus" and replace Debian's under a running host, days later. The
// signing key is removed for the same reason the source is — the shell build
// removes only the source, so every image published so far carries an
// unadvertised third-party trust anchor, and that is not reproduced here.
func webUISteps() []Step {
	// Architectures is deliberately omitted. apt defaults it to the guest's own
	// dpkg architecture, which is exactly what is wanted, and stating it needed
	// a $(dpkg --print-architecture) that the quoted heredoc below never runs —
	// apt then read the substitution literally, matched no architecture, and
	// found no package. The build still passed, because these steps are
	// optional, so the image simply had no web UI in it.
	source := fmt.Sprintf(`Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: %s
Components: main
Signed-By: %s
`, zabblySuite, zabblyKeyPath)

	return []Step{
		{
			Kind:     StepRun,
			Desc:     "fetch the Zabbly signing key for the web UI",
			Optional: true,
			Argv: []string{"/bin/sh", "-c",
				fmt.Sprintf("mkdir -p %s && curl -fsSL %s -o %s",
					"/etc/apt/keyrings", zabblyKeyURL, zabblyKeyPath)},
		},
		{
			Kind:     StepWriteFile,
			Desc:     "add the Zabbly archive for the web UI",
			Optional: true,
			// Written directly rather than through a shell heredoc. Nothing in
			// the content needs expanding now, and the heredoc form is what let
			// an unrunnable $(...) sit in the file looking like it worked.
			Path:    zabblySourcePath,
			Mode:    aptConfMode,
			Content: source,
		},
		{
			Kind:     StepRun,
			Desc:     "refresh the package index for the web UI",
			Optional: true,
			Argv:     []string{"apt-get", "update"},
		},
		{
			Kind:     StepRun,
			Desc:     fmt.Sprintf("extract %s to %s and point incusd at it", uiPackage, uiRoot),
			Optional: true,
			// Downloaded and unpacked, never installed — see the note above on
			// why apt-installing it would swap out Debian's incus. dpkg-deb -x
			// writes the package's own paths, which is what puts the files at
			// uiRoot.
			//
			// The drop-in is written HERE, chained behind the extract, rather
			// than as its own step. Apply continues past an optional failure,
			// so a separate step would still run after a Zabbly outage skipped
			// the extract, and the image would ship incusd configured to serve
			// a directory that does not exist.
			Argv: []string{"/bin/sh", "-c",
				fmt.Sprintf("cd /tmp && apt-get download %s && "+
					"deb=$(ls -1 %s_*.deb | head -1) && dpkg-deb -x \"$deb\" / && rm -f \"$deb\" && "+
					"test -d %s && "+
					"mkdir -p %s && printf '%s' > %s",
					uiPackage, uiPackage, uiRoot,
					filepath.Dir(uiDropInPath), uiDropInContent, uiDropInPath)},
		},
		{
			Kind: StepRun,
			Desc: "remove the Zabbly archive and signing key",
			// Unconditional. If either survives, the image carries a
			// third-party archive that apt will act on later, or a trust
			// anchor for one.
			Argv: []string{"rm", "-f", zabblySourcePath, zabblyKeyPath},
		},
	}
}
