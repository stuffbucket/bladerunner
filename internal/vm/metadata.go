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

func saveMetadata(cfg *config.Config, md *runtimeMetadata) error {
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime metadata: %w", err)
	}
	if err := os.WriteFile(cfg.MetadataPath, b, 0o644); err != nil {
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
