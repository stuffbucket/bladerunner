package main

import (
	"math"
	"sort"
)

// Weights is the transparent, tunable combination of the five signals. They sum
// to 1, so a pair score is directly readable as "this fraction of the available
// evidence agrees".
//
// The ordering is chosen on principle, before any result was looked at:
//
//   - Calls weigh most. The set of functions a body invokes is the closest
//     stand-in for what the body DOES, and it is the signal an author cannot
//     avoid matching when re-solving the same problem: two functions that both
//     call os.CreateTemp, Chmod, Sync and os.Rename are the same concept
//     whatever they are called and however they are spelled.
//   - Names weigh second. Suggestive, but the whole premise of this tool is
//     that independently written copies DIVERGE in naming, so names cannot be
//     trusted alone. Inverse document frequency is what makes them usable at
//     all: it discards get/new/path/err and keeps bsd/atomic/veto.
//   - Signature weighs third. It is a prior, not proof. A great many functions
//     share (string) -> (error).
//   - Literals weigh fourth. Very high precision when they fire ("/dev/",
//     ".tmp-*") but they fire rarely, and many real duplicates share none.
//   - Structure weighs least. Node-kind bags are genuinely noisy for short
//     bodies, where a guard clause and a return dominate the vector.
type Weights struct {
	Sig    float64 `json:"sig"`
	Names  float64 `json:"names"`
	Struct float64 `json:"struct"`
	Calls  float64 `json:"calls"`
	Lits   float64 `json:"lits"`

	// SamePackage ranks cross-package pairs above same-package pairs.
	// Duplication inside one package is usually deliberate and always visible
	// to whoever edits it next; duplication across packages is the kind that
	// silently diverges.
	SamePackage float64 `json:"same_package_penalty"`

	// Platform handles the darwin / !darwin pairs. Such siblings are MEANT to
	// share a name and a signature, and the go tool compiles only one of them,
	// so they are not duplication in any actionable sense. They are
	// DOWN-WEIGHTED rather than dropped, and they stay labelled in the output,
	// so a genuinely suspicious pair can still surface. Set it to 1 to see what
	// the report looks like without the correction.
	Platform float64 `json:"platform_sibling_penalty"`
}

// DefaultWeights are the weights described above.
func DefaultWeights() Weights {
	return Weights{
		Calls:  0.30,
		Names:  0.22,
		Sig:    0.20,
		Lits:   0.16,
		Struct: 0.12,

		SamePackage: 0.60,
		Platform:    0.15,
	}
}

// sizeFloor is the worst multiplier a size mismatch can impose. Body size is a
// weak tie-breaker, so it may nudge a ranking but must never sink a pair on its
// own.
const sizeFloor = 0.65

// Corpus holds the inverse document frequencies computed over every indexed
// function. A token's weight is log(1 + N/df): ubiquitous tokens tend to zero
// and a token seen twice in a corpus of a thousand carries roughly its full
// weight.
type Corpus struct {
	N       int
	NameIDF map[string]float64
	CallIDF map[string]float64
	LitIDF  map[string]float64

	nameNorm []float64
	callNorm []float64
	litNorm  []float64
	kindNorm []float64
	sigCount []map[string]int
}

// NewCorpus computes the document frequencies and the per-function vector
// norms.
func NewCorpus(funcs []*Func) *Corpus {
	c := &Corpus{
		N:       len(funcs),
		NameIDF: idf(funcs, len(funcs), func(f *Func) []string { return f.Names }),
		CallIDF: idf(funcs, len(funcs), func(f *Func) []string { return f.Calls }),
		LitIDF:  idf(funcs, len(funcs), func(f *Func) []string { return f.Lits }),
	}
	c.nameNorm = norms(funcs, c.NameIDF, func(f *Func) []string { return f.Names })
	c.callNorm = norms(funcs, c.CallIDF, func(f *Func) []string { return f.Calls })
	c.litNorm = norms(funcs, c.LitIDF, func(f *Func) []string { return f.Lits })

	c.kindNorm = make([]float64, len(funcs))
	c.sigCount = make([]map[string]int, len(funcs))
	for i, f := range funcs {
		var sum float64
		for _, v := range f.Kinds {
			sum += v * v
		}
		c.kindNorm[i] = math.Sqrt(sum)
		m := make(map[string]int, len(f.SigBag))
		for _, t := range f.SigBag {
			m[t]++
		}
		c.sigCount[i] = m
	}
	return c
}

func idf(funcs []*Func, n int, get func(*Func) []string) map[string]float64 {
	df := map[string]int{}
	for _, f := range funcs {
		for _, t := range get(f) {
			df[t]++
		}
	}
	out := make(map[string]float64, len(df))
	for t, d := range df {
		out[t] = math.Log(1 + float64(n)/float64(d))
	}
	return out
}

func norms(funcs []*Func, w map[string]float64, get func(*Func) []string) []float64 {
	out := make([]float64, len(funcs))
	for i, f := range funcs {
		var sum float64
		for _, t := range get(f) {
			sum += w[t] * w[t]
		}
		out[i] = math.Sqrt(sum)
	}
	return out
}

// Signals is the per-pair breakdown. Reporting it is the difference between an
// actionable finding and a mysterious one: a reader must be able to see WHY a
// pair was flagged and dismiss it in a second when the reason is bad.
type Signals struct {
	Sig    float64 `json:"sig"`
	Names  float64 `json:"names"`
	Struct float64 `json:"struct"`
	Calls  float64 `json:"calls"`
	Lits   float64 `json:"lits"`

	SizeMul     float64 `json:"size_mul"`
	CrossPkg    bool    `json:"cross_pkg"`
	PlatSibling bool    `json:"platform_sibling"`

	SharedCalls []string `json:"shared_calls,omitempty"`
	SharedLits  []string `json:"shared_lits,omitempty"`
	SharedNames []string `json:"shared_names,omitempty"`
}

// Pair is a scored candidate.
type Pair struct {
	A, B  int
	Score float64
	Sig   Signals
}

// Score compares two indexed functions.
func (c *Corpus) Score(funcs []*Func, w Weights, i, j int) Pair {
	a, b := funcs[i], funcs[j]
	var s Signals

	s.Sig = multisetJaccard(c.sigCount[i], c.sigCount[j])
	s.Names, s.SharedNames = weightedCosine(a.Names, b.Names, c.NameIDF, c.nameNorm[i], c.nameNorm[j])
	s.Calls, s.SharedCalls = weightedCosine(a.Calls, b.Calls, c.CallIDF, c.callNorm[i], c.callNorm[j])
	s.Lits, s.SharedLits = weightedCosine(a.Lits, b.Lits, c.LitIDF, c.litNorm[i], c.litNorm[j])
	s.Struct = kindCosine(a.Kinds, b.Kinds, c.kindNorm[i], c.kindNorm[j])

	s.CrossPkg = a.Pkg != b.Pkg
	s.PlatSibling = platformSiblings(a, b)

	lo, hi := float64(min(a.Nodes, b.Nodes)), float64(max(a.Nodes, b.Nodes))
	ratio := 1.0
	if hi > 0 {
		ratio = lo / hi
	}
	s.SizeMul = sizeFloor + (1-sizeFloor)*ratio

	score := w.Sig*s.Sig + w.Names*s.Names + w.Struct*s.Struct + w.Calls*s.Calls + w.Lits*s.Lits
	score *= s.SizeMul
	if !s.CrossPkg {
		score *= w.SamePackage
	}
	if s.PlatSibling {
		score *= w.Platform
	}
	return Pair{A: i, B: j, Score: score, Sig: s}
}

// platformSiblings reports whether two declarations are the darwin / !darwin
// halves of one thing rather than two copies of it.
//
// The test is deliberately narrow. It only fires inside a single package, where
// build-tagged siblings actually live, and it needs a real disagreement in
// constraints: either both files carry a constraint and the constraints differ,
// or the two declarations share a name and exactly one file is constrained
// (the plain foo.go / foo_darwin.go arrangement).
func platformSiblings(a, b *Func) bool {
	if a.Pkg != b.Pkg || a.Plat == b.Plat {
		return false
	}
	if a.Plat != "" && b.Plat != "" {
		return true
	}
	return a.Name == b.Name
}

// multisetJaccard compares two signature bags, so an extra parameter costs
// something but does not zero the signal.
func multisetJaccard(x, y map[string]int) float64 {
	if len(x) == 0 && len(y) == 0 {
		return 1
	}
	inter, union := 0, 0
	for k, xv := range x {
		yv := y[k]
		inter += min(xv, yv)
		union += max(xv, yv)
	}
	for k, yv := range y {
		if _, ok := x[k]; !ok {
			union += yv
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// weightedCosine is cosine similarity over binary term frequency with inverse
// document frequency weights. It returns the shared tokens ordered by weight,
// most distinctive first, because those tokens ARE the explanation.
func weightedCosine(a, b []string, w map[string]float64, na, nb float64) (float64, []string) {
	if na == 0 || nb == 0 {
		return 0, nil
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[t] = struct{}{}
	}
	var dot float64
	var shared []string
	for _, t := range b {
		if _, ok := set[t]; !ok {
			continue
		}
		weight := w[t]
		dot += weight * weight
		shared = append(shared, t)
	}
	sort.Slice(shared, func(i, j int) bool {
		if w[shared[i]] != w[shared[j]] {
			return w[shared[i]] > w[shared[j]]
		}
		return shared[i] < shared[j]
	})
	return dot / (na * nb), shared
}

// kindCosine compares structural fingerprints. Counts are used directly and the
// vectors are length-normalised, so the shape matters and the size does not.
func kindCosine(a, b map[string]float64, na, nb float64) float64 {
	if na == 0 || nb == 0 {
		return 0
	}
	var dot float64
	for k, av := range a {
		if bv, ok := b[k]; ok {
			dot += av * bv
		}
	}
	return dot / (na * nb)
}

// Candidates generates the pairs worth scoring.
//
// All-pairs over a few thousand functions is affordable, but blocking is both
// faster and more honest about what the tool can see: a pair that shares no
// distinctive call, literal, name token or signature has no evidence behind it
// and would score near zero anyway.
//
// A posting list longer than dfCap is skipped, because a token that common
// carries no information and would otherwise generate a quadratic blow-up.
func Candidates(funcs []*Func, corpus *Corpus, dfCap, sigCap int) map[[2]int]struct{} {
	post := map[string][]int{}
	add := func(key string, i int) { post[key] = append(post[key], i) }

	for i, f := range funcs {
		for _, t := range f.Calls {
			add("c:"+t, i)
		}
		for _, t := range f.Lits {
			add("l:"+t, i)
		}
		for _, t := range f.Names {
			add("n:"+t, i)
		}
		add("s:"+f.Sig, i)
	}

	out := map[[2]int]struct{}{}
	keys := make([]string, 0, len(post))
	for k := range post {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ids := post[k]
		limit := dfCap
		if k[0] == 's' {
			limit = sigCap
		}
		if len(ids) < 2 || len(ids) > limit {
			continue
		}
		for x := range ids {
			for y := x + 1; y < len(ids); y++ {
				a, b := ids[x], ids[y]
				if a > b {
					a, b = b, a
				}
				out[[2]int{a, b}] = struct{}{}
			}
		}
	}
	return out
}
