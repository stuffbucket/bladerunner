package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Func is one indexed top-level function or method, reduced to the feature
// vectors the scorer compares. Everything expensive (parsing, walking) happens
// once here; scoring only intersects sets.
type Func struct {
	Pkg   string `json:"pkg"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Name  string `json:"name"`
	Recv  string `json:"recv,omitempty"`
	Sig   string `json:"sig"`
	Plat  string `json:"plat,omitempty"`
	Stmts int    `json:"stmts"`
	Nodes int    `json:"nodes"`
	Depth int    `json:"depth"`

	// Feature bags. SigBag is a multiset rendered as a sorted slice with
	// duplicates retained; the rest are deduplicated token sets.
	SigBag []string           `json:"-"`
	Names  []string           `json:"-"`
	Calls  []string           `json:"-"`
	Lits   []string           `json:"-"`
	Kinds  map[string]float64 `json:"-"`
}

// Ref renders the file:line the human output and editors want.
func (f *Func) Ref() string { return fmt.Sprintf("%s:%d", f.File, f.Line) }

// Label renders the declaration the way a Go programmer names it.
func (f *Func) Label() string {
	if f.Recv != "" {
		return "(" + f.Recv + ")." + f.Name
	}
	return f.Name
}

// ConstDef is a package-level constant bound to a string literal. Duplicated
// literal VALUES across packages are reported separately from function
// clusters: a constant has no body, so none of the body signals apply, but a
// value re-declared in four packages is exactly the divergence this tool exists
// to find.
type ConstDef struct {
	Pkg   string `json:"pkg"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Index is the whole corpus.
type Index struct {
	Funcs   []*Func
	Consts  []*ConstDef
	Files   int
	Skipped int
}

// skipDirNames are directories that never hold first-party Go worth indexing.
// Hidden directories and nested modules are skipped by rule, not by name.
var skipDirNames = map[string]bool{
	"testdata":     true,
	"node_modules": true,
	"vendor":       true,
	"bin":          true,
	"site":         true,
}

// buildLineRe finds the //go:build constraint. Only the first match in the
// first few kilobytes counts, so a string literal deeper in the file that
// happens to contain the text cannot be mistaken for a constraint.
var buildLineRe = regexp.MustCompile(`(?m)^//go:build (.+)$`)

// generatedRe marks machine-written files. Generated code is repetitive by
// construction and its duplication is not actionable.
var generatedRe = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// platformSuffixes are the file-name suffixes the go tool treats as an implicit
// build constraint. Only the ones this tree can plausibly use are listed;
// an unknown suffix simply yields no platform key, which is the safe answer.
var platformSuffixes = map[string]bool{
	"darwin": true, "linux": true, "windows": true, "freebsd": true,
	"openbsd": true, "netbsd": true, "dragonfly": true, "solaris": true,
	"android": true, "ios": true, "js": true, "wasip1": true, "plan9": true,
	"aix": true, "unix": true,
	"amd64": true, "arm64": true, "arm": true, "386": true,
	"riscv64": true, "ppc64": true, "ppc64le": true, "s390x": true,
	"wasm": true, "mips": true, "mips64": true, "loong64": true,
	"cgo": true,
}

// BuildIndex parses every Go file under root and reduces each top-level
// function to its feature vectors.
//
// Files are grouped by directory so that package-level constants can be
// resolved before the function bodies that reference them are walked: a body
// that says devDir must contribute the literal "/dev/" to the literal signal,
// otherwise naming the constant would hide the very duplication being hunted.
func BuildIndex(root string, includeTests bool, minStmts int) (*Index, error) {
	byDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || skipDirNames[name] {
				return filepath.SkipDir
			}
			// A directory with its own go.mod is a different module and is
			// not part of this corpus. This is what excludes clonedetect
			// itself.
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		byDir[dir] = append(byDir[dir], path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	idx := &Index{}
	fset := token.NewFileSet()
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		files := byDir[dir]
		sort.Strings(files)
		type parsedFile struct {
			path string
			rel  string
			ast  *ast.File
			plat string
		}
		var parsed []parsedFile
		consts := map[string]string{}

		for _, path := range files {
			src, readErr := os.ReadFile(path) //nolint:gosec // paths come from a directory walk
			if readErr != nil {
				idx.Skipped++
				continue
			}
			if generatedRe.Match(src) {
				idx.Skipped++
				continue
			}
			af, parseErr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if parseErr != nil {
				idx.Skipped++
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			parsed = append(parsed, parsedFile{path: path, rel: rel, ast: af, plat: platformKey(path, src)})
			idx.Files++
		}

		pkgRel, relErr := filepath.Rel(root, dir)
		if relErr != nil {
			pkgRel = dir
		}
		if pkgRel == "." {
			pkgRel = "(root)"
		}

		for _, pf := range parsed {
			collectConsts(fset, pf.ast, pkgRel, pf.rel, consts, &idx.Consts)
		}
		for _, pf := range parsed {
			for _, decl := range pf.ast.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				fn := extractFunc(fset, fd, pkgRel, pf.rel, pf.plat, consts, importNames(pf.ast))
				if fn.Stmts < minStmts {
					continue
				}
				idx.Funcs = append(idx.Funcs, fn)
			}
		}
	}
	return idx, nil
}

// platformKey renders the build constraints of a file as a comparable string.
// Two files in one package whose keys differ are platform siblings: the go tool
// will never compile both, so an identical function in each is deliberate, not
// duplication.
func platformKey(path string, src []byte) string {
	head := src
	const headLimit = 4096
	if len(head) > headLimit {
		head = head[:headLimit]
	}
	var parts []string
	if m := buildLineRe.FindSubmatch(head); m != nil {
		parts = append(parts, "tag:"+strings.TrimSpace(string(m[1])))
	}
	base := strings.TrimSuffix(filepath.Base(path), ".go")
	base = strings.TrimSuffix(base, "_test")
	if i := strings.LastIndex(base, "_"); i >= 0 && platformSuffixes[base[i+1:]] {
		parts = append(parts, "suffix:"+base[i+1:])
	}
	return strings.Join(parts, ";")
}

// collectConsts records package-level constants bound to a string literal, both
// into a name->value map used to resolve identifiers inside bodies and into the
// index's own list for the duplicated-constant report.
func collectConsts(fset *token.FileSet, af *ast.File, pkg, rel string, into map[string]string, defs *[]*ConstDef) {
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil || len(val) < 2 {
				continue
			}
			into[vs.Names[0].Name] = val
			if gd.Tok == token.CONST {
				*defs = append(*defs, &ConstDef{
					Pkg:   pkg,
					File:  rel,
					Line:  fset.Position(vs.Names[0].Pos()).Line,
					Name:  vs.Names[0].Name,
					Value: val,
				})
			}
		}
	}
}

// extractFunc reduces one declaration to its feature vectors.
func extractFunc(fset *token.FileSet, fd *ast.FuncDecl, pkg, rel, plat string, consts map[string]string, imports map[string]bool) *Func {
	fn := &Func{
		Pkg:   pkg,
		File:  rel,
		Line:  fset.Position(fd.Pos()).Line,
		Name:  fd.Name.Name,
		Plat:  plat,
		Kinds: map[string]float64{},
	}

	params := []string{}
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		fn.Recv = typeString(fd.Recv.List[0].Type)
		// The receiver folds in as a leading parameter, so a method and the
		// plain function that does the same job compare equal.
		params = append(params, fn.Recv)
	}
	params = append(params, fieldTypes(fd.Type.Params)...)
	results := fieldTypes(fd.Type.Results)
	fn.Sig = "(" + strings.Join(params, ", ") + ") -> (" + strings.Join(results, ", ") + ")"
	for _, p := range params {
		fn.SigBag = append(fn.SigBag, "p:"+p)
	}
	for _, r := range results {
		fn.SigBag = append(fn.SigBag, "r:"+r)
	}
	sort.Strings(fn.SigBag)

	names := map[string]bool{}
	addTokens(names, fd.Name.Name)
	addTokens(names, strings.TrimPrefix(fn.Recv, "*"))
	addFieldNames(names, fd.Type.Params)
	addFieldNames(names, fd.Type.Results)

	calls := map[string]bool{}
	lits := map[string]bool{}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		fn.Nodes++
		if _, ok := n.(ast.Stmt); ok {
			fn.Stmts++
		}
		fn.Kinds[kindOf(n)]++

		switch x := n.(type) {
		case *ast.CallExpr:
			recordCall(calls, imports, x.Fun)
		case *ast.BasicLit:
			recordLiteral(lits, x)
		case *ast.Ident:
			// A body that names a package-level string constant contributes
			// that constant's VALUE, so naming "/dev/" does not hide it.
			if v, ok := consts[x.Name]; ok {
				lits[v] = true
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						addTokens(names, id.Name)
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				addTokens(names, id.Name)
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{x.Key, x.Value} {
				if id, ok := e.(*ast.Ident); ok {
					addTokens(names, id.Name)
				}
			}
		}
		return true
	})

	best := 0
	ast.Walk(depthVisitor{best: &best}, fd.Body)
	fn.Depth = best

	fn.Names = sortedKeys(names)
	fn.Calls = sortedKeys(calls)
	fn.Lits = sortedKeys(lits)
	return fn
}

// depthVisitor measures block nesting. It carries its depth by value, so each
// returned visitor descends with its own count and no post-order hook is
// needed.
type depthVisitor struct {
	d    int
	best *int
}

func (v depthVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	if _, ok := n.(*ast.BlockStmt); ok {
		next := v.d + 1
		if next > *v.best {
			*v.best = next
		}
		return depthVisitor{d: next, best: v.best}
	}
	return v
}

// kindOf names an AST node for the structural fingerprint. Operators are folded
// into the kind, because "if x == nil" and "if x > n" are different shapes.
func kindOf(n ast.Node) string {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		return "BinaryExpr" + x.Op.String()
	case *ast.UnaryExpr:
		return "UnaryExpr" + x.Op.String()
	case *ast.BranchStmt:
		return "BranchStmt:" + x.Tok.String()
	case *ast.AssignStmt:
		return "AssignStmt" + x.Tok.String()
	}
	name := fmt.Sprintf("%T", n)
	return strings.TrimPrefix(name, "*ast.")
}

// recordCall records a called symbol.
//
// The bare form (".Sync") is always recorded: it is what lets f.Sync() and
// tmp.Sync() count as the same call even though the receivers are unrelated
// local values, and that is precisely the case two independent copies present.
// The qualified form ("os.CreateTemp") is recorded only when the qualifier is
// an imported PACKAGE. Recording "tmp.Write" as well would fill both vectors
// with tokens keyed on an arbitrary local variable name, which can never match
// across copies and so dilutes the one signal that should be strongest.
func recordCall(into map[string]bool, imports map[string]bool, fun ast.Expr) {
	switch x := fun.(type) {
	case *ast.Ident:
		into[x.Name] = true
	case *ast.SelectorExpr:
		into["."+x.Sel.Name] = true
		if id, ok := x.X.(*ast.Ident); ok && imports[id.Name] {
			into[id.Name+"."+x.Sel.Name] = true
		}
	case *ast.IndexExpr:
		recordCall(into, imports, x.X)
	case *ast.IndexListExpr:
		recordCall(into, imports, x.X)
	case *ast.ParenExpr:
		recordCall(into, imports, x.X)
	}
}

// importNames is the set of package names a file can qualify a call with.
// Versioned module paths ("golang.org/x/tools/go/packages", ".../vz/v3") are
// reduced to the element a caller actually writes.
func importNames(af *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range af.Imports {
		if imp.Name != nil {
			if imp.Name.Name != "_" && imp.Name.Name != "." {
				out[imp.Name.Name] = true
			}
			continue
		}
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		parts := strings.Split(p, "/")
		last := parts[len(parts)-1]
		if len(parts) > 1 && majorVersionRe.MatchString(last) {
			last = parts[len(parts)-2]
		}
		if i := strings.Index(last, "."); i > 0 {
			last = last[:i]
		}
		out[last] = true
	}
	return out
}

var majorVersionRe = regexp.MustCompile(`^v[0-9]+$`)

// recordLiteral records string literals verbatim and numeric literals that are
// not the universally common ones. A shared "0" says nothing; a shared 30*time
// .Second or a shared "/dev/" says a great deal.
func recordLiteral(into map[string]bool, lit *ast.BasicLit) {
	switch lit.Kind {
	case token.STRING:
		v, err := strconv.Unquote(lit.Value)
		if err != nil || len(v) < 2 {
			return
		}
		into[v] = true
	case token.INT, token.FLOAT:
		switch lit.Value {
		case "0", "1", "2", "10", "0.0":
			return
		}
		into["#"+lit.Value] = true
	}
}

// fieldTypes renders a parameter or result list, expanding grouped names so
// "(a, b string)" contributes two entries.
func fieldTypes(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.List {
		t := typeString(f.Type)
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, t)
		}
	}
	return out
}

func addFieldNames(into map[string]bool, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, id := range f.Names {
			addTokens(into, id.Name)
		}
	}
}

// typeString renders a type expression with its package qualifier REMOVED.
//
// Dropping the qualifier is the point: two packages that re-solve one problem
// reach for whichever spelling of a type is in scope, so internal/util takes an
// fs.FileMode where internal/webproxy takes an os.FileMode. Those are the same
// type and must compare equal, and a tool that keeps the qualifier splits the
// pair into different signature buckets and never scores it. The cost is that
// two unrelated Config types collide, which is why the signature is one signal
// among five rather than a gate.
func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + typeString(t.Elt)
		}
		return "[N]" + typeString(t.Elt)
	case *ast.Ellipsis:
		return "[]" + typeString(t.Elt)
	case *ast.MapType:
		return "map[" + typeString(t.Key) + "]" + typeString(t.Value)
	case *ast.ChanType:
		return "chan " + typeString(t.Value)
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any"
		}
		return "interface"
	case *ast.StructType:
		return "struct"
	case *ast.IndexExpr:
		return typeString(t.X)
	case *ast.IndexListExpr:
		return typeString(t.X)
	case *ast.ParenExpr:
		return typeString(t.X)
	}
	return "?"
}

// addTokens splits an identifier into lowercase words and adds them to a set.
func addTokens(into map[string]bool, ident string) {
	for _, tok := range splitIdent(ident) {
		into[tok] = true
	}
}

// splitIdent breaks an identifier on underscores and camelCase boundaries,
// keeping acronym runs whole: BSDName yields "bsd" and "name", not six
// single letters.
func splitIdent(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "_") {
		rs := []rune(part)
		start := 0
		for i := 1; i < len(rs); i++ {
			prev, cur := rs[i-1], rs[i]
			var next rune
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			boundary := false
			switch {
			case unicode.IsLower(prev) && unicode.IsUpper(cur):
				boundary = true
			case unicode.IsUpper(prev) && unicode.IsUpper(cur) && next != 0 && unicode.IsLower(next):
				boundary = true
			case unicode.IsDigit(prev) != unicode.IsDigit(cur):
				boundary = true
			}
			if boundary {
				out = append(out, string(rs[start:i]))
				start = i
			}
		}
		if start < len(rs) {
			out = append(out, string(rs[start:]))
		}
	}
	kept := out[:0]
	for _, tok := range out {
		tok = strings.ToLower(tok)
		if len(tok) < 2 || isAllDigits(tok) {
			continue
		}
		kept = append(kept, tok)
	}
	return kept
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
