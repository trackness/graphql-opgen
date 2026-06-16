package genops

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// flattenDirective collapses a single-fragment-spread field so genqlient binds
// the field's Go type directly to the spread fragment's canonical type rather
// than emitting a path-named wrapper struct (spike finding A1). It is emitted on
// the line *above* the field — never above an operation, where it would bind to
// the first variable definition and error.
const flattenDirective = "# @genqlient(flatten: true)"

// displayField returns the non-deprecated display field ("name" or "title") of
// def, or "" if it has neither — i.e. def cannot be represented by a Ref.
func displayField(def *ast.Definition) string {
	for _, name := range [...]string{"name", "title"} {
		if f := def.Fields.ForName(name); f != nil && !IsDeprecated(f) {
			return name
		}
	}
	return ""
}

// hasID reports whether def has an `id: ID!` field.
func hasID(def *ast.Definition) bool {
	f := def.Fields.ForName("id")
	return f != nil && BaseTypeName(f.Type) == "ID"
}

// IsRefable reports whether an object type can be represented by a Ref fragment.
// Per B1 a ref is always {id, name|title}; an id-only "ref" is useless, so a
// display field is mandatory.
func IsRefable(def *ast.Definition) bool {
	return def != nil && def.Kind == ast.Object && hasID(def) && displayField(def) != ""
}

// hasRequiredArgs reports whether a field takes any argument that is non-null
// with no default — such a field cannot be selected without supplying a value,
// so it is omitted from generated entity selections (its data, where it
// matters, is reachable through a sibling field; e.g. Order.lineItems
// covers the parameterized Order.lineItem accessor).
func hasRequiredArgs(f *ast.FieldDefinition) bool {
	for _, a := range f.Arguments {
		if a.Type.NonNull && a.DefaultValue == nil {
			return true
		}
	}
	return false
}

// selectable reports whether a field may appear in a generated entity selection.
func selectable(f *ast.FieldDefinition) bool {
	return !IsDeprecated(f) && !isIntrospection(f.Name) && !hasRequiredArgs(f)
}

// hasObjectEdges reports whether def has at least one selectable field whose
// underlying type is an object, interface, or union.
func hasObjectEdges(s *ast.Schema, def *ast.Definition) bool {
	for _, f := range def.Fields {
		if !selectable(f) {
			continue
		}
		if t := s.Types[BaseTypeName(f.Type)]; t != nil {
			switch t.Kind {
			case ast.Object, ast.Interface, ast.Union:
				return true
			}
		}
	}
	return false
}

// isMixedWrapper reports whether def is a junction object that must be inlined
// as a path-named selection (A3): it carries object edges but has no id, so it
// is neither a referenceable entity nor a flat value type. The caller's
// PathNamedAllowlist enumerates the concrete SDL instances.
func isMixedWrapper(s *ast.Schema, def *ast.Definition) bool {
	return def.Kind == ast.Object && !hasID(def) && hasObjectEdges(s, def)
}

// isRootOperation reports whether def is the Query, Mutation, or Subscription
// root type — these are entry points, not selectable entities.
func isRootOperation(s *ast.Schema, def *ast.Definition) bool {
	return (s.Query != nil && def == s.Query) ||
		(s.Mutation != nil && def == s.Mutation) ||
		(s.Subscription != nil && def == s.Subscription)
}

// RefName and FieldsName are the canonical genqlient type names for a type.
func RefName(t string) string    { return t + "Ref" }
func FieldsName(t string) string { return t + "Fields" }

// FragmentSet is the deterministic, acyclic set of genqlient fragments
// generated from a schema: a <T>Ref leaf for every ref-able entity and a
// <T>Fields fragment for every fully-expanded object type. Nested entity edges
// resolve to Ref (B2: cyclic edges stay ref-only); the operation layer spreads
// the full <E>Fields at an operation's payload root.
type FragmentSet struct {
	schema *ast.Schema
	bodies map[string]string // fragment name -> "fragment ... { ... }\n"
	// building tracks the fragment-construction DFS path (set only by
	// ensureFields) so value-type cycles like Address<->Region terminate. It
	// persists across nested ensureFields calls.
	building map[string]bool
	// onPath tracks the operation-render and mixed-wrapper-inline path. It is
	// scoped to inline rendering and deliberately cleared while a canonical
	// fragment is built (see ensureFields), so a lazily-built <T>Fields body is
	// context-free: which operation first triggers its construction cannot change
	// its shape.
	onPath map[string]bool
	path   *pathNamedRecorder
	// valueEdges is the static value-type fragment-spread graph: valueEdges[A]
	// holds every value type B that A would spread as ...BFields if not for cycle
	// termination (object edge to a non-ref-able, non-mixed-wrapper object). It
	// drives valueCycleEdge, which decides cycle termination from this fixed graph
	// rather than the construction DFS stack — so an Address<->Region edge
	// terminates the same way no matter which fragment is built first (genops-4).
	valueEdges map[string]map[string]bool
}

type pathNamedRecorder struct {
	types map[string]bool
}

// newFragmentSet returns an empty fragment set over a schema. Fragments are
// materialised on demand by ensureRef / ensureFields.
func newFragmentSet(s *ast.Schema) *FragmentSet {
	return &FragmentSet{
		schema:     s,
		bodies:     map[string]string{},
		building:   map[string]bool{},
		onPath:     map[string]bool{},
		path:       &pathNamedRecorder{types: map[string]bool{}},
		valueEdges: buildValueEdges(s),
	}
}

// buildValueEdges computes the static value-type fragment-spread graph: an edge
// A -> B exists when object type A has a selectable object edge whose target B
// is a value type (an object that is neither ref-able nor a mixed wrapper), i.e.
// an edge writeObjectEdge would render as a flattened ...BFields spread. Ref-able
// targets (terminated at a Ref, B2), mixed wrappers (inlined), and abstract types
// are not fragment spreads and are excluded — they cannot form a value-type
// fragment cycle. The graph is order-free, so the termination decision built on
// it (valueCycleEdge) is independent of fragment construction order.
func buildValueEdges(s *ast.Schema) map[string]map[string]bool {
	g := make(map[string]map[string]bool)
	for name, def := range s.Types {
		if def.Kind != ast.Object {
			continue
		}
		for _, f := range def.Fields {
			if !selectable(f) {
				continue
			}
			t := s.Types[BaseTypeName(f.Type)]
			if t == nil || t.Kind != ast.Object || IsRefable(t) || isMixedWrapper(s, t) {
				continue
			}
			if g[name] == nil {
				g[name] = make(map[string]bool)
			}
			g[name][t.Name] = true
		}
	}
	return g
}

// valueCycleEdge reports whether expanding the value-type edge src -> t would
// (transitively, through value-type fragment spreads) lead back to src, which
// would close a fragment cycle. It is computed purely from the static valueEdges
// graph, so the answer for any (src, t) pair is fixed regardless of which
// fragment is built first: the Address<->Region mutual edges all report true
// and terminate scalars-only, while a one-way edge into the address/region graph
// (Shipment.origin -> Address, Region.timezone -> Timezone)
// reports false and expands fully (genops-4).
func (fs *FragmentSet) valueCycleEdge(src string, t *ast.Definition) bool {
	if t.Name == src {
		return true // direct self-reference
	}
	// Does t reach src through value-type spreads?
	seen := map[string]bool{}
	var reaches func(n string) bool
	reaches = func(n string) bool {
		for m := range fs.valueEdges[n] {
			if m == src {
				return true
			}
			if !seen[m] {
				seen[m] = true
				if reaches(m) {
					return true
				}
			}
		}
		return false
	}
	return reaches(t.Name)
}

// BuildFragments walks the schema and produces the full fragment set for every
// reachable object type. It is deterministic: the same schema yields
// byte-identical fragment text. Generation uses [Compile], which builds only
// operation-reachable fragments; BuildFragments materialises the whole universe
// for conformance.
func BuildFragments(s *ast.Schema) *FragmentSet {
	fs := newFragmentSet(s)
	// Materialise a fragment for every ref-able entity and every expandable
	// object type reachable from the schema, in sorted type order so cycle
	// termination is independent of any operation's walk order.
	for _, name := range slices.Sorted(maps.Keys(s.Types)) {
		def := s.Types[name]
		if def.Kind != ast.Object || strings.HasPrefix(name, "__") || isRootOperation(s, def) {
			continue
		}
		if IsRefable(def) {
			fs.ensureRef(def)
		}
		if !isMixedWrapper(s, def) {
			fs.ensureFields(def)
		}
	}
	return fs
}

// Fragment returns the text of a named fragment, if present.
func (fs *FragmentSet) Fragment(name string) (string, bool) {
	body, ok := fs.bodies[name]
	return body, ok
}

// Names returns the fragment names in sorted (deterministic) order.
func (fs *FragmentSet) Names() []string {
	return slices.Sorted(maps.Keys(fs.bodies))
}

// PathNamedTypes returns the object types that were emitted as path-named
// inline selections (mixed wrappers, unions, interfaces, and terminated value
// cycles) rather than canonical fragments — the A3 exception set.
func (fs *FragmentSet) PathNamedTypes() []string {
	return slices.Sorted(maps.Keys(fs.path.types))
}

func (fs *FragmentSet) ensureRef(def *ast.Definition) string {
	name := RefName(def.Name)
	if _, ok := fs.bodies[name]; !ok {
		fs.bodies[name] = fmt.Sprintf("fragment %s on %s {\n  id\n  %s\n}\n", name, def.Name, displayField(def))
	}
	return name
}

func (fs *FragmentSet) ensureFields(def *ast.Definition) string {
	name := FieldsName(def.Name)
	if _, ok := fs.bodies[name]; ok {
		return name
	}
	if fs.building[def.Name] {
		return name // cycle; caller terminated the edge
	}
	fs.building[def.Name] = true
	// A canonical fragment must be byte-identical regardless of which operation
	// first triggers its construction, so build it with the inline-render path
	// cleared. Without this, a root field's result-wrapper flag (or a mixed
	// wrapper being inlined above) leaks in and truncates a valid edge — e.g.
	// OrderFields.lines.order is dropped when orderLines roots before orders.
	savedOnPath := fs.onPath
	fs.onPath = map[string]bool{}
	var b strings.Builder
	fmt.Fprintf(&b, "fragment %s on %s {\n", name, def.Name)
	fs.writeSelection(&b, def, "  ", false)
	b.WriteString("}\n")
	fs.onPath = savedOnPath
	delete(fs.building, def.Name)
	fs.bodies[name] = b.String()
	return name
}

// writeSelection renders def's non-deprecated fields at the given indent. When
// full is true (an operation's payload root), ref-able edges expand to the full
// <T>Fields; otherwise (inside a fragment) they terminate at <T>Ref (B2). src is
// the name of the type whose selection is being written — the cycle origin for
// the order-independent value-type termination in writeObjectEdge.
func (fs *FragmentSet) writeSelection(b *strings.Builder, def *ast.Definition, indent string, full bool) {
	for _, f := range def.Fields {
		if !selectable(f) {
			continue
		}
		t := fs.schema.Types[BaseTypeName(f.Type)]
		if t == nil {
			continue
		}
		switch t.Kind {
		case ast.Scalar, ast.Enum:
			fmt.Fprintf(b, "%s%s\n", indent, f.Name)
		case ast.Object:
			fs.writeObjectEdge(b, def.Name, f.Name, f, t, indent, full)
		case ast.Union, ast.Interface:
			fs.writeAbstractEdge(b, f, t, indent)
		}
	}
}

// writeObjectEdge renders an object-typed field on type src: a flattened Ref (or
// full Fields at a payload root) spread for a ref-able entity, a flattened Fields
// spread for a value type, or an inline path-named selection for a mixed wrapper
// / terminated cycle. label is the response token printed before the selection
// block — the bare field name, or a "Prefix_name: name" alias when a conflicting
// union member's edges must each carry a distinct response name.
func (fs *FragmentSet) writeObjectEdge(b *strings.Builder, src, label string, f *ast.FieldDefinition, t *ast.Definition, indent string, full bool) {
	switch {
	case IsRefable(t):
		// A Ref is a leaf and a Fields fragment is memoised + on-path guarded, so
		// both are cycle-safe.
		if full {
			writeSpread(b, label, fs.ensureFields(t), indent)
		} else {
			writeSpread(b, label, fs.ensureRef(t), indent)
		}
	case fs.valueCycleEdge(src, t):
		// A value-type edge that closes a fragment cycle (Address<->Region,
		// Region->Region). The decision is taken from the static value-type graph
		// (valueCycleEdge), not the construction stack, so BOTH directions of a
		// mutual cycle terminate identically no matter which fragment is built
		// first — the genops-4 order dependence is gone. Terminate scalars-only so
		// the fragment DAG stays finite and acyclic; record as path-named (B6).
		fs.path.types[t.Name] = true
		writeInline(b, label, indent, func(inner string) {
			fs.writeScalarsOnly(b, t, inner)
		})
	case fs.onPath[t.Name]:
		// A self-referential inline render revisited a type already on the
		// inline-render path: a result-wrapper container (e.g. SearchResult) or a
		// recursive mixed wrapper (e.g. CategoryTree.parent). Terminate
		// scalars-only so the inline selection stays finite.
		fs.path.types[t.Name] = true
		writeInline(b, label, indent, func(inner string) {
			fs.writeScalarsOnly(b, t, inner)
		})
	case isMixedWrapper(fs.schema, t):
		// Junction wrapper: inline (path-named), tracking it on the inline-render
		// path so a recursive wrapper terminates above. onPath (not building) so
		// this does not leak into a canonical fragment built underneath.
		fs.path.types[t.Name] = true
		fs.onPath[t.Name] = true
		writeInline(b, label, indent, func(inner string) {
			fs.writeSelection(b, t, inner, false)
		})
		delete(fs.onPath, t.Name)
	default:
		writeSpread(b, label, fs.ensureFields(t), indent)
	}
}

// writeAbstractEdge renders a union/interface field as an inline selection with
// __typename (A5: required for the generated UnmarshalJSON) plus one inline
// fragment per concrete member, each spreading the member's Fields fragment.
func (fs *FragmentSet) writeAbstractEdge(b *strings.Builder, f *ast.FieldDefinition, t *ast.Definition, indent string) {
	fmt.Fprintf(b, "%s%s {\n", indent, f.Name)
	fs.writeAbstractBody(b, t, indent+"  ")
	fmt.Fprintf(b, "%s}\n", indent)
}

// writeAbstractBody renders the inside of a union/interface selection at indent:
// __typename (mandatory — the generated UnmarshalJSON keys on it, A5) followed
// by one inline fragment per concrete member.
//
// When the members agree on the type of every shared field, each member spreads
// its canonical Fields fragment. When they disagree (a shared field is Int on
// one member but String on another), spreading would violate GraphQL's
// SameResponseShape rule, so each member's whole selection is rendered under
// member-prefixed response aliases instead — both its scalar leaves AND its
// object edges (genops-6: the object edges were previously dropped entirely,
// leaving the union scalars-only). The prefix makes every member's response
// names unique, so two members can select the same underlying field name with
// different shapes without colliding.
func (fs *FragmentSet) writeAbstractBody(b *strings.Builder, t *ast.Definition, indent string) {
	fs.path.types[t.Name] = true
	fmt.Fprintf(b, "%s__typename\n", indent)
	members := fs.abstractMembers(t)
	conflict := abstractHasConflict(fs.schema, members)
	for _, m := range members {
		mdef := fs.schema.Types[m]
		fmt.Fprintf(b, "%s... on %s {\n", indent, m)
		if conflict {
			fs.writeAliasedMember(b, mdef, m, indent+"  ")
		} else {
			writeSpreadBody(b, fs.ensureFields(mdef), indent+"  ")
		}
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

// abstractHasConflict reports whether two members of an abstract type disagree
// on the type of a shared, selectable field — in which case spreading their
// Fields fragments into one selection set is invalid GraphQL.
func abstractHasConflict(s *ast.Schema, members []string) bool {
	seen := map[string]string{}
	for _, m := range members {
		def := s.Types[m]
		if def == nil {
			continue
		}
		for _, f := range def.Fields {
			if !selectable(f) {
				continue
			}
			if prev, ok := seen[f.Name]; ok && prev != f.Type.String() {
				return true
			}
			seen[f.Name] = f.Type.String()
		}
	}
	return false
}

// writeAliasedMember renders a conflicting union member's full selection with
// every response name prefixed by the member type, so no two members collide on
// a response name (the SameResponseShape rule). Scalar/enum leaves become
// "Prefix_name: name"; object edges become "Prefix_name: name { ... }" with the
// inner selection rendered by writeObjectEdge (a Ref for a ref-able target, a
// flattened Fields spread for a value type, an inline path-named selection for a
// mixed wrapper, scalars-only at a cycle); abstract edges recurse with __typename
// and per-member inline fragments. Only the member's top-level response name
// needs the prefix — nested selection sets are distinct, so their fields are
// rendered unaliased. The prefix is the cycle origin (src) for value-type
// termination; member types are mixed wrappers, not value-cycle nodes, so this
// is conservative.
func (fs *FragmentSet) writeAliasedMember(b *strings.Builder, def *ast.Definition, prefix, indent string) {
	for _, f := range def.Fields {
		if !selectable(f) {
			continue
		}
		t := fs.schema.Types[BaseTypeName(f.Type)]
		if t == nil {
			continue
		}
		label := fmt.Sprintf("%s_%s: %s", prefix, f.Name, f.Name)
		switch t.Kind {
		case ast.Scalar, ast.Enum:
			fmt.Fprintf(b, "%s%s\n", indent, label)
		case ast.Object:
			fs.writeObjectEdge(b, prefix, label, f, t, indent, false)
		case ast.Union, ast.Interface:
			fmt.Fprintf(b, "%s%s {\n", indent, label)
			fs.writeAbstractBody(b, t, indent+"  ")
			fmt.Fprintf(b, "%s}\n", indent)
		}
	}
}

// abstractMembers returns the concrete object types of a union (its Types) or
// the implementations of an interface, in sorted order.
func (fs *FragmentSet) abstractMembers(t *ast.Definition) []string {
	var members []string
	if t.Kind == ast.Union {
		members = append(members, t.Types...)
	} else { // interface
		for _, def := range fs.schema.Types {
			if def.Kind == ast.Object {
				for _, iface := range def.Interfaces {
					if iface == t.Name {
						members = append(members, def.Name)
					}
				}
			}
		}
	}
	slices.Sort(members)
	return members
}

// writeScalarsOnly renders only the scalar/enum leaves of def (cycle terminator).
func (fs *FragmentSet) writeScalarsOnly(b *strings.Builder, def *ast.Definition, indent string) {
	for _, f := range def.Fields {
		if !selectable(f) {
			continue
		}
		if t := fs.schema.Types[BaseTypeName(f.Type)]; t != nil {
			switch t.Kind {
			case ast.Scalar, ast.Enum:
				fmt.Fprintf(b, "%s%s\n", indent, f.Name)
			}
		}
	}
}

// writeSpread renders a flattened single-fragment-spread field:
//
//	# @genqlient(flatten: true)
//	<field> {
//	  ...<frag>
//	}
func writeSpread(b *strings.Builder, field, frag, indent string) {
	fmt.Fprintf(b, "%s%s\n", indent, flattenDirective)
	fmt.Fprintf(b, "%s%s {\n", indent, field)
	writeSpreadBody(b, frag, indent+"  ")
	fmt.Fprintf(b, "%s}\n", indent)
}

func writeSpreadBody(b *strings.Builder, frag, indent string) {
	fmt.Fprintf(b, "%s...%s\n", indent, frag)
}

// writeInline renders an inline (non-flattened, path-named) object selection.
func writeInline(b *strings.Builder, field, indent string, body func(inner string)) {
	fmt.Fprintf(b, "%s%s {\n", indent, field)
	body(indent + "  ")
	fmt.Fprintf(b, "%s}\n", indent)
}
