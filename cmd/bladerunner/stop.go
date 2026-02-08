package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
)

var stopFlags struct {
	timeout int
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running VM",
	Long:  `Sends a graceful shutdown signal to the running Bladerunner VM.`,
	RunE:  runStop,
}

func init() {
	stopCmd.Flags().IntVarP(&stopFlags.timeout, "timeout", "t", config.DefaultStopTimeout, "Seconds to wait for graceful shutdown")
}

func runStop(cmd *cobra.Command, args []string) error {
	stateDir := config.DefaultStateDir()

	client := control.NewClient(stateDir)

	if !client.IsRunning() {
		return fmt.Errorf("VM is not running")
	}

	fmt.Println("Stopping VM (sending graceful shutdown signal)...")
	if err := client.StopVM(); err != nil {
		return err
	}

	// Wait for the control socket to disappear (indicating process exited)
	socketPath := control.SocketPath(stateDir)
	fmt.Printf("Waiting up to %d seconds for shutdown...\n", stopFlags.timeout)
	deadline := time.Now().Add(time.Duration(stopFlags.timeout) * time.Second)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			fmt.Println("VM stopped")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for VM to stop")
}
