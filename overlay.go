package genops

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/vektah/gqlparser/v2/ast"
	"go.yaml.in/yaml/v3"
)

// Overlay is the curated operation metadata that the SDL alone cannot express:
// which operations mutate irreversible state (Destructive) and which return a
// background job id rather than their result inline (JobReturning). Both lists
// are keyed by GraphQL root-field name, e.g. purgeOrders — not by the
// generated operation name.
type Overlay struct {
	Destructive  []string `yaml:"destructive"`
	JobReturning []string `yaml:"jobReturning"`
}

// LoadOverlay reads and parses the curated overlay YAML at path. It does not
// validate the names against a schema — call [Overlay.Validate] (or build a
// manifest/catalog, which validates implicitly) once the schema is loaded.
func LoadOverlay(path string) (*Overlay, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read overlay %q: %w", path, err)
	}
	var ov Overlay
	// KnownFields(true) preserves yaml.v2's UnmarshalStrict behaviour: an unknown
	// key in the overlay is rejected rather than silently ignored, so a typo or a
	// stale field is a red build instead of a no-op.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&ov); err != nil {
		return nil, fmt.Errorf("parse overlay %q: %w", path, err)
	}
	return &ov, nil
}

// Validate reports an error if the overlay is malformed against the schema:
//
//   - a name in either list does not correspond to a real root field across
//     Query, Mutation, and Subscription (an upstream rename or removal);
//   - a name appears more than once within the same list (a redundant,
//     drift-prone entry that the duplicate-collapsing set would hide).
//
// A name may legitimately appear in BOTH lists — Destructive and JobReturning
// are independent classifications, and an operation can be both (e.g.
// reindexCatalog wipes the table and runs as a background job; the catalog
// sets both flags and accumulates both exit-code extensions). Cross-list
// membership is therefore not flagged.
//
// Each problem is a build error rather than a silent acceptance, keeping the
// overlay from drifting (the project's red-build philosophy). Problems are
// reported together, sorted, so a single run surfaces every issue.
func (ov *Overlay) Validate(s *ast.Schema) error {
	roots := rootFieldNames(s)
	var problems []string

	problems = append(problems, validateList(roots, "destructive", ov.Destructive)...)
	problems = append(problems, validateList(roots, "jobReturning", ov.JobReturning)...)

	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("overlay is invalid: %v", problems)
	}
	return nil
}

// validateList reports, for one overlay list, every name that is unknown to the
// schema and every name that appears more than once within the list. Duplicates
// are reported once each (not once per extra occurrence), in sorted order.
func validateList(roots map[string]bool, label string, names []string) []string {
	var problems []string
	count := map[string]int{}
	for _, name := range names {
		if !roots[name] {
			problems = append(problems, label+": unknown "+name)
		}
		count[name]++
	}
	for _, name := range slices.Sorted(maps.Keys(count)) {
		if count[name] > 1 {
			problems = append(problems, label+": duplicate "+name)
		}
	}
	return problems
}

// rootFieldNames returns the set of every root-field name across Query,
// Mutation, and Subscription (introspection excluded, via RootFields).
func rootFieldNames(s *ast.Schema) map[string]bool {
	out := map[string]bool{}
	for _, op := range []ast.Operation{ast.Query, ast.Mutation, ast.Subscription} {
		for _, f := range RootFields(s, op) {
			out[f.Name] = true
		}
	}
	return out
}

// destructiveSet and jobReturningSet expose the overlay lists as sets keyed by
// root-field name, for O(1) lookup while building the manifest and catalog.
func (ov *Overlay) destructiveSet() map[string]bool  { return toSet(ov.Destructive) }
func (ov *Overlay) jobReturningSet() map[string]bool { return toSet(ov.JobReturning) }

func toSet(xs []string) map[string]bool {
	out := make(map[string]bool, len(xs))
	for _, x := range xs {
		out[x] = true
	}
	return out
}
