//go:build darwin

// runtimeMetadata is loaded and persisted only by the darwin VM runner
// (runner_darwin.go); on other platforms the VM runner is an unsupported stub,
// so this file is darwin-tagged to keep it out of those builds.
package vm

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/util"
)

// metadataFilePerm is the mode runtime-metadata.json is published with. It
// records the VM's generated MAC address, which is not a secret.
const metadataFilePerm = 0o644

type runtimeMetadata struct {
	MACAddress string `json:"mac_address"`
}

func loadOrCreateMetadata(cfg *config.Config) (*runtimeMetadata, error) {
	if util.FileExists(cfg.MetadataPath) {
		b, err := os.ReadFile(cfg.MetadataPath)
		if err == nil {
			var md runtimeMetadata
			if err := json.Unmarshal(b, &md); err == nil && md.MACAddress != "" {
				return &md, nil
			}
		}
	}

	mac, err := generateLocalMAC()
	if err != nil {
		return nil, err
	}

	md := &runtimeMetadata{MACAddress: mac.String()}
	if err := saveMetadata(cfg, md); err != nil {
		return nil, err
	}
	return md, nil
}

// saveMetadata publishes md to cfg.MetadataPath.
//
// The write goes through internal/util, the owner of atomic file writes: a
// plain os.WriteFile opens O_TRUNC, so a crash mid-write would leave the file
// empty and loadOrCreateMetadata would invent a NEW MAC address on the next
// boot, changing the VM's DHCP lease and its identity on the network.
func saveMetadata(cfg *config.Config, md *runtimeMetadata) error {
	if err := util.WriteJSONAtomic(cfg.MetadataPath, md, metadataFilePerm); err != nil {
		return fmt.Errorf("write runtime metadata: %w", err)
	}
	return nil
}

func generateLocalMAC() (net.HardwareAddr, error) {
	mac := make([]byte, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, fmt.Errorf("generate random mac: %w", err)
	}
	mac[0] = (mac[0] | 2) & 0xfe // locally administered unicast
	return net.HardwareAddr(mac), nil
}
