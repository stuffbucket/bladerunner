package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/ssh"
	"github.com/stuffbucket/bladerunner/internal/vm"
)

const (
	defaultVMName      = "incus-vm"
	defaultCPUs        = 4
	defaultMemoryGiB   = 8
	defaultDiskSizeGiB = 64
	defaultTimeout     = 5 * time.Minute
)

// Use constants to satisfy the linter (they document defaults)
var _ = []any{defaultVMName, defaultCPUs, defaultMemoryGiB, defaultDiskSizeGiB, defaultTimeout}

var startFlags struct {
	name      string
	cpus      uint
	memory    uint64
	disk      int
	headless  bool
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
	f.StringVarP(&startFlags.name, "name", "n", "incus-vm", "VM name")
	f.UintVar(&startFlags.cpus, "cpus", 4, "Number of CPUs")
	f.Uint64Var(&startFlags.memory, "memory", 8, "Memory in GiB")
	f.IntVar(&startFlags.disk, "disk", 64, "Disk size in GiB")
	f.BoolVar(&startFlags.headless, "headless", false, "Run without GUI")
	f.StringVar(&startFlags.stateDir, "state-dir", "", "State directory (default: ~/.local/state/bladerunner)")
	f.StringVar(&startFlags.imageURL, "image-url", "", "Base image URL")
	f.StringVar(&startFlags.imagePath, "image-path", "", "Local base image path")
	f.DurationVar(&startFlags.timeout, "timeout", 5*time.Minute, "Wait timeout for Incus")
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Build config
	cfg, err := config.Default(startFlags.stateDir, startFlags.name)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Apply flags
	cfg.CPUs = startFlags.cpus
	cfg.MemoryGiB = startFlags.memory
	cfg.DiskSizeGiB = startFlags.disk
	cfg.GUI = !startFlags.headless
	cfg.WaitForIncus = startFlags.timeout

	if startFlags.imageURL != "" {
		cfg.BaseImageURL = startFlags.imageURL
	}
	if startFlags.imagePath != "" {
		cfg.BaseImagePath = startFlags.imagePath
	}

	// Setup logging
	logFile, err := setupLogging(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	// Ensure SSH keys
	keyPair, err := ssh.EnsureKeyPair()
	if err != nil {
		return fmt.Errorf("ssh keys: %w", err)
	}
	cfg.SetSSHKeys(keyPair.PublicKey, keyPair.PrivateKeyPath)

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

	if cfg.GUI {
		fmt.Println(subtle("GUI window opened. Waiting for Incus to initialize..."))
	} else {
		fmt.Println(subtle("Headless mode. Waiting for Incus to initialize..."))
	}

	// Wait for Incus in background
	go func() {
		if _, err := runner.WaitForIncus(ctx); err != nil {
			log.Error("wait for incus", "error", err)
		}
	}()

	// Write SSH config after VM starts
	sshConfigPath, err := ssh.WriteSSHConfig(cfg.Name, cfg.LocalSSHPort, cfg.SSHUser, cfg.SSHPrivateKeyPath)
	if err != nil {
		log.Warn("ssh config", "error", err)
	} else {
		cfg.SSHConfigPath = sshConfigPath
	}

	fmt.Println()
	fmt.Println(success("✓ VM is running"))
	fmt.Printf("  %s %s\n", key("SSH:"), command(fmt.Sprintf("br ssh %s", cfg.Name)))
	fmt.Printf("  %s %s\n", key("Shell:"), command(fmt.Sprintf("br shell %s", cfg.Name)))
	fmt.Printf("  %s %s\n", key("API:"), value(result.Endpoint))
	fmt.Println()

	// Wait for shutdown signal
	<-ctx.Done()
	fmt.Println(subtle("\nShutting down..."))
	return nil
}

func setupLogging(logPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	log.SetLevel(log.DebugLevel)
	return f, nil
}
