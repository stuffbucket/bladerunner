package imagebuild

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/util"
)

// Guest-side locations Describe reads. They are where the distribution keeps
// the facts, not where this build writes them, so they are correct for an image
// however it was produced.
const (
	// dpkgStatusPath is dpkg's database of installed packages.
	dpkgStatusPath = "/var/lib/dpkg/status"
	// enabledUnitsDir is where systemctl enable links a unit for the default
	// target, so its contents are the enabled set.
	enabledUnitsDir = "/etc/systemd/system/multi-user.target.wants"
)

// absentDigest marks a path the recipe writes that is not in the image.
//
// A missing file is recorded rather than skipped. The two mechanics' web-UI
// divergence is exactly this shape — one wrote a drop-in and the other did not
// — and a comparison that omits missing paths calls those images identical.
const absentDigest = "<absent>"

// dpkgPackageField is the line dpkg's status file names a package on.
const dpkgPackageField = "Package: "

// Description is what an image CONTAINS, read out of the image itself.
//
// It exists so two images can be compared on their contents rather than on a
// list of properties someone remembered to check. A checklist gate polices the
// differences that already happened and is silent on the next one; a
// description fails on any difference, including one nobody predicted.
type Description struct {
	// Packages is the installed set, sorted.
	Packages []string
	// Units is the enabled systemd unit set, sorted.
	Units []string
	// InitramfsModules is the module list the initramfs is built with.
	InitramfsModules []string
	// Files maps each path the recipe writes to the SHA-256 of its contents,
	// or absentDigest when the image does not carry it.
	//
	// The KEY SET COMES FROM THE RECIPE, not from a list here. A step added to
	// the recipe joins the comparison with no change to this file, which is
	// what makes the gate hold for the next divergence rather than the last.
	Files map[string]string
}

// Describe reads a guest root and reports what it contains.
//
// rootDir is a mounted image, not an image file: mounting needs privileges and
// a mechanic, and keeping those out means this is testable against an ordinary
// directory.
func Describe(rootDir string, r Recipe) (Description, error) {
	packages, err := installedPackages(rootDir)
	if err != nil {
		return Description{}, err
	}
	units, err := enabledUnits(rootDir)
	if err != nil {
		return Description{}, err
	}
	modules, err := readLines(rootDir, initramfsModulesPath)
	if err != nil {
		return Description{}, err
	}
	files, err := recipeFileDigests(rootDir, r)
	if err != nil {
		return Description{}, err
	}
	return Description{
		Packages:         packages,
		Units:            units,
		InitramfsModules: modules,
		Files:            files,
	}, nil
}

// Diff reports every way two descriptions differ, as human-readable lines. An
// empty result means the two images agree on everything compared.
func (d Description) Diff(other Description) []string {
	var out []string
	out = append(out, diffSets("package", d.Packages, other.Packages)...)
	out = append(out, diffSets("enabled unit", d.Units, other.Units)...)
	out = append(out, diffSets("initramfs module", d.InitramfsModules, other.InitramfsModules)...)

	for path, digest := range d.Files {
		if theirs := other.Files[path]; theirs != digest {
			out = append(out, fmt.Sprintf("file %s: %s vs %s", path, digest, theirs))
		}
	}
	slices.Sort(out)
	return out
}

// diffSets names what each side has that the other does not.
func diffSets(label string, a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, fmt.Sprintf("%s %s: only in the first image", label, v))
		}
	}
	for _, v := range b {
		if !slices.Contains(a, v) {
			out = append(out, fmt.Sprintf("%s %s: only in the second image", label, v))
		}
	}
	return out
}

// installedPackages reads dpkg's own database rather than running dpkg, so this
// works against a mounted root without entering it.
func installedPackages(rootDir string) ([]string, error) {
	f, err := openUnder(rootDir, dpkgStatusPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, manifestScanInitial), manifestScanMax)
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), dpkgPackageField); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", dpkgStatusPath, err)
	}
	slices.Sort(out)
	return out, nil
}

// enabledUnits lists what systemctl enable has linked for the default target.
func enabledUnits(rootDir string) ([]string, error) {
	dir, err := util.SafeJoin(rootDir, enabledUnitsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", enabledUnitsDir, err)
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", enabledUnitsDir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out, nil
}

// readLines returns a guest file's non-empty lines, or nil when it is absent.
func readLines(rootDir, guestPath string) ([]string, error) {
	f, err := openUnder(rootDir, guestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", guestPath, err)
	}
	slices.Sort(out)
	return out, nil
}

// recipeFileDigests hashes every path the recipe writes.
//
// The paths come from the recipe's own steps, so this cannot fall behind it.
func recipeFileDigests(rootDir string, r Recipe) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range r.Steps() {
		if s.Kind != StepWriteFile && s.Kind != StepAppendFile {
			continue
		}
		digest, err := fileDigest(rootDir, s.Path)
		if err != nil {
			return nil, err
		}
		out[s.Path] = digest
	}
	return out, nil
}

// fileDigest is the SHA-256 of a guest file, or absentDigest when it is absent.
func fileDigest(rootDir, guestPath string) (string, error) {
	f, err := openUnder(rootDir, guestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return absentDigest, nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("digest %s: %w", guestPath, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// openUnder opens a guest-absolute path inside rootDir. A missing file is
// reported by wrapping fs.ErrNotExist, so callers decide whether absence is an
// error or an answer. Containment is checked, because a description is often
// taken of an image someone else built.
func openUnder(rootDir, guestPath string) (*os.File, error) {
	full, err := util.SafeJoin(rootDir, guestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", guestPath, err)
	}
	f, err := os.Open(filepath.Clean(full))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", guestPath, err)
	}
	return f, nil
}
