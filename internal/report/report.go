package report

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// reportFilePerm is the mode startup-report.json is published with. The report
// is a human-readable summary of an already-running VM, so it is world-readable.
const reportFilePerm = 0o644

// StartupReport is the machine-readable record of one boot: the host it ran on,
// the guest that was built, how to reach it, and what the Incus API said about
// itself. SaveJSON writes it to config.ReportPath
// (~/.local/state/bladerunner/startup-report.json). Nothing in this program
// reads it back — the readers are the operator and whatever tooling wraps `br`,
// which is why the JSON tags are stable and the paths are absolute.
//
// A report is also written when the readiness wait FAILS, with Incus left at
// its zero value, so the file describes a broken boot as well as a good one.
// The presence of the file is not evidence the VM came up; an empty Incus
// section, or Incus.Auth that is not "trusted", says it did not.
//
// This struct is an on-disk format that a later version reads (CLAUDE.md
// section 9, point 3). Add fields and give each new one `omitempty`; do not
// rename a field, retag one, or reuse a name for a different meaning.
type StartupReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	Host        HostInfo  `json:"host"`
	VM          VMInfo    `json:"vm"`
	Network     NetInfo   `json:"network"`
	Incus       IncusInfo `json:"incus"`
	Access      Access    `json:"access"`
}

// HostInfo records the machine that ran the boot. RequestedCPU sits beside the
// host's real CPUCount deliberately: the guest being given as many or more
// vCPUs than the host has cores is a common explanation for a boot that
// crawled, and neither number survives the process otherwise.
type HostInfo struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	CPUCount     int    `json:"cpu_count"`
	RequestedCPU uint   `json:"requested_cpu"`
}

// VMInfo records the shape of the guest and the absolute paths of the files
// behind it, so an investigation after the fact can open the console log or the
// disk without re-deriving any path from the config. ConsoleLog is usually the
// first file to read when a boot fails: the guest's own panic lands there and
// nowhere else.
//
// BaseImageURL and BaseImagePath say where the root filesystem came from, and
// are omitted when the boot did not resolve one.
type VMInfo struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Directory     string `json:"directory"`
	DiskPath      string `json:"disk_path"`
	DiskSizeGiB   int    `json:"disk_size_gib"`
	MemoryGiB     uint64 `json:"memory_gib"`
	GuestArch     string `json:"guest_arch"`
	GUIEnabled    bool   `json:"gui_enabled"`
	ConsoleLog    string `json:"console_log"`
	CloudInitISO  string `json:"cloud_init_iso"`
	BaseImageURL  string `json:"base_image_url,omitempty"`
	BaseImagePath string `json:"base_image_path,omitempty"`
}

// NetInfo records how the guest was attached to the network and the host-side
// addresses that reach it. In the default shared mode the endpoints are
// loopback host ports forwarded into the guest, and they are allocated per
// instance: two instances running at once are told apart here, so this is the
// section to check when a command reaches the wrong VM.
//
// BridgeInterface is filled only in bridged mode; in shared mode it is omitted.
type NetInfo struct {
	Mode             string `json:"mode"`
	BridgeInterface  string `json:"bridge_interface,omitempty"`
	MACAddress       string `json:"mac_address"`
	LocalSSHEndpoint string `json:"local_ssh_endpoint"`
	LocalAPIEndpoint string `json:"local_api_endpoint"`
	DashboardURL     string `json:"dashboard_url"`
}

// IncusInfo is what the Incus server inside the guest reported about itself
// once it accepted this client. Every field is omitted when the readiness wait
// never got an authorized answer, so a report with an empty incus section is a
// boot that did not finish. Auth is the field that decides that: only "trusted"
// means the guest's trust store holds our client certificate.
type IncusInfo struct {
	ServerVersion string   `json:"server_version,omitempty"`
	APIVersion    string   `json:"api_version,omitempty"`
	Auth          string   `json:"auth,omitempty"`
	ServerName    string   `json:"server_name,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
	APIExtensions int      `json:"api_extensions"`
}

// Access holds the ready-to-run commands and the credential paths a user needs
// to get into the VM: an ssh line, a curl line against the Incus API, and the
// files those two depend on. It is the part of the report a human actually
// copies from, which is why the commands are stored assembled rather than left
// for the reader to build out of the other sections.
//
// SSHConfigPath is omitted when no per-instance ssh config was written — no
// key was provisioned, or writing it failed — and SSHCommand then degrades to a
// bare `ssh -p <port> user@127.0.0.1` that still works.
//
// GoClientExamplePath is omitted on the same principle: it names a file on disk
// for the user to open, so it is only set when that file was actually written.
type Access struct {
	SSHCommand          string `json:"ssh_command"`
	SSHConfigPath       string `json:"ssh_config_path,omitempty"`
	SSHKeyPath          string `json:"ssh_key_path,omitempty"`
	RESTExample         string `json:"rest_example"`
	GoClientExamplePath string `json:"go_client_example_path,omitempty"`
	ClientCertPath      string `json:"client_cert_path"`
	ClientKeyPath       string `json:"client_key_path"`
	LogPath             string `json:"log_path"`
}

// SaveJSON publishes report to path as indented JSON, replacing whatever was
// there. Indentation is deliberate: the file is meant to be read by a person
// with `cat` as often as by a program.
//
// The write goes through internal/util, the owner of atomic file writes: a
// plain os.WriteFile opens O_TRUNC, so a reader (or `br status`) racing a
// rewrite would find the report empty or half-formed.
//
// The parent directory must already exist — this does not create it, and a
// missing directory is returned as a wrapped write error. The caller decides
// how much a failure matters: on the failed-boot path the runner only logs it,
// because a diagnostic report that cannot be saved must not mask the boot error
// that produced it.
func SaveJSON(path string, report *StartupReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal startup report: %w", err)
	}
	if err := util.WriteFileAtomic(path, b, reportFilePerm); err != nil {
		return fmt.Errorf("write startup report: %w", err)
	}
	return nil
}
