package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitIdentKeepsAcronymsWhole(t *testing.T) {
	cases := map[string][]string{
		"BSDName":          {"bsd", "name"},
		"normalizeDevNode": {"normalize", "dev", "node"},
		"isBareBSDName":    {"is", "bare", "bsd", "name"},
		"WriteFileAtomic":  {"write", "file", "atomic"},
		"write_file_sync":  {"write", "file", "sync"},
		"HTTPSProxy":       {"https", "proxy"},
		"sha256Sum":        {"sha", "sum"},
		"x":                {},
	}
	for in, want := range cases {
		got := splitIdent(in)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("splitIdent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTypeStringDropsPackageQualifier(t *testing.T) {
	// This is the normalisation that lets internal/util's fs.FileMode and
	// internal/webproxy's os.FileMode land in the same signature bucket.
	src := `package p
import (
	"io/fs"
	"os"
)
func A(path string, data []byte, perm fs.FileMode) error { _ = path; return nil }
func B(path string, data []byte, perm os.FileMode) error { _ = path; return nil }
`
	idx := indexSource(t, map[string]string{"a.go": src})
	if len(idx.Funcs) != 2 {
		t.Fatalf("indexed %d funcs, want 2", len(idx.Funcs))
	}
	if idx.Funcs[0].Sig != idx.Funcs[1].Sig {
		t.Fatalf("signatures differ after normalisation:\n  %s\n  %s", idx.Funcs[0].Sig, idx.Funcs[1].Sig)
	}
	if want := "(string, []byte, FileMode) -> (error)"; idx.Funcs[0].Sig != want {
		t.Errorf("Sig = %q, want %q", idx.Funcs[0].Sig, want)
	}
}

func TestReceiverFoldsIntoSignature(t *testing.T) {
	src := `package p
type T struct{}
func (t *T) Do(n int) error { _ = n; return nil }
func Do(t *T, n int) error { _ = n; return nil }
`
	idx := indexSource(t, map[string]string{"a.go": src})
	if len(idx.Funcs) != 2 {
		t.Fatalf("indexed %d funcs, want 2", len(idx.Funcs))
	}
	if idx.Funcs[0].Sig != idx.Funcs[1].Sig {
		t.Errorf("method and plain function should compare equal:\n  %s\n  %s",
			idx.Funcs[0].Sig, idx.Funcs[1].Sig)
	}
}

func TestNamedConstantResolvesToItsLiteral(t *testing.T) {
	// A body that says devDir must contribute "/dev/" to the literal signal,
	// otherwise hiding a value behind a constant hides the duplication.
	src := `package p
import "strings"
const devDir = "/dev/"
func f(ref string) string {
	if strings.HasPrefix(ref, devDir) {
		return ref
	}
	return devDir + ref
}
`
	idx := indexSource(t, map[string]string{"a.go": src})
	if len(idx.Funcs) != 1 {
		t.Fatalf("indexed %d funcs, want 1", len(idx.Funcs))
	}
	if !slices.Contains(idx.Funcs[0].Lits, "/dev/") {
		t.Errorf("literals = %v, want to contain %q", idx.Funcs[0].Lits, "/dev/")
	}
	if len(idx.Consts) != 1 || idx.Consts[0].Value != "/dev/" {
		t.Errorf("consts = %+v, want one entry valued %q", idx.Consts, "/dev/")
	}
}

func TestPlatformKeySeparatesBuildTagSiblings(t *testing.T) {
	dir := t.TempDir()
	darwin := filepath.Join(dir, "x_darwin.go")
	other := filepath.Join(dir, "x_other.go")
	plain := filepath.Join(dir, "x.go")
	write(t, darwin, "//go:build darwin\n\npackage p\n")
	write(t, other, "//go:build !darwin\n\npackage p\n")
	write(t, plain, "package p\n")

	kd := platformKey(darwin, mustRead(t, darwin))
	ko := platformKey(other, mustRead(t, other))
	kp := platformKey(plain, mustRead(t, plain))
	if kd == ko {
		t.Errorf("darwin and !darwin share a platform key %q", kd)
	}
	if kd == "" || ko == "" {
		t.Errorf("constrained files must have a key: %q %q", kd, ko)
	}
	if kp != "" {
		t.Errorf("unconstrained file has key %q, want empty", kp)
	}
}

func TestPlatformSiblingsAreDownWeighted(t *testing.T) {
	// Identical bodies in darwin / !darwin halves of one package are
	// deliberate, and must not outrank a genuine cross-package copy.
	body := `func Start(name string) error {
	if name == "" {
		return nil
	}
	return nil
}
`
	idx := indexSource(t, map[string]string{
		"x_darwin.go": "//go:build darwin\n\npackage p\n\n" + body,
		"x_other.go":  "//go:build !darwin\n\npackage p\n\n" + body,
	})
	if len(idx.Funcs) != 2 {
		t.Fatalf("indexed %d funcs, want 2", len(idx.Funcs))
	}
	corpus := NewCorpus(idx.Funcs)
	p := corpus.Score(idx.Funcs, DefaultWeights(), 0, 1)
	if !p.Sig.PlatSibling {
		t.Fatal("identical darwin/!darwin declarations were not detected as platform siblings")
	}
	if p.Score >= 0.2 {
		t.Errorf("platform siblings scored %.3f, want heavily down-weighted", p.Score)
	}
}

func TestCrossPackageOutranksSamePackage(t *testing.T) {
	body := `import "os"

func write(path string, data []byte) error {
	tmp, err := os.CreateTemp("", "x")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
`
	same := indexSource(t, map[string]string{
		"a.go": "package p\n\n" + body,
		"b.go": "package p\n\n" + strings.Replace(body, "func write(", "func store(", 1),
	})
	cross := indexSourceDirs(t, map[string]map[string]string{
		"p": {"a.go": "package p\n\n" + body},
		"q": {"b.go": "package q\n\n" + strings.Replace(body, "func write(", "func store(", 1)},
	})

	sameScore := scoreFirstPair(t, same)
	crossScore := scoreFirstPair(t, cross)
	if crossScore <= sameScore {
		t.Errorf("cross-package %.3f must outrank same-package %.3f", crossScore, sameScore)
	}
}

func TestDetectsAtomicWriteCopyByCallOverlapAlone(t *testing.T) {
	// The two bodies share no distinctive identifier and no literal. Only the
	// call signal and the signature can connect them, which is exactly the
	// case token-based detectors miss.
	a := `package p

import (
	"io/fs"
	"os"
	"path/filepath"
)

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "stage")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
`
	b := `package q

import (
	"os"
	"path/filepath"
)

func publish(target string, blob []byte, mode os.FileMode) error {
	parent := filepath.Dir(target)
	staged, err := os.CreateTemp(parent, "scratch")
	if err != nil {
		return err
	}
	if _, err := staged.Write(blob); err != nil {
		return err
	}
	if err := staged.Chmod(mode); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(staged.Name(), target)
}
`
	idx := indexSourceDirs(t, map[string]map[string]string{
		"p": {"a.go": a},
		"q": {"b.go": b},
	})
	corpus := NewCorpus(idx.Funcs)
	p := corpus.Score(idx.Funcs, DefaultWeights(), 0, 1)
	if p.Sig.Calls < 0.8 {
		t.Errorf("call signal %.3f, want a strong match", p.Sig.Calls)
	}
	if p.Score < 0.45 {
		t.Errorf("score %.3f, want at or above the default threshold", p.Score)
	}
	for _, want := range []string{"os.CreateTemp", "os.Rename"} {
		if !slices.Contains(p.Sig.SharedCalls, want) {
			t.Errorf("shared calls %v, want to contain %q", p.Sig.SharedCalls, want)
		}
	}
}

func TestClusteringGroupsFourCopiesAsOne(t *testing.T) {
	body := func(pkg, fn string) string {
		return "package " + pkg + `

import "os"

func ` + fn + `(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(nil); err != nil {
		return false
	}
	return true
}
`
	}
	idx := indexSourceDirs(t, map[string]map[string]string{
		"a": {"a.go": body("a", "alive")},
		"b": {"b.go": body("b", "running")},
		"c": {"c.go": body("c", "isUp")},
		"d": {"d.go": body("d", "liveProcess")},
	})
	corpus := NewCorpus(idx.Funcs)
	var pairs []Pair
	for i := range idx.Funcs {
		for j := i + 1; j < len(idx.Funcs); j++ {
			pairs = append(pairs, corpus.Score(idx.Funcs, DefaultWeights(), i, j))
		}
	}
	clusters := BuildClusters(idx.Funcs, pairs, corpus, 0.45)
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters, want the four copies grouped into 1", len(clusters))
	}
	if n := len(clusters[0].Members); n != 4 {
		t.Errorf("cluster has %d members, want 4", n)
	}
	if clusters[0].Chained {
		t.Error("a fully connected clone set must not be labelled chained")
	}
	if clusters[0].Cohesion < 0.99 {
		t.Errorf("cohesion %.2f, want fully connected", clusters[0].Cohesion)
	}
}

func TestChainedNeighbourhoodIsLabelled(t *testing.T) {
	// A long path graph with no shortcuts is a neighbourhood, not a clone set.
	// A SHORT path is not labelled: four members joined by three edges still
	// has cohesion 0.5, which is a plausible clone set, and the floor is set so
	// that only genuine chaining trips it.
	const n = 8
	var funcs []*Func
	for i := range n {
		funcs = append(funcs, &Func{
			Pkg:   string(rune('a' + i)),
			File:  string(rune('a'+i)) + ".go",
			Name:  string(rune('a' + i)),
			Kinds: map[string]float64{},
		})
	}
	corpus := NewCorpus(funcs)
	var pairs []Pair
	for i := range n - 1 {
		pairs = append(pairs, Pair{A: i, B: i + 1, Score: 0.6, Sig: Signals{CrossPkg: true}})
	}
	clusters := BuildClusters(funcs, pairs, corpus, 0.45)
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(clusters))
	}
	if !clusters[0].Chained {
		t.Errorf("cohesion %.2f over %d members should be labelled chained", clusters[0].Cohesion, n)
	}
}

func TestTestFilesExcludedByDefault(t *testing.T) {
	files := map[string]string{
		"a.go":      "package p\n\nfunc f() { _ = 1; _ = 2; _ = 3 }\n",
		"a_test.go": "package p\n\nfunc g() { _ = 1; _ = 2; _ = 3 }\n",
	}
	if got := len(indexSource(t, files).Funcs); got != 1 {
		t.Errorf("indexed %d funcs with tests excluded, want 1", got)
	}
	dir := writeTree(t, map[string]map[string]string{".": files})
	idx, err := BuildIndex(dir, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Funcs) != 2 {
		t.Errorf("indexed %d funcs with -include-tests, want 2", len(idx.Funcs))
	}
}

func TestNestedModuleIsSkipped(t *testing.T) {
	dir := writeTree(t, map[string]map[string]string{
		".":     {"a.go": "package p\n\nfunc f() { _ = 1; _ = 2; _ = 3 }\n"},
		"tools": {"go.mod": "module x\n\ngo 1.25\n", "b.go": "package main\n\nfunc g() { _ = 1; _ = 2; _ = 3 }\n"},
	})
	idx, err := BuildIndex(dir, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Funcs) != 1 {
		t.Errorf("indexed %d funcs, want 1 (the nested module must be skipped)", len(idx.Funcs))
	}
}

func TestGroupConstsFindsValuesSharedAcrossPackages(t *testing.T) {
	defs := []*ConstDef{
		{Pkg: "internal/diskarb", File: "a.go", Name: "DevDir", Value: "/dev/"},
		{Pkg: "internal/cartridge", File: "b.go", Name: "devDir", Value: "/dev/"},
		{Pkg: "internal/diskarb", File: "a.go", Name: "alsoHere", Value: "/dev/"},
		{Pkg: "internal/diskarb", File: "a.go", Name: "onlyMine", Value: "solo"},
	}
	groups := GroupConsts(defs, 2)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Value != "/dev/" {
		t.Errorf("value = %q, want %q", groups[0].Value, "/dev/")
	}
	if len(groups[0].Defs) != 3 || len(groups[0].Packages) != 2 {
		t.Errorf("got %d defs in %d packages, want 3 in 2", len(groups[0].Defs), len(groups[0].Packages))
	}
}

func TestIDFSuppressesUbiquitousTokens(t *testing.T) {
	funcs := []*Func{}
	for i := range 50 {
		f := &Func{Pkg: "p", Name: "f", Kinds: map[string]float64{}, Names: []string{"get", "path"}}
		if i < 2 {
			f.Names = append(f.Names, "bsd")
		}
		funcs = append(funcs, f)
	}
	c := NewCorpus(funcs)
	if c.NameIDF["bsd"] <= c.NameIDF["get"] {
		t.Errorf("rare token bsd (%.3f) must outweigh ubiquitous get (%.3f)",
			c.NameIDF["bsd"], c.NameIDF["get"])
	}
}

// -- helpers ---------------------------------------------------------------

func scoreFirstPair(t *testing.T, idx *Index) float64 {
	t.Helper()
	if len(idx.Funcs) != 2 {
		t.Fatalf("indexed %d funcs, want 2", len(idx.Funcs))
	}
	return NewCorpus(idx.Funcs).Score(idx.Funcs, DefaultWeights(), 0, 1).Score
}

func indexSource(t *testing.T, files map[string]string) *Index {
	t.Helper()
	return indexSourceDirs(t, map[string]map[string]string{".": files})
}

func indexSourceDirs(t *testing.T, dirs map[string]map[string]string) *Index {
	t.Helper()
	root := writeTree(t, dirs)
	idx, err := BuildIndex(root, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func writeTree(t *testing.T, dirs map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dir, files := range dirs {
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			write(t, filepath.Join(full, name), body)
		}
	}
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return b
}
