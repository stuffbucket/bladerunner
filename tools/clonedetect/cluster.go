package main

import (
	"sort"
)

// Cluster is a group of functions joined transitively by pairs above the
// threshold. Grouping matters for the report: a concept copied four times
// produces six pairs, and six near-identical findings bury everything below
// them. One cluster of four is one decision for the reader.
type Cluster struct {
	Score       float64   `json:"score"`
	Members     []*Func   `json:"members"`
	Packages    []string  `json:"packages"`
	CrossPkg    bool      `json:"cross_pkg"`
	PlatSibling bool      `json:"platform_sibling"`
	Concept     []string  `json:"concept"`
	Signals     Signals   `json:"signals"`
	Edges       int       `json:"edges"`
	Cohesion    float64   `json:"cohesion"`
	Chained     bool      `json:"chained"`
	Evidence    Evidence  `json:"evidence"`
	Fired       []string  `json:"fired"`
	Pairs       []PairRef `json:"pairs,omitempty"`
}

// PairRef names one edge of a cluster for the JSON consumer.
type PairRef struct {
	A     string  `json:"a"`
	B     string  `json:"b"`
	Score float64 `json:"score"`
}

// Evidence is the union of the shared tokens over the cluster's edges, ordered
// most distinctive first.
type Evidence struct {
	Calls []string `json:"calls,omitempty"`
	Lits  []string `json:"lits,omitempty"`
	Names []string `json:"names,omitempty"`
}

// firedThreshold is the value at which a signal is called out by name as having
// contributed. It is a display choice only and does not affect ranking.
const firedThreshold = 0.40

// cohesionFloor separates a genuine clone set from a CHAIN. Transitive grouping
// joins A to C whenever A~B and B~C, so as the threshold falls the whole corpus
// eventually collapses into one component. Cohesion — the fraction of member
// pairs that are actually edges — measures that directly: a real 4-copy concept
// is close to fully connected, whereas a chain of 200 has cohesion near zero.
// A cluster below the floor is still reported, but labelled, because "these 17
// functions are in one neighbourhood" is a weaker claim than "these 4 are the
// same function" and must not be presented as the same thing.
const cohesionFloor = 0.34

type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

// BuildClusters groups the pairs at or above minScore and summarises each
// group.
func BuildClusters(funcs []*Func, pairs []Pair, corpus *Corpus, minScore float64) []*Cluster {
	uf := newUnionFind(len(funcs))
	var kept []Pair
	for _, p := range pairs {
		if p.Score < minScore {
			continue
		}
		kept = append(kept, p)
		uf.union(p.A, p.B)
	}

	byRoot := map[int][]Pair{}
	for _, p := range kept {
		r := uf.find(p.A)
		byRoot[r] = append(byRoot[r], p)
	}

	var out []*Cluster
	for _, edges := range byRoot {
		out = append(out, summarise(funcs, edges, corpus))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CrossPkg != out[j].CrossPkg {
			return out[i].CrossPkg
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Members[0].Ref() < out[j].Members[0].Ref()
	})
	return out
}

// summarise reduces a group's edges to one reportable finding. The cluster
// score is the STRONGEST edge, not the mean: a chain joined through one weak
// link should still be judged by the evidence that is actually there, and the
// members list shows the reader the whole chain either way.
func summarise(funcs []*Func, edges []Pair, corpus *Corpus) *Cluster {
	c := &Cluster{Edges: len(edges)}
	memberSet := map[int]bool{}
	callW, litW, nameW := map[string]float64{}, map[string]float64{}, map[string]float64{}

	var best Pair
	for i, p := range edges {
		memberSet[p.A] = true
		memberSet[p.B] = true
		if i == 0 || p.Score > best.Score {
			best = p
		}
		c.Score = max(c.Score, p.Score)
		c.CrossPkg = c.CrossPkg || p.Sig.CrossPkg
		c.PlatSibling = c.PlatSibling || p.Sig.PlatSibling
		for _, t := range p.Sig.SharedCalls {
			callW[t] = corpus.CallIDF[t]
		}
		for _, t := range p.Sig.SharedLits {
			litW[t] = corpus.LitIDF[t]
		}
		for _, t := range p.Sig.SharedNames {
			nameW[t] = corpus.NameIDF[t]
		}
	}
	c.Signals = best.Sig

	ids := make([]int, 0, len(memberSet))
	for id := range memberSet {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	pkgSet := map[string]bool{}
	for _, id := range ids {
		c.Members = append(c.Members, funcs[id])
		pkgSet[funcs[id].Pkg] = true
	}
	sort.Slice(c.Members, func(i, j int) bool { return c.Members[i].Ref() < c.Members[j].Ref() })
	for p := range pkgSet {
		c.Packages = append(c.Packages, p)
	}
	sort.Strings(c.Packages)

	const evidenceLimit = 6
	c.Evidence.Calls = topByWeight(callW, evidenceLimit)
	c.Evidence.Lits = topByWeight(litW, evidenceLimit)
	c.Evidence.Names = topByWeight(nameW, evidenceLimit)

	if n := len(c.Members); n > 1 {
		c.Cohesion = float64(len(edges)) / (float64(n) * float64(n-1) / 2)
		c.Chained = n > 2 && c.Cohesion < cohesionFloor
	}

	// The concept guess is simply the most distinctive shared vocabulary,
	// drawn from all three token spaces at once. It is a label, not a claim.
	merged := map[string]float64{}
	for t, w := range callW {
		merged[trimCallToken(t)] = w * 1.1
	}
	for t, w := range litW {
		merged[t] = max(merged[t], w)
	}
	for t, w := range nameW {
		merged[t] = max(merged[t], w)
	}
	c.Concept = topByWeight(merged, 4)

	for _, s := range []struct {
		name string
		v    float64
	}{
		{"calls", c.Signals.Calls},
		{"names", c.Signals.Names},
		{"sig", c.Signals.Sig},
		{"lits", c.Signals.Lits},
		{"struct", c.Signals.Struct},
	} {
		if s.v >= firedThreshold {
			c.Fired = append(c.Fired, s.name)
		}
	}

	for _, p := range edges {
		c.Pairs = append(c.Pairs, PairRef{A: funcs[p.A].Ref(), B: funcs[p.B].Ref(), Score: p.Score})
	}
	sort.Slice(c.Pairs, func(i, j int) bool { return c.Pairs[i].Score > c.Pairs[j].Score })
	return c
}

func trimCallToken(t string) string {
	if len(t) > 0 && t[0] == '.' {
		return t[1:]
	}
	return t
}

func topByWeight(m map[string]float64, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// ConstGroup is a string value declared as a constant in more than one package.
// Constants have no body, so none of the five function signals apply to them,
// but a value spelled out in four packages is the same failure in a simpler
// form.
type ConstGroup struct {
	Value    string      `json:"value"`
	Packages []string    `json:"packages"`
	Defs     []*ConstDef `json:"defs"`
}

// GroupConsts finds string constant values declared in at least minPkgs
// distinct packages.
func GroupConsts(defs []*ConstDef, minPkgs int) []*ConstGroup {
	byValue := map[string][]*ConstDef{}
	for _, d := range defs {
		byValue[d.Value] = append(byValue[d.Value], d)
	}
	var out []*ConstGroup
	for v, ds := range byValue {
		pkgSet := map[string]bool{}
		for _, d := range ds {
			pkgSet[d.Pkg] = true
		}
		if len(pkgSet) < minPkgs {
			continue
		}
		g := &ConstGroup{Value: v, Defs: ds}
		for p := range pkgSet {
			g.Packages = append(g.Packages, p)
		}
		sort.Strings(g.Packages)
		sort.Slice(g.Defs, func(i, j int) bool { return g.Defs[i].File < g.Defs[j].File })
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Defs) != len(out[j].Defs) {
			return len(out[i].Defs) > len(out[j].Defs)
		}
		return out[i].Value < out[j].Value
	})
	return out
}
