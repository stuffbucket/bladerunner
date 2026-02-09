package main

import (
	"context"
	"fmt"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/control"
	"github.com/stuffbucket/bladerunner/internal/logging"
	"github.com/stuffbucket/bladerunner/internal/ssh"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

var startFlags struct {
	cpus      uint
	memory    uint64
	disk      int
	gui       bool
	stateDir  string
	imageURL  string
	imagePath string
	timeout   time.Duration
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a new VM instance",
	Long: `Start a new Incus VM instance. If no VM exists, one will be created
with cloud-init provisioning.`,
	RunE: runStart,
}

func init() {
	f := startCmd.Flags()
	f.UintVar(&startFlags.cpus, "cpus", config.DefaultCPUs, "Number of CPUs")
	f.Uint64Var(&startFlags.memory, "memory", config.DefaultMemoryGiB, "Memory in GiB")
	f.IntVar(&startFlags.disk, "disk", config.DefaultDiskSizeGiB, "Disk size in GiB")
	f.BoolVar(&startFlags.gui, "gui", false, "Open GUI console window")
	f.StringVar(&startFlags.stateDir, "state-dir", "", "State directory (default: ~/.local/state/bladerunner)")
	f.StringVar(&startFlags.imageURL, "image-url", "", "Base image URL")
	f.StringVar(&startFlags.imagePath, "image-path", "", "Local base image path")
	f.DurationVar(&startFlags.timeout, "timeout", config.DefaultTimeout, "Wait timeout for Incus")
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Build config
	cfg, err := config.Default(startFlags.stateDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Check if already running
	client := control.NewClient(cfg.VMDir)
	if client.IsRunning() {
		return fmt.Errorf("VM is already running (use 'br stop' first)")
	}

	// Start control server
	ctrlServer, err := control.NewServer(cfg.VMDir, cancel)
	if err != nil {
		return fmt.Errorf("start control server: %w", err)
	}
	defer func() { _ = ctrlServer.Close() }()

	// Mount config handler (captures cfg by reference; sees values set after VM start)
	cfgHandler := control.NewConfigRouter(cfg)
	ctrlServer.Router().Mount("config", cfgHandler.Router())

	go ctrlServer.Start(ctx)

	// Apply flags
	cfg.CPUs = startFlags.cpus
	cfg.MemoryGiB = startFlags.memory
	cfg.DiskSizeGiB = startFlags.disk
	cfg.GUI = startFlags.gui
	cfg.WaitForIncus = startFlags.timeout

	if startFlags.imageURL != "" {
		cfg.BaseImageURL = startFlags.imageURL
	}
	if startFlags.imagePath != "" {
		cfg.BaseImagePath = startFlags.imagePath
	}

	// Setup logging
	if err := logging.Init(cfg.LogPath); err != nil {
		return err
	}

	// Ensure SSH keys
	keyPair, err := ssh.EnsureKeyPair()
	if err != nil {
		return fmt.Errorf("ssh keys: %w", err)
	}
	cfgHandler.Lock()
	cfg.SetSSHKeys(keyPair.PublicKey, keyPair.PrivateKeyPath)
	cfgHandler.Unlock()

	// Create and start VM
	runner, err := vm.NewRunner(cfg)
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	defer func() { _ = runner.Stop() }()

	fmt.Println(title("Starting Bladerunner VM..."))
	fmt.Printf("  %s %s\n", key("Name:"), value(cfg.Name))
	fmt.Printf("  %s %d\n", key("CPUs:"), cfg.CPUs)
	fmt.Printf("  %s %d GiB\n", key("Memory:"), cfg.MemoryGiB)
	fmt.Printf("  %s %s\n", key("Arch:"), value(runtime.GOARCH))
	fmt.Println()

	result, err := runner.StartVM(ctx)
	if err != nil {
		return fmt.Errorf("start vm: %w", err)
	}

	// Write SSH config after VM starts
	sshConfigPath, err := ssh.WriteSSHConfig(cfg.LocalSSHPort, cfg.SSHUser, cfg.SSHPrivateKeyPath)
	if err != nil {
		logging.L().Warn("ssh config", "error", err)
	} else {
		cfgHandler.Lock()
		cfg.SSHConfigPath = sshConfigPath
		cfgHandler.Unlock()
	}

	fmt.Println()
	fmt.Println(success("✓ VM is running"))
	fmt.Printf("  %s %s\n", key("SSH:"), command("br ssh"))
	fmt.Printf("  %s %s\n", key("Shell:"), command("br shell"))
	fmt.Printf("  %s %s\n", key("API:"), value(result.Endpoint))
	fmt.Println()

	// Wait for Incus in background
	go func() {
		if _, err := runner.WaitForIncus(ctx); err != nil {
			logging.L().Error("wait for incus", "error", err)
		}
	}()

	if cfg.GUI {
		fmt.Println(subtle("Opening GUI window (runs on main thread)..."))
		// StartGUI blocks and runs the macOS event loop on main thread
		if err := runner.StartGUI(); err != nil {
			return fmt.Errorf("start gui: %w", err)
		}
	} else {
		fmt.Println(subtle("Headless mode. Press Ctrl+C to stop."))
		// Wait for shutdown signal
		<-ctx.Done()
	}

	fmt.Println(subtle("\nShutting down..."))
	return nil
}
