package vm_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AGENTS.md section 4.2 requires every darwin-only exported name to have a
// non-darwin counterpart, so the Linux build keeps compiling. Nothing enforced
// it: the rule held only for names something outside darwin happened to CALL,
// so a method nobody called off darwin could go years without a stub. Two did —
// StopWithTimeout and LastStopOutcome — and both surfaced by accident.
//
// This check DERIVES the two method sets from the source instead of listing
// them. A test that enumerates what already exists can never fail for something
// new, which is the defect this repo keeps rediscovering; deriving is what makes
// the check hold for the next method rather than the last one.
//
// It reads SOURCE rather than compiled types on purpose. An untagged Go file
// compiles against one build at a time, so a compile-time assertion — including
// the `var _ interface{...}` shape that looks like the obvious answer — only
// ever sees the platform it was built for and can never compare darwin against
// non-darwin. Parsing sidesteps that: both sets are visible from either OS.
//
// WHAT IT ASSUMES, plainly. It is derived, not omniscient. It recognizes a
// method by the spelling of its receiver type (Runner / *Runner) and a file's
// platform by its //go:build constraint or its _darwin.go suffix. A darwin-only
// method would still slip past if it were declared on an aliased or embedded
// receiver, or in a file that is darwin-only for some reason not visible in its
// build constraint. That is narrower than "the name is not in my list", but it
// is not nothing, and it is the honest boundary of this check.
//
// Scope is deliberately Runner alone. The same walk widens to every exported
// type by relaxing one filter; that is a separate change.

// vmPackageDir is the package under test. The test binary runs with its working
// directory set to the package directory, so the sources are alongside it.
const vmPackageDir = "."

// platform is which build a file belongs to.
type platform int

const (
	platformAny platform = iota
	platformDarwin
	platformOther
)

// receiverTypeName returns the bare type name of a method receiver, following
// the pointer if there is one, or "" for a shape this check does not read.
func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// platformOf classifies a file by its //go:build constraint, falling back to
// the _darwin.go filename convention. Only comments ABOVE the package clause
// count: a //go:build line further down is not a build constraint.
func platformOf(path string, f *ast.File) platform {
	for _, group := range f.Comments {
		if group.Pos() > f.Package {
			break
		}
		for _, c := range group.List {
			text, ok := strings.CutPrefix(c.Text, "//go:build")
			if !ok {
				continue
			}
			switch {
			case strings.Contains(text, "!darwin"):
				return platformOther
			case strings.Contains(text, "darwin"):
				return platformDarwin
			}
		}
	}
	if strings.HasSuffix(filepath.Base(path), "_darwin.go") {
		return platformDarwin
	}
	return platformAny
}

// runnerMethodsByPlatform returns the exported Runner methods declared for
// darwin, and those declared for non-darwin.
func runnerMethodsByPlatform(t *testing.T) (darwin map[string]string, other map[string]bool) {
	t.Helper()
	darwin, other = map[string]string{}, map[string]bool{}

	entries, err := os.ReadDir(vmPackageDir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(vmPackageDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		p := platformOf(path, f)
		if p == platformAny {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if receiverTypeName(fn.Recv.List[0].Type) != "Runner" || !fn.Name.IsExported() {
				continue
			}
			if p == platformDarwin {
				darwin[fn.Name.Name] = name
			} else {
				other[fn.Name.Name] = true
			}
		}
	}
	return darwin, other
}

// Every exported Runner method that exists on darwin must also exist off it, or
// the Linux build breaks the moment anything calls it.
func TestEveryDarwinRunnerMethodHasANonDarwinStub(t *testing.T) {
	darwin, other := runnerMethodsByPlatform(t)

	// Guard against a silently empty walk. A parse that found nothing would
	// satisfy the subset check vacuously, which is the exact failure mode this
	// file exists to remove — a check that cannot fail is not a check.
	if len(darwin) == 0 {
		t.Fatal("found no exported darwin Runner methods; the source walk is broken, not the parity")
	}
	if len(other) == 0 {
		t.Fatal("found no exported non-darwin Runner methods; the source walk is broken, not the parity")
	}

	var missing []string
	for name, file := range darwin {
		if !other[name] {
			missing = append(missing, name+" (declared in "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("exported Runner methods exist on darwin with no non-darwin counterpart:\n  %s\n"+
			"AGENTS.md section 4.2: add a stub to runner_unsupported.go so the Linux build keeps compiling.",
			strings.Join(missing, "\n  "))
	}
}
