//go:build linux

package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Attach and mount parameters.
const (
	// nbdMaxPart is how many partitions the nbd module surfaces per device.
	// The Debian cloud images use three at most; eight leaves headroom without
	// creating hundreds of unused device nodes.
	nbdMaxPart = 8
	// partitionSettleTimeout bounds the wait for the kernel to surface
	// partitions after the image is attached. The device appears immediately
	// but its partition table is scanned asynchronously, so reading too early
	// finds nothing.
	partitionSettleTimeout = 20 * time.Second
	// partitionPollInterval is how often the wait re-checks.
	partitionPollInterval = 200 * time.Millisecond
	// sysBlockDir is where the kernel publishes block device geometry.
	sysBlockDir = "/sys/class/block"
	// partitionNodeMode is the mode for a block device node the build creates
	// for itself. Only the build reads or writes it, and it lives in the work
	// directory rather than /dev.
	partitionNodeMode = 0o600
)

// guestPATH is the search path for commands run inside the guest. It is set
// explicitly because the environment is otherwise emptied: inheriting the
// host's PATH would point at host directories that do not exist in the guest.
const guestPATH = "/usr/sbin:/usr/bin:/sbin:/bin"

// fallbackNameservers keep apt working when the host has no readable resolver
// configuration. The chroot shares the host network namespace, so a public
// resolver is reachable whenever the host itself has connectivity.
const fallbackNameservers = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"

// hostResolvConf is the host resolver configuration copied into the guest for
// the duration of the build.
const hostResolvConf = "/etc/resolv.conf"

// guestResolvConf is where that copy lands inside the guest.
const guestResolvConf = "/etc/resolv.conf"

// bindMounts are the pseudo-filesystems apt and systemctl need inside the
// chroot. Order matters on teardown, which unwinds in reverse.
var bindMounts = []string{"/dev", "/proc", "/sys"}

// undo is one teardown action, with a description used when it fails.
type undo struct {
	desc string
	run  func() error
}

// nativeMount is a guest image attached to the host and mounted, together with
// everything needed to take it apart again.
//
// Teardown is recorded as it is built up, rather than reconstructed afterwards,
// because a partial failure part-way through attach must unwind exactly what
// succeeded. Reconstructing it would mean guessing, and guessing wrong here
// leaves an nbd device connected and an image that cannot be reopened.
type nativeMount struct {
	// Root is the mounted guest filesystem.
	Root string
	// device is the nbd device the image is attached to.
	device string
	// undos are teardown actions, run in reverse order.
	undos []undo
}

// push records a teardown action.
func (m *nativeMount) push(desc string, run func() error) {
	m.undos = append(m.undos, undo{desc: desc, run: run})
}

// Close unwinds the mount.
//
// Every action is attempted even after one fails, because leaving an nbd device
// connected is worse than any single teardown error: the device stays busy and
// the next build on the machine cannot attach. Errors are joined so none is
// lost.
func (m *nativeMount) Close() error {
	var errs []error
	for i := len(m.undos) - 1; i >= 0; i-- {
		if err := m.undos[i].run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.undos[i].desc, err))
		}
	}
	m.undos = nil
	return errors.Join(errs...)
}

// attachImage connects image to an nbd device, mounts its root filesystem under
// workDir, and prepares the chroot.
//
// On any failure it unwinds whatever it had already done, so a failed attach
// never leaves a device connected.
func attachImage(ctx context.Context, image, workDir, device string) (_ *nativeMount, err error) {
	// The mount is a local rather than a named return: every failure path
	// returns a nil pointer, which would leave the deferred unwind with nothing
	// to unwind and a nil receiver to call it on.
	m := &nativeMount{device: device}
	defer func() {
		if err == nil {
			return
		}
		if closeErr := m.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("unwind the failed attach: %w", closeErr))
		}
	}()

	if err = ensureNBDModule(ctx, device); err != nil {
		return nil, err
	}
	if err = run(ctx, "qemu-nbd", "--connect="+device, image); err != nil {
		return nil, fmt.Errorf("attach %s to %s: %w", image, device, err)
	}
	m.push("disconnect "+device, func() error {
		return run(context.WithoutCancel(ctx), "qemu-nbd", "--disconnect", device)
	})

	var rootPart string
	if rootPart, err = waitForRootPartition(ctx, device); err != nil {
		return nil, err
	}

	var node string
	if node, err = m.partitionNode(rootPart, workDir); err != nil {
		return nil, err
	}

	mountpoint := filepath.Join(workDir, "mnt")
	if err = os.MkdirAll(mountpoint, guestDirMode); err != nil {
		return nil, fmt.Errorf("create mount point %s: %w", mountpoint, err)
	}
	if err = unix.Mount(node, mountpoint, "ext4", 0, ""); err != nil {
		return nil, fmt.Errorf("mount %s at %s: %w", node, mountpoint, err)
	}
	m.Root = mountpoint
	m.push("unmount "+mountpoint, func() error { return unmount(mountpoint) })

	if err = m.bindPseudoFilesystems(); err != nil {
		return nil, err
	}
	if err = m.installResolver(); err != nil {
		return nil, err
	}
	return m, nil
}

// ensureNBDModule makes sure the nbd device is usable.
//
// modprobe is best-effort because the module is frequently already loaded and
// not loadable again from where the build runs: inside a container the kernel
// belongs to the host, so /lib/modules is either absent or the wrong version
// while /dev/nbd0 is present and working. The device's existence is what
// actually matters, so that is what decides.
func ensureNBDModule(ctx context.Context, device string) error {
	modprobeErr := run(ctx, "modprobe", "nbd", "max_part="+strconv.Itoa(nbdMaxPart))
	if _, err := os.Stat(device); err == nil {
		return nil
	}
	if modprobeErr != nil {
		return fmt.Errorf("load the nbd module and %s is absent: %w", device, modprobeErr)
	}
	return fmt.Errorf("the nbd module loaded but %s did not appear", device)
}

// bindPseudoFilesystems makes /dev, /proc and /sys visible inside the chroot.
// apt maintainer scripts and systemctl both fail without them.
//
// The binds are deliberately not recursive. A recursive bind of /dev or /sys
// brings its submounts along — /dev/pts, /dev/shm, the cgroup hierarchy — and
// then the parent cannot be unmounted while its children are still mounted, so
// teardown fails with EBUSY and the build leaks an attached device.
func (m *nativeMount) bindPseudoFilesystems() error {
	for _, src := range bindMounts {
		dst := filepath.Join(m.Root, src)
		if err := os.MkdirAll(dst, guestDirMode); err != nil {
			return fmt.Errorf("create bind target %s: %w", dst, err)
		}
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s at %s: %w", src, dst, err)
		}
		m.push("unmount "+dst, func() error { return unmount(dst) })
	}
	return nil
}

// installResolver gives the chroot a working resolver for the duration of the
// build, and puts the image's own back afterwards.
//
// The Debian cloud image ships /etc/resolv.conf as a symlink into
// systemd-resolved's runtime directory, which dangles inside an offline chroot,
// so apt cannot resolve the mirror. The original is restored on teardown: a
// baked image carrying the build host's resolver would resolve DNS differently
// from stock Debian on every guest it ever boots.
func (m *nativeMount) installResolver() error {
	target := filepath.Join(m.Root, guestResolvConf)

	original, err := os.Lstat(target)
	hadOriginal := err == nil
	var linkTarget string
	if hadOriginal && original.Mode()&os.ModeSymlink != 0 {
		if linkTarget, err = os.Readlink(target); err != nil {
			return fmt.Errorf("read the image's resolv.conf symlink: %w", err)
		}
	}

	if hadOriginal {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove the image's resolv.conf: %w", err)
		}
	}

	body, err := os.ReadFile(hostResolvConf)
	if err != nil || len(body) == 0 {
		body = []byte(fallbackNameservers)
	}
	if err := os.WriteFile(target, body, aptConfMode); err != nil {
		return fmt.Errorf("install a build-time resolv.conf: %w", err)
	}

	m.push("restore the image's resolv.conf", func() error {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		switch {
		case !hadOriginal:
			return nil
		case linkTarget != "":
			return os.Symlink(linkTarget, target)
		default:
			return nil
		}
	})
	return nil
}

// waitForRootPartition returns the device node of the guest's root partition,
// waiting for the kernel to finish scanning the partition table.
//
// The partition is identified by parsing the GPT off the attached device and
// matching the result against the kernel's own view in sysfs, rather than
// assuming an index. Debian's cloud images carry an EFI system partition whose
// entry order does not match its on-disk order, so a fixed index mounts the
// wrong filesystem on some releases and the right one on others.
func waitForRootPartition(ctx context.Context, device string) (string, error) {
	deadline := time.Now().Add(partitionSettleTimeout)
	var lastErr error
	for {
		node, err := rootPartitionNode(device)
		if err == nil {
			return node, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return "", fmt.Errorf("no root partition appeared on %s within %s: %w", device, partitionSettleTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(partitionPollInterval):
		}
	}
}

// rootPartitionNode resolves the root partition to a kernel block device name
// in one attempt.
func rootPartitionNode(device string) (string, error) {
	f, err := os.Open(device)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", device, err)
	}
	defer func() { _ = f.Close() }()

	root, err := findRootPartition(f)
	if err != nil {
		return "", err
	}
	return matchPartitionNode(device, root.StartLBA)
}

// partitionNode returns a path that can be mounted for the named partition.
//
// It prefers the node the system already publishes, and creates a private one
// when there is none. A container gets a minimal /dev that is populated when it
// starts, so a partition the kernel surfaces afterwards has an entry in sysfs
// and no node in /dev — the build would otherwise fail on exactly the hosts the
// unprivileged appliance exists to avoid needing. The node is made from the
// kernel's own major:minor, so it names the same device either way.
func (m *nativeMount) partitionNode(name, workDir string) (string, error) {
	published := filepath.Join("/dev", name)
	if _, err := os.Stat(published); err == nil {
		return published, nil
	}

	major, minor, err := deviceNumber(name)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(workDir, "dev")
	if err := os.MkdirAll(dir, guestDirMode); err != nil {
		return "", fmt.Errorf("create a directory for the partition node: %w", err)
	}
	node := filepath.Join(dir, name)
	_ = os.Remove(node) // A node left by an interrupted build is not reusable.
	if err := unix.Mknod(node, unix.S_IFBLK|partitionNodeMode, int(unix.Mkdev(major, minor))); err != nil {
		return "", fmt.Errorf("create a block device node for %s (%d:%d): %w", name, major, minor, err)
	}
	m.push("remove the partition node "+node, func() error { return os.Remove(node) })
	return node, nil
}

// deviceNumber reads a block device's major and minor numbers from sysfs.
func deviceNumber(name string) (major, minor uint32, err error) {
	body, err := os.ReadFile(filepath.Join(sysBlockDir, name, "dev"))
	if err != nil {
		return 0, 0, fmt.Errorf("read the device number for %s: %w", name, err)
	}
	text := strings.TrimSpace(string(body))
	majorText, minorText, ok := strings.Cut(text, ":")
	if !ok {
		return 0, 0, fmt.Errorf("device number for %s is %q, want major:minor", name, text)
	}
	parsedMajor, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse the major number for %s: %w", name, err)
	}
	parsedMinor, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse the minor number for %s: %w", name, err)
	}
	return uint32(parsedMajor), uint32(parsedMinor), nil
}

// matchPartitionNode finds the kernel partition on device whose start sector is
// startLBA, and returns its block device name.
//
// Matching on the start sector ties the GPT parse and the kernel's own scan
// together. If they disagree the build stops rather than mounting whichever
// partition happened to be first, because the two disagreeing means one of them
// is reading a table the other is not.
func matchPartitionNode(device string, startLBA uint64) (string, error) {
	base := filepath.Base(device)
	entries, err := os.ReadDir(filepath.Join(sysBlockDir, base))
	if err != nil {
		return "", fmt.Errorf("read the partition list for %s: %w", device, err)
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) || e.Name() == base {
			continue
		}
		body, err := os.ReadFile(filepath.Join(sysBlockDir, base, e.Name(), "start"))
		if err != nil {
			continue // Not a partition directory.
		}
		start, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
		if err != nil {
			continue
		}
		if start == startLBA {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no partition on %s starts at sector %d, though the GPT names one there", device, startLBA)
}

// unmount detaches a mount point, tolerating one that is already gone.
//
// A busy mount is retried as a lazy detach rather than reported. Leaving the
// guest root mounted is the worse outcome by a wide margin: the nbd device
// cannot then be disconnected, the image stays locked, every later build on the
// machine fails, and the caller's work directory still contains a live view of
// a guest filesystem that cleanup would try to delete file by file.
func unmount(path string) error {
	err := unix.Unmount(path, 0)
	if isUnmounted(err) {
		return nil
	}
	if !errors.Is(err, unix.EBUSY) {
		return err
	}

	// The filesystem is flushed first, because a lazy detach returns before
	// the kernel has finished releasing the mount.
	unix.Sync()
	if lazyErr := unix.Unmount(path, unix.MNT_DETACH); !isUnmounted(lazyErr) {
		return fmt.Errorf("%w (a lazy detach also failed: %w)", err, lazyErr)
	}
	return nil
}

// isUnmounted reports whether an unmount call left nothing mounted at the path,
// whether or not it had to do anything.
func isUnmounted(err error) bool {
	return err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT)
}

// syncFilesystems flushes pending writes to disk.
//
// The build writes through a mounted filesystem onto an nbd device backed by a
// qcow2 file. Detaching before those writes reach the file leaves the compress
// step reading a partially written image, which produces a corrupt result that
// still looks like a successful build.
func syncFilesystems() {
	unix.Sync()
}

// run executes a host command, folding its output into any error so a failure
// reports what the tool actually said.
func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// chrootRunner executes build steps inside a mounted guest root.
type chrootRunner struct {
	// root is the mounted guest filesystem.
	root string
	// log receives one line per command, so a long build shows progress.
	log func(string)
}

// Run executes argv inside the guest root.
//
// The command is launched through the guest's own /bin/sh so that PATH is
// resolved inside the chroot. Go performs the chroot in the child between fork
// and exec, which means the binary named here must exist in the guest, not on
// the host — and the recipe names commands like `apt-get` without a path,
// exactly as a shell script inside the image would.
func (c chrootRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("no command to run")
	}
	if c.log != nil {
		c.log(strings.Join(argv, " "))
	}

	cmd := exec.CommandContext(ctx, "/bin/sh")
	// `exec "$@"` runs argv without re-quoting it through a command string,
	// so an argument containing spaces cannot be split into two.
	cmd.Args = append([]string{"sh", "-c", `exec "$@"`, "sh"}, argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: c.root}
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=" + guestPATH,
		// Without this, package configuration blocks waiting for a terminal
		// that an offline build does not have.
		"DEBIAN_FRONTEND=noninteractive",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
