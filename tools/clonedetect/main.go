// Command clonedetect finds duplicated CONCEPTS in a Go tree.
//
// It exists because token-based clone detectors cannot see the failure mode
// this repository actually has. Parallel authors who could not read each
// other's work re-solved the same primitives from scratch, so the copies are
// semantically equivalent but textually unrelated, and then they diverged.
// dupl, PMD CPD and jscpd all compare token streams and score such copies at
// zero.
//
// The approach here is to index every top-level function, reduce each to five
// independent feature vectors, and score pairs on a weighted combination:
//
//  1. normalised type signature, package qualifiers stripped, receiver folded in
//  2. inverse-document-frequency-weighted overlap of identifier name tokens
//  3. structural fingerprint of the body as a bag of AST node kinds
//  4. inverse-document-frequency-weighted overlap of called symbols
//  5. overlap of string and numeric literals, with named constants resolved
//
// Pairs above the threshold are joined transitively, so a concept copied four
// times reports as one cluster of four rather than six pairs.
//
// Usage:
//
//	go run ./tools/clonedetect -root . -top 15
//	go run ./tools/clonedetect -json | jq '.clusters[0]'
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Report is the -json document.
type Report struct {
	Root        string        `json:"root"`
	Funcs       int           `json:"funcs"`
	Files       int           `json:"files"`
	Candidates  int           `json:"candidate_pairs"`
	Scored      int           `json:"pairs_above_threshold"`
	MinScore    float64       `json:"min_score"`
	Weights     Weights       `json:"weights"`
	ElapsedMS   int64         `json:"elapsed_ms"`
	Clusters    []*Cluster    `json:"clusters"`
	ConstGroups []*ConstGroup `json:"const_groups,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "clonedetect:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		root         = flag.String("root", ".", "repository root to index")
		minScore     = flag.Float64("min-score", 0.45, "report clusters whose strongest edge reaches this score")
		top          = flag.Int("top", 20, "show at most this many clusters (0 for all)")
		asJSON       = flag.Bool("json", false, "emit JSON instead of the human report")
		includeTests = flag.Bool("include-tests", false, "index _test.go files too (table tests are repetitive by design and drown the signal)")
		minStmts     = flag.Int("min-stmts", 3, "ignore functions with fewer statements than this; below 3 the body signals carry almost no information")
		noConsts     = flag.Bool("no-consts", false, "skip the duplicated-constant report")
		showPairs    = flag.Bool("pairs", false, "list every edge inside each cluster")
		dfCap        = flag.Int("df-cap", 0, "blocking: ignore tokens seen in more than this many functions (0 picks N/40)")
		platPenalty  = flag.Float64("platform-penalty", DefaultWeights().Platform, "multiplier for darwin/!darwin sibling pairs; set to 1 to disable the correction")
		samePkgMul   = flag.Float64("same-package-penalty", DefaultWeights().SamePackage, "multiplier for pairs inside one package")
	)
	flag.Parse()

	start := time.Now()
	idx, err := BuildIndex(*root, *includeTests, *minStmts)
	if err != nil {
		return err
	}
	if len(idx.Funcs) == 0 {
		return fmt.Errorf("no functions indexed under %s", *root)
	}

	corpus := NewCorpus(idx.Funcs)
	tokenCap := *dfCap
	if tokenCap <= 0 {
		tokenCap = max(4, len(idx.Funcs)/40)
	}
	// Signature buckets get a larger cap than token postings: a bucket like
	// "(string) -> (error)" is broad but still narrows the field usefully,
	// while an unbounded one would be quadratic on its own.
	candidates := Candidates(idx.Funcs, corpus, tokenCap, 250)

	weights := DefaultWeights()
	weights.Platform = *platPenalty
	weights.SamePackage = *samePkgMul
	pairs := make([]Pair, 0, len(candidates))
	for key := range candidates {
		p := corpus.Score(idx.Funcs, weights, key[0], key[1])
		if p.Score < *minScore {
			continue
		}
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Score > pairs[j].Score })

	clusters := BuildClusters(idx.Funcs, pairs, corpus, *minScore)

	var constGroups []*ConstGroup
	if !*noConsts {
		constGroups = GroupConsts(idx.Consts, 2)
	}

	rep := &Report{
		Root:        *root,
		Funcs:       len(idx.Funcs),
		Files:       idx.Files,
		Candidates:  len(candidates),
		Scored:      len(pairs),
		MinScore:    *minScore,
		Weights:     weights,
		Clusters:    clusters,
		ConstGroups: constGroups,
	}
	if *top > 0 && len(rep.Clusters) > *top {
		rep.Clusters = rep.Clusters[:*top]
	}
	if !*showPairs && !*asJSON {
		for _, c := range rep.Clusters {
			c.Pairs = nil
		}
	}
	rep.ElapsedMS = time.Since(start).Milliseconds()

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	writeText(os.Stdout, rep, clusters, *showPairs)
	return nil
}

func writeText(w *os.File, rep *Report, all []*Cluster, showPairs bool) {
	fmt.Fprintf(w, "clonedetect  %d functions in %d files  %d candidate pairs  %d edges >= %.2f  %dms\n",
		rep.Funcs, rep.Files, rep.Candidates, rep.Scored, rep.MinScore, rep.ElapsedMS)
	fmt.Fprintf(w, "weights: calls %.2f  names %.2f  sig %.2f  lits %.2f  struct %.2f\n",
		rep.Weights.Calls, rep.Weights.Names, rep.Weights.Sig, rep.Weights.Lits, rep.Weights.Struct)
	fmt.Fprintf(w, "%d clusters total, showing %d\n\n", len(all), len(rep.Clusters))

	for i, c := range rep.Clusters {
		scope := "cross-package"
		if !c.CrossPkg {
			scope = "same-package"
		}
		flags := ""
		if c.PlatSibling {
			flags += fmt.Sprintf("  [platform-sibling: down-weighted x%.2f]", rep.Weights.Platform)
		}
		if c.Chained {
			flags += fmt.Sprintf("  [chained: cohesion %.2f, a neighbourhood not a clone set]", c.Cohesion)
		}
		fmt.Fprintf(w, "#%-3d score %.3f  %s  %d members%s\n", i+1, c.Score, scope, len(c.Members), flags)
		if len(c.Concept) > 0 {
			fmt.Fprintf(w, "     concept   %s\n", strings.Join(c.Concept, " + "))
		}
		fmt.Fprintf(w, "     signals   calls %s  names %s  sig %s  lits %s  struct %s\n",
			mark(c.Signals.Calls), mark(c.Signals.Names), mark(c.Signals.Sig),
			mark(c.Signals.Lits), mark(c.Signals.Struct))
		if len(c.Fired) > 0 {
			fmt.Fprintf(w, "     fired     %s\n", strings.Join(c.Fired, ", "))
		}
		if len(c.Evidence.Calls) > 0 {
			fmt.Fprintf(w, "     calls     %s\n", strings.Join(c.Evidence.Calls, ", "))
		}
		if len(c.Evidence.Lits) > 0 {
			fmt.Fprintf(w, "     literals  %s\n", strings.Join(quoteAll(c.Evidence.Lits), ", "))
		}
		if len(c.Evidence.Names) > 0 {
			fmt.Fprintf(w, "     names     %s\n", strings.Join(c.Evidence.Names, ", "))
		}
		width := 0
		for _, m := range c.Members {
			width = max(width, len(m.Ref()))
		}
		for _, m := range c.Members {
			fmt.Fprintf(w, "     %-*s  %s %s\n", width, m.Ref(), m.Label(), m.Sig)
		}
		if showPairs {
			for _, p := range c.Pairs {
				fmt.Fprintf(w, "       edge %.3f  %s  ~  %s\n", p.Score, p.A, p.B)
			}
		}
		fmt.Fprintln(w)
	}

	if len(rep.ConstGroups) > 0 {
		fmt.Fprintf(w, "duplicated string constants across packages (%d values)\n", len(rep.ConstGroups))
		for _, g := range rep.ConstGroups {
			fmt.Fprintf(w, "  %-24q %d declarations in %d packages\n", g.Value, len(g.Defs), len(g.Packages))
			for _, d := range g.Defs {
				fmt.Fprintf(w, "     %s:%d  %s\n", d.File, d.Line, d.Name)
			}
		}
		fmt.Fprintln(w)
	}
}

// mark renders a signal value and stars the ones that carried the finding, so
// the reason a cluster is on the list is readable at a glance.
func mark(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	if v >= firedThreshold {
		return s + "*"
	}
	return s + " "
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
