//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// The settings window is a thin WKWebView renderer over the type-safe
// config.Settings model. The labor-intensive, error-prone parts — laying out
// the form and reading values back — are done here in pure Go (an HTML form
// string + a posted-values parser), so they are fully unit-testable and the
// Objective-C side only has to host a web view and shuttle one JSON string.
// This is the design's documented WKWebView path, chosen over hand-laid
// NSTextFields whose layout/binding can't be unit-verified.

// Form field names, shared by the generated HTML and the parser so they can't
// drift.
const (
	fStartPolicy   = "startPolicy"
	fCPUs          = "cpus"
	fMemoryGiB     = "memoryGiB"
	fDiskSizeGiB   = "diskSizeGiB"
	fNetworkMode   = "networkMode"
	fBridgeIface   = "bridgeInterface"
	fAuthMode      = "authMode"
	fImageKind     = "imageKind"
	fImageURL      = "imageURL"
	fImagePath     = "imagePath"
	fNestedVirt    = "nestedVirt"
	fUseGuestAgent = "useGuestAgent"
	fWaitForIncus  = "waitForIncus"
)

// valuesFromSettings flattens a Settings into the string form values the HTML
// inputs hold, so generation and round-trip tests share one mapping.
func valuesFromSettings(s config.Settings) map[string]string {
	return map[string]string{
		fStartPolicy:   string(s.StartPolicy),
		fCPUs:          strconv.FormatUint(uint64(s.CPUs), 10),
		fMemoryGiB:     strconv.FormatUint(s.MemoryGiB, 10),
		fDiskSizeGiB:   strconv.Itoa(s.DiskSizeGiB),
		fNetworkMode:   string(s.NetworkMode),
		fBridgeIface:   s.BridgeInterface,
		fAuthMode:      string(s.AuthMode),
		fImageKind:     string(s.Image.Kind),
		fImageURL:      s.Image.URL,
		fImagePath:     s.Image.Path,
		fNestedVirt:    string(s.NestedVirt),
		fUseGuestAgent: strconv.FormatBool(s.UseGuestAgent),
		fWaitForIncus:  time.Duration(s.WaitForIncus).String(),
	}
}

// parseSettingsForm applies posted string values onto base (the current
// persisted settings, which carries non-form fields like SchemaVersion) and
// returns the validated result. Unknown/missing keys keep base's value, so a
// partial post never corrupts unrelated fields.
func parseSettingsForm(posted map[string]string, base config.Settings) (config.Settings, error) {
	s := base

	get := func(key string) (string, bool) {
		v, ok := posted[key]
		return strings.TrimSpace(v), ok
	}

	if v, ok := get(fStartPolicy); ok {
		s.StartPolicy = config.StartPolicy(v)
	}
	if v, ok := get(fNetworkMode); ok {
		s.NetworkMode = config.NetSetting(v)
	}
	if v, ok := get(fBridgeIface); ok {
		s.BridgeInterface = v
	}
	if v, ok := get(fAuthMode); ok {
		s.AuthMode = config.AuthSetting(v)
	}
	if v, ok := get(fNestedVirt); ok {
		s.NestedVirt = config.NestedVirtSetting(v)
	}

	if v, ok := get(fCPUs); ok {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return config.Settings{}, fmt.Errorf("cpus: %w", err)
		}
		s.CPUs = uint(n)
	}
	if v, ok := get(fMemoryGiB); ok {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return config.Settings{}, fmt.Errorf("memory: %w", err)
		}
		s.MemoryGiB = n
	}
	if v, ok := get(fDiskSizeGiB); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config.Settings{}, fmt.Errorf("disk size: %w", err)
		}
		s.DiskSizeGiB = n
	}
	if v, ok := get(fUseGuestAgent); ok {
		s.UseGuestAgent = v == "true" || v == "on" || v == "1"
	}
	if v, ok := get(fWaitForIncus); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config.Settings{}, fmt.Errorf("wait-for-incus: %w", err)
		}
		s.WaitForIncus = config.Duration(d)
	}

	// Image source is a closed union: the chosen kind decides which extra field
	// is meaningful; clear the others so Validate's union invariant holds.
	if v, ok := get(fImageKind); ok {
		s.Image = imageSourceFromForm(config.ImageKind(v), posted)
	}

	if err := s.Validate(); err != nil {
		return config.Settings{}, err
	}
	return s, nil
}

func imageSourceFromForm(kind config.ImageKind, posted map[string]string) config.ImageSource {
	switch kind {
	case config.ImageCustomURL:
		return config.ImageSource{Kind: kind, URL: strings.TrimSpace(posted[fImageURL])}
	case config.ImageLocalPath:
		return config.ImageSource{Kind: kind, Path: strings.TrimSpace(posted[fImagePath])}
	default: // hosted, debian, or unknown — no extra field
		return config.ImageSource{Kind: kind}
	}
}

// settingsSaveOutcome is what the settings UI should do after a save attempt.
type settingsSaveOutcome struct {
	Message string // status line to show (empty when Close)
	IsError bool   // render Message as an error
	Close   bool   // success with nothing to report — close the window
}

// applySettingsForm parses the posted form JSON, validates it against the
// current persisted settings, and persists it under stateDir, returning what
// the UI should do. vmRunning lets a restart-only change advise a restart
// instead of closing. Pure (no cgo) so the whole save path is unit-testable.
func applySettingsForm(rawJSON, stateDir string, vmRunning bool) settingsSaveOutcome {
	var posted map[string]string
	if err := json.Unmarshal([]byte(rawJSON), &posted); err != nil {
		return settingsSaveOutcome{Message: "Could not read the form.", IsError: true}
	}

	base, err := config.LoadSettings(stateDir)
	if err != nil {
		base = config.DefaultSettings()
	}

	updated, err := parseSettingsForm(posted, base)
	if err != nil {
		return settingsSaveOutcome{Message: err.Error(), IsError: true}
	}
	if err := updated.Save(stateDir); err != nil {
		return settingsSaveOutcome{Message: "Could not save: " + err.Error(), IsError: true}
	}

	if settingsRequiresRestart(base, updated) && vmRunning {
		return settingsSaveOutcome{Message: "Saved. Restart the VM (menu ▸ Restart VM) to apply."}
	}
	return settingsSaveOutcome{Close: true}
}

// settingsRequiresRestart reports whether the change from old to new touches a
// field that only takes effect on the next VM start (CPUs/memory/disk/network/
// auth/image/nested-virt/guest-agent). StartPolicy and a bare bridge-iface tweak
// while shared are menubar-only and don't need a restart.
func settingsRequiresRestart(old, neu config.Settings) bool {
	return old.CPUs != neu.CPUs ||
		old.MemoryGiB != neu.MemoryGiB ||
		old.DiskSizeGiB != neu.DiskSizeGiB ||
		old.NetworkMode != neu.NetworkMode ||
		old.BridgeInterface != neu.BridgeInterface ||
		old.AuthMode != neu.AuthMode ||
		old.NestedVirt != neu.NestedVirt ||
		old.UseGuestAgent != neu.UseGuestAgent ||
		old.Image != neu.Image
}

// option renders one <option>, marking it selected when it matches cur.
func option(value, label, cur string) string {
	sel := ""
	if value == cur {
		sel = " selected"
	}
	return fmt.Sprintf("<option value=%q%s>%s</option>", value, sel, html.EscapeString(label))
}

// settingsFormHTML renders the settings form as a self-contained HTML document.
// On Save it posts a JSON object of field→value to the native message handler
// named "bladerunner"; the conditional rows (bridge iface, image url/path) are
// shown/hidden by tiny inline JS keyed off the relevant selects.
func settingsFormHTML(s config.Settings) string {
	v := valuesFromSettings(s)
	esc := func(key string) string { return html.EscapeString(v[key]) }

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>`)
	b.WriteString(settingsCSS)
	b.WriteString(`</style></head><body><form id="f">`)
	b.WriteString(`<h1>Bladerunner Settings</h1>`)

	// General.
	b.WriteString(`<section><h2>General</h2>`)
	b.WriteString(`<label>Start policy<select name="` + fStartPolicy + `">`)
	b.WriteString(option(string(config.StartManual), "Manual (start only when I ask)", v[fStartPolicy]))
	b.WriteString(option(string(config.StartOnLaunch), "Start when the menubar launches", v[fStartPolicy]))
	b.WriteString(option(string(config.StartOnFirstAction), "Start on first Web/Shell action", v[fStartPolicy]))
	b.WriteString(`</select></label></section>`)

	// Resources.
	b.WriteString(`<section><h2>Resources</h2>`)
	fmt.Fprintf(&b, `<label>CPUs<input type="number" min="1" name=%q value=%q></label>`, fCPUs, esc(fCPUs))
	fmt.Fprintf(&b, `<label>Memory (GiB)<input type="number" min="2" name=%q value=%q></label>`, fMemoryGiB, esc(fMemoryGiB))
	fmt.Fprintf(&b, `<label>Disk (GiB)<input type="number" min="%d" name=%q value=%q></label>`, config.MinDiskSizeGiB, fDiskSizeGiB, esc(fDiskSizeGiB))
	b.WriteString(`</section>`)

	// Network.
	b.WriteString(`<section><h2>Network</h2>`)
	b.WriteString(`<label>Mode<select id="net" name="` + fNetworkMode + `" onchange="sync()">`)
	b.WriteString(option(string(config.NetSettingShared), "Shared (NAT)", v[fNetworkMode]))
	b.WriteString(option(string(config.NetSettingBridged), "Bridged", v[fNetworkMode]))
	b.WriteString(`</select></label>`)
	fmt.Fprintf(&b, `<label id="bridgeRow">Bridge interface<input type="text" name=%q value=%q></label>`, fBridgeIface, esc(fBridgeIface))
	b.WriteString(`</section>`)

	// Auth.
	b.WriteString(`<section><h2>Authentication</h2>`)
	b.WriteString(`<label>Mode<select name="` + fAuthMode + `">`)
	b.WriteString(option(string(config.AuthSettingOIDC), "OIDC (single sign-on)", v[fAuthMode]))
	b.WriteString(option(string(config.AuthSettingCert), "Client certificate (mTLS)", v[fAuthMode]))
	b.WriteString(`</select></label></section>`)

	// Advanced.
	b.WriteString(`<section><h2>Advanced</h2>`)
	b.WriteString(`<label>Base image<select id="img" name="` + fImageKind + `" onchange="sync()">`)
	b.WriteString(option(string(config.ImageDebian), "Debian Trixie (pinned)", v[fImageKind]))
	b.WriteString(option(string(config.ImageHosted), "Pre-baked hosted image", v[fImageKind]))
	b.WriteString(option(string(config.ImageCustomURL), "Custom URL", v[fImageKind]))
	b.WriteString(option(string(config.ImageLocalPath), "Local path", v[fImageKind]))
	b.WriteString(`</select></label>`)
	fmt.Fprintf(&b, `<label id="urlRow">Image URL<input type="text" name=%q value=%q></label>`, fImageURL, esc(fImageURL))
	fmt.Fprintf(&b, `<label id="pathRow">Image path<input type="text" name=%q value=%q></label>`, fImagePath, esc(fImagePath))
	b.WriteString(`<label>Nested virtualization<select name="` + fNestedVirt + `">`)
	b.WriteString(option(string(config.NestedAuto), "Auto (enable where supported)", v[fNestedVirt]))
	b.WriteString(option(string(config.NestedDisabled), "Disabled", v[fNestedVirt]))
	b.WriteString(`</select></label>`)
	checked := ""
	if v[fUseGuestAgent] == "true" {
		checked = " checked"
	}
	fmt.Fprintf(&b, `<label class="cb"><input type="checkbox" name=%q%s>Use in-guest agent for boot config</label>`, fUseGuestAgent, checked)
	fmt.Fprintf(&b, `<label>Wait for Incus (e.g. 10m)<input type="text" name=%q value=%q></label>`, fWaitForIncus, esc(fWaitForIncus))
	b.WriteString(`</section>`)

	b.WriteString(`<div id="err" class="err"></div>`)
	b.WriteString(`<div class="actions"><button type="button" onclick="save()">Save</button></div>`)
	b.WriteString(`</form><script>`)
	b.WriteString(settingsJS)
	b.WriteString(`</script></body></html>`)
	return b.String()
}

// settingsCSS styles the form to feel native and respect light/dark mode.
const settingsCSS = `
:root { color-scheme: light dark; }
body { font: -apple-system-body, system-ui; margin: 0; padding: 16px 20px; }
h1 { font-size: 17px; margin: 0 0 12px; }
h2 { font-size: 12px; text-transform: uppercase; letter-spacing: .04em; opacity: .6; margin: 16px 0 6px; }
section { margin-bottom: 6px; }
label { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 5px 0; font-size: 13px; }
label.cb { justify-content: flex-start; gap: 8px; }
input[type=text], input[type=number], select { width: 220px; font-size: 13px; }
input[type=checkbox] { width: auto; }
.actions { margin-top: 14px; text-align: right; }
button { font-size: 13px; padding: 4px 14px; }
.err { color: #d33; font-size: 12px; min-height: 16px; margin-top: 8px; white-space: pre-wrap; }
`

// settingsJS shows/hides the conditional rows and posts the form as JSON.
const settingsJS = `
function sync(){
  document.getElementById('bridgeRow').style.display =
    document.getElementById('net').value === 'bridged' ? '' : 'none';
  var k = document.getElementById('img').value;
  document.getElementById('urlRow').style.display = k === 'custom-url' ? '' : 'none';
  document.getElementById('pathRow').style.display = k === 'local-path' ? '' : 'none';
}
function save(){
  var f = document.getElementById('f');
  var o = {};
  for (var i=0;i<f.elements.length;i++){
    var e = f.elements[i];
    if(!e.name) continue;
    o[e.name] = (e.type === 'checkbox') ? (e.checked ? 'true':'false') : e.value;
  }
  window.webkit.messageHandlers.bladerunner.postMessage(JSON.stringify(o));
}
// showMessage is called from native: isError red, otherwise an informational
// (e.g. "saved, restart to apply") notice.
function showMessage(m, isError){
  var e = document.getElementById('err');
  e.textContent = m;
  e.style.color = isError ? '#d33' : '#2a8a2a';
}
sync();
`
