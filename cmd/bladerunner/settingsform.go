//go:build darwin

package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// Atkinson Hyperlegible (Braille Institute, SIL OFL 1.1) — a low-vision font
// chosen for the settings UI. SF Pro isn't dependable inside the WKWebView and
// isn't web-embeddable, so the latin subset (regular + bold) is embedded and
// served as a data: @font-face, making it available regardless of the host.
//
//go:embed assets/fonts/AtkinsonHyperlegible-Regular.woff2
var ahRegularWOFF2 []byte

//go:embed assets/fonts/AtkinsonHyperlegible-Bold.woff2
var ahBoldWOFF2 []byte

// Atkinson ships only regular (400) and bold (700) faces.
const (
	fontWeightRegular = 400
	fontWeightBold    = 700
)

// fontFaceCSS emits @font-face rules embedding the font as base64 data URIs.
func fontFaceCSS() string {
	enc := base64.StdEncoding
	const tmpl = "@font-face{font-family:'Atkinson Hyperlegible';font-style:normal;" +
		"font-weight:%d;font-display:swap;src:url(data:font/woff2;base64,%s) format('woff2');}"
	return fmt.Sprintf(tmpl, fontWeightRegular, enc.EncodeToString(ahRegularWOFF2)) +
		fmt.Sprintf(tmpl, fontWeightBold, enc.EncodeToString(ahBoldWOFF2))
}

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
		return settingsSaveOutcome{Message: "Saved. Restart the VM (menu › Restart VM) to apply."}
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
// selectCtl renders a styled <select> control with the given options
// (value,label pairs); cur marks the selected one.
func selectCtl(name, id, onchange, cur string, opts [][2]string) string {
	attrs := `class="ctl" name="` + name + `"`
	if id != "" {
		attrs += ` id="` + id + `"`
	}
	if onchange != "" {
		attrs += ` onchange="` + onchange + `"`
	}
	var b strings.Builder
	b.WriteString("<select " + attrs + ">")
	for _, o := range opts {
		b.WriteString(option(o[0], o[1], cur))
	}
	b.WriteString("</select>")
	return b.String()
}

// srow renders one settings row inside a card: a label on the left and a control
// on the right. A non-empty id lets the conditional-visibility JS hide the row.
func srow(id, label, control string) string {
	idAttr := ""
	if id != "" {
		idAttr = ` id="` + id + `"`
	}
	return `<div class="row"` + idAttr + `><span class="lbl">` + label + `</span>` + control + `</div>`
}

func settingsFormHTML(s config.Settings) string {
	v := valuesFromSettings(s)
	esc := func(key string) string { return html.EscapeString(v[key]) }
	numCtl := func(name string, minVal int, key string) string {
		return fmt.Sprintf(`<input class="ctl" type="number" min="%d" name=%q value=%q>`, minVal, name, esc(key))
	}
	textCtl := func(name, key string) string {
		return fmt.Sprintf(`<input class="ctl" type="text" name=%q value=%q>`, name, esc(key))
	}

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><style>`)
	b.WriteString(fontFaceCSS())
	b.WriteString(settingsCSS)
	b.WriteString(`</style></head><body><form id="f">`)

	// Brand header (the native title bar already says "Bladerunner Settings").
	b.WriteString(`<header class="head"><span class="mark"><b>br</b></span>` +
		`<span class="ht"><span class="title">Bladerunner</span>` +
		`<span class="sub">Virtual machine settings</span></span></header>`)

	// General.
	b.WriteString(`<div class="group"><div class="group-title">General</div><div class="card">`)
	b.WriteString(srow("", "Start policy", selectCtl(fStartPolicy, "", "", v[fStartPolicy], [][2]string{
		{string(config.StartManual), "Manual — only when I ask"},
		{string(config.StartOnLaunch), "When the menu-bar app launches"},
		{string(config.StartOnFirstAction), "On first Web/Shell action"},
	})))
	b.WriteString(`</div></div>`)

	// Resources.
	b.WriteString(`<div class="group"><div class="group-title">Resources</div><div class="card">`)
	b.WriteString(srow("", "CPUs", numCtl(fCPUs, 1, fCPUs)))
	b.WriteString(srow("", "Memory (GiB)", numCtl(fMemoryGiB, 2, fMemoryGiB)))
	b.WriteString(srow("", "Disk (GiB)", numCtl(fDiskSizeGiB, config.MinDiskSizeGiB, fDiskSizeGiB)))
	b.WriteString(`</div></div>`)

	// Network.
	b.WriteString(`<div class="group"><div class="group-title">Network</div><div class="card">`)
	b.WriteString(srow("", "Mode", selectCtl(fNetworkMode, "net", "sync()", v[fNetworkMode], [][2]string{
		{string(config.NetSettingShared), "Shared (NAT)"},
		{string(config.NetSettingBridged), "Bridged"},
	})))
	b.WriteString(srow("bridgeRow", "Bridge interface", textCtl(fBridgeIface, fBridgeIface)))
	b.WriteString(`</div></div>`)

	// Authentication.
	b.WriteString(`<div class="group"><div class="group-title">Authentication</div><div class="card">`)
	b.WriteString(srow("", "Mode", selectCtl(fAuthMode, "", "", v[fAuthMode], [][2]string{
		{string(config.AuthSettingOIDC), "OIDC (single sign-on)"},
		{string(config.AuthSettingCert), "Client certificate (mTLS)"},
	})))
	b.WriteString(`</div></div>`)

	// Advanced.
	b.WriteString(`<div class="group"><div class="group-title">Advanced</div><div class="card">`)
	b.WriteString(srow("", "Base image", selectCtl(fImageKind, "img", "sync()", v[fImageKind], [][2]string{
		{string(config.ImageDebian), "Debian Trixie (pinned)"},
		{string(config.ImageHosted), "Pre-baked hosted image"},
		{string(config.ImageCustomURL), "Custom URL"},
		{string(config.ImageLocalPath), "Local path"},
	})))
	b.WriteString(srow("urlRow", "Image URL", textCtl(fImageURL, fImageURL)))
	b.WriteString(srow("pathRow", "Image path", textCtl(fImagePath, fImagePath)))
	b.WriteString(srow("", "Nested virtualization", selectCtl(fNestedVirt, "", "", v[fNestedVirt], [][2]string{
		{string(config.NestedAuto), "Auto (where supported)"},
		{string(config.NestedDisabled), "Disabled"},
	})))
	checked := ""
	if v[fUseGuestAgent] == "true" {
		checked = " checked"
	}
	b.WriteString(srow("", "In-guest boot agent",
		fmt.Sprintf(`<input class="sw" type="checkbox" name=%q%s>`, fUseGuestAgent, checked)))
	b.WriteString(srow("", "Wait for Incus", textCtl(fWaitForIncus, fWaitForIncus)))
	b.WriteString(`</div></div>`)

	b.WriteString(`<footer class="actions"><span id="err" class="err"></span>` +
		`<button type="button" class="primary" onclick="save()">Save</button></footer>`)
	b.WriteString(`</form><script>`)
	b.WriteString(settingsJS)
	b.WriteString(`</script></body></html>`)
	return b.String()
}

// settingsCSS styles the form as a native macOS System Settings-style panel:
// grouped rounded cards, SF system type, a single brand accent, dark theme.
const settingsCSS = `
:root{
  color-scheme: dark;
  --bg:#1c1c1e; --card:rgba(255,255,255,.055); --line:rgba(255,255,255,.09);
  --text:#f2f2f7; --muted:#9b9ba1; --field:rgba(255,255,255,.06);
  --field-line:rgba(255,255,255,.14); --accent:#8a5cf6;
}
*{box-sizing:border-box;}
html,body{margin:0;}
body{
  background:var(--bg); color:var(--text);
  font-family:'Atkinson Hyperlegible',-apple-system,sans-serif;
  font-size:13px; line-height:1.3; padding:20px 22px 18px;
  -webkit-font-smoothing:antialiased;
}
.head{display:flex; align-items:center; gap:11px; margin:0 0 20px;}
.mark{width:32px; height:32px; border-radius:8px; background:#0b0f14;
  display:flex; align-items:center; justify-content:center;
  box-shadow:0 1px 2px rgba(0,0,0,.4), inset 0 0 0 .5px rgba(255,255,255,.07);}
.mark b{font-size:16px; font-weight:700; font-style:italic; letter-spacing:-1px;
  background:linear-gradient(105deg,#8a5cf6,#3b82f6,#06b6d4,#34d399);
  -webkit-background-clip:text; background-clip:text; color:transparent;}
.ht{display:flex; flex-direction:column; line-height:1.25;}
.ht .title{font-size:15px; font-weight:700;}
.ht .sub{font-size:11.5px; color:var(--muted);}
.group{margin-bottom:17px;}
.group-title{font-size:11px; font-weight:700; letter-spacing:.5px; text-transform:uppercase;
  color:var(--muted); margin:0 0 7px 12px;}
.card{background:var(--card); border-radius:11px; overflow:hidden;
  box-shadow:inset 0 0 0 .5px var(--line);}
.row{display:flex; align-items:center; justify-content:space-between; gap:16px;
  min-height:40px; padding:7px 14px; border-top:.5px solid var(--line);}
.card .row:first-child{border-top:0;}
.lbl{color:var(--text); white-space:nowrap;}
.ctl{width:240px; flex:0 0 240px; font:inherit; color:var(--text);
  background:var(--field); border:.5px solid var(--field-line); border-radius:6px;
  padding:5px 9px; transition:border-color .12s, box-shadow .12s;}
select.ctl{-webkit-appearance:none; appearance:none; padding-right:26px; cursor:pointer;
  background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='7'%3E%3Cpath d='M1 1l4 4 4-4' fill='none' stroke='%239b9ba1' stroke-width='1.6' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E");
  background-repeat:no-repeat; background-position:right 9px center;}
.ctl:focus{outline:none; border-color:var(--accent);
  box-shadow:0 0 0 3px color-mix(in srgb, var(--accent) 32%, transparent);}
.sw{width:16px; height:16px; accent-color:var(--accent); cursor:pointer;}
.actions{display:flex; align-items:center; justify-content:flex-end; gap:14px; margin-top:18px;}
.err{flex:1 1 auto; min-height:16px; font-size:12px; color:#ff7a7a; white-space:pre-wrap;}
.err.ok{color:#46d39a;}
button.primary{font:inherit; font-weight:700; color:#fff; background:var(--accent);
  border:0; border-radius:7px; padding:6px 18px; cursor:pointer;
  transition:filter .12s, transform .04s;}
button.primary:hover{filter:brightness(1.09);}
button.primary:active{transform:translateY(.5px); filter:brightness(.94);}
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
