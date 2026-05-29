package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lxc/incus/v6/shared/api"
	"github.com/spf13/cobra"
)

var lsFlags struct {
	jsonOut bool
}

var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List Incus instances",
	Long:  `List Incus instances (containers and virtual machines) running in the Bladerunner VM.`,
	Args:  cobra.NoArgs,
	RunE:  runLs,
}

func init() {
	lsCmd.Flags().BoolVar(&lsFlags.jsonOut, "json", false, "Output as JSON")
}

func runLs(_ *cobra.Command, _ []string) error {
	client, err := connectIncus()
	if err != nil {
		return err
	}
	instances, err := client.ListInstances(context.Background())
	if err != nil {
		return err
	}

	if lsFlags.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(instances)
	}

	return renderInstanceTable(os.Stdout, instances)
}

func renderInstanceTable(out *os.File, instances []api.InstanceFull) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tTYPE\tSTATUS\tIPV4\tIMAGE"); err != nil {
		return err
	}
	for i := range instances {
		inst := &instances[i]
		typ := inst.Type
		if typ == "" {
			typ = "container"
		}
		ipv4 := primaryIPv4(inst)
		image := imageSource(inst)
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", inst.Name, typ, inst.Status, ipv4, image); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// fingerprintShortLen is the number of hex characters to display from an image fingerprint.
const fingerprintShortLen = 12

// primaryIPv4 picks the first non-loopback inet address from the instance state.
func primaryIPv4(inst *api.InstanceFull) string {
	if inst.State == nil {
		return ""
	}
	for ifname, n := range inst.State.Network { //nolint:gocritic // InstanceStateNetwork is a value type returned by the SDK
		if ifname == "lo" {
			continue
		}
		for _, addr := range n.Addresses {
			if addr.Family != "inet" {
				continue
			}
			if addr.Scope == "local" {
				continue
			}
			return addr.Address
		}
	}
	return ""
}

// imageSource returns a best-effort short description of where the instance was created from.
func imageSource(inst *api.InstanceFull) string {
	if alias := inst.Config["image.description"]; alias != "" {
		return alias
	}
	if alias := inst.Config["image.os"] + " " + inst.Config["image.release"]; strings.TrimSpace(alias) != "" {
		return strings.TrimSpace(alias)
	}
	if fp := inst.Config["volatile.base_image"]; fp != "" {
		if len(fp) > fingerprintShortLen {
			fp = fp[:fingerprintShortLen]
		}
		return fp
	}
	return ""
}
