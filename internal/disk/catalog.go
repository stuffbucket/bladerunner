package disk

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed manifests/*.disk
var builtinFS embed.FS

// Entry is one catalog disk: its manifest plus where it came from.
type Entry struct {
	Manifest *Manifest
	// Origin is OriginBuiltin or OriginUser.
	Origin string
	// Path is the on-disk path for user disks; "" for builtins.
	Path string
}

// Catalog is the merged set of builtin and user disks, indexed by name.
type Catalog struct {
	byName map[string]Entry
}

// DefaultDiskDir returns the XDG-compliant directory of user disk manifests:
// $XDG_CONFIG_HOME/bladerunner/disks or ~/.config/bladerunner/disks. Mirrors
// oidc.DefaultIdentityDir's layout.
func DefaultDiskDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bladerunner", "disks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "bladerunner", "disks")
	}
	return filepath.Join(home, ".config", "bladerunner", "disks")
}

// LoadCatalog seeds the catalog from the embedded builtins, then overlays user
// disks from DefaultDiskDir() (user wins on name collision). A missing user
// directory is not an error; malformed user files are skipped (not fatal),
// mirroring oidc.Store.Load.
func LoadCatalog() (*Catalog, error) {
	c := &Catalog{byName: make(map[string]Entry)}

	if err := c.loadBuiltins(); err != nil {
		return nil, err
	}
	if err := c.loadUserDir(DefaultDiskDir()); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) loadBuiltins() error {
	entries, err := fs.ReadDir(builtinFS, "manifests")
	if err != nil {
		return fmt.Errorf("read builtin disks: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ManifestExt) {
			continue
		}
		data, err := builtinFS.ReadFile(filepath.Join("manifests", e.Name()))
		if err != nil {
			return fmt.Errorf("read builtin disk %s: %w", e.Name(), err)
		}
		m, err := Parse(data)
		if err != nil {
			return fmt.Errorf("parse builtin disk %s: %w", e.Name(), err)
		}
		c.byName[m.Name] = Entry{Manifest: m, Origin: OriginBuiltin}
	}
	return nil
}

func (c *Catalog) loadUserDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read user disks dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ManifestExt) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, err := Load(path)
		if err != nil {
			// Skip malformed files but do not abort the whole load.
			continue
		}
		c.byName[m.Name] = Entry{Manifest: m, Origin: OriginUser, Path: path}
	}
	return nil
}

// Lookup returns the catalog entry for name, if present.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	e, ok := c.byName[name]
	return e, ok
}

// List returns all catalog entries sorted by disk name.
func (c *Catalog) List() []Entry {
	out := make([]Entry, 0, len(c.byName))
	for _, e := range c.byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}
