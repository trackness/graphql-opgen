package genops

import (
	"maps"
	"slices"
	"strings"
)

// Path-based cycle termination
//
// genqlient fragments must form a directed acyclic graph: a fragment may not
// (transitively) spread itself. genops keeps the graph acyclic without a
// hand-maintained stop-list (B5):
//
//   - Nested entity edges resolve to a <T>Ref leaf, never the full <T>Fields
//     (B2), so every cycle through a ref-able type is broken at depth one.
//   - Value-type edges (e.g. address/region detail) are expanded until the walk
//     revisits a type already on the DFS path, at which point the edge is
//     terminated with a scalars-only inline selection (recorded as path-named).
//
// spreadGraph and FragmentCycles let the conformance suite assert the invariant
// directly against the emitted text.

// spreadGraph maps each fragment to the fragment names it spreads (via a
// `...Name` selection), parsed from the generated fragment bodies. Inline
// fragments (`... on Type`) are not spreads and are ignored.
func spreadGraph(fs *FragmentSet) map[string][]string {
	g := make(map[string][]string, len(fs.bodies))
	for _, name := range fs.Names() {
		body, _ := fs.Fragment(name)
		g[name] = parseSpreads(body)
	}
	return g
}

// parseSpreads returns the named fragment spreads (`...Name`) in some GraphQL
// text, deduplicated and sorted. Inline fragments (`... on Type`) are not named
// spreads and are ignored.
func parseSpreads(text string) []string {
	seen := map[string]bool{}
	var spreads []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "...") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "..."))
		if rest == "" || strings.HasPrefix(rest, "on ") {
			continue
		}
		frag := strings.Fields(rest)[0]
		if !seen[frag] {
			seen[frag] = true
			spreads = append(spreads, frag)
		}
	}
	slices.Sort(spreads)
	return spreads
}

// reachableFragments returns, sorted, the fragment names actually spread by the
// operations together with their transitive fragment dependencies — the subset
// of the pre-built universe that must be emitted (genqlient rejects unused
// fragments).
func reachableFragments(fs *FragmentSet, ops []Operation) []string {
	g := spreadGraph(fs)
	reached := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if reached[name] {
			return
		}
		if _, ok := fs.bodies[name]; !ok {
			return // a spread of a non-fragment (e.g. an inline body); ignore
		}
		reached[name] = true
		for _, dep := range g[name] {
			visit(dep)
		}
	}
	for _, op := range ops {
		for _, name := range parseSpreads(op.Text) {
			visit(name)
		}
	}
	return slices.Sorted(maps.Keys(reached))
}

// FragmentCycles returns each cycle in the fragment spread graph as the list of
// fragment names on the cycle. An empty result means the path-based termination
// produced a valid acyclic fragment DAG.
func FragmentCycles(fs *FragmentSet) [][]string {
	g := spreadGraph(fs)
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := map[string]int{}
	var stack []string
	var cycles [][]string

	names := slices.Sorted(maps.Keys(g))

	var visit func(string)
	visit = func(n string) {
		color[n] = gray
		stack = append(stack, n)
		for _, m := range g[n] {
			switch color[m] {
			case white:
				visit(m)
			case gray:
				// Back-edge: extract the cycle from the stack.
				for i, s := range stack {
					if s == m {
						cycle := slices.Clone(stack[i:])
						cycles = append(cycles, append(cycle, m))
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}

	for _, n := range names {
		if color[n] == white {
			visit(n)
		}
	}
	return cycles
}
