package genops

import (
	"cmp"
	"fmt"
	"go/token"
	"slices"
	"strings"
	"unicode"

	"github.com/vektah/gqlparser/v2/ast"
)

// Operation is one generated genqlient operation: a single root field wrapped
// in a named query, mutation, or subscription with its variables forwarded.
type Operation struct {
	Name  string        // exported operation name (FindThings, ThingCreate, ...)
	Op    ast.Operation // query | mutation | subscription
	Field string        // root field name (findThings, thingCreate, ...)
	Text  string        // full operation source, terminated by a newline
}

// BuildOperations assembles one operation per root field across Query,
// Mutation, and Subscription. Output is deterministic — sorted by operation
// type then field name — and names are collision-free. Variables are typed
// verbatim from the SDL with deprecated arguments dropped (A6, e.g. a field
// keeps its current ids:[ID!] argument and omits a deprecated one); interface/
// union selections carry __typename (A5).
func BuildOperations(s *ast.Schema, fs *FragmentSet) ([]Operation, error) {
	var ops []Operation
	seen := map[string]string{}
	for _, ot := range []ast.Operation{ast.Query, ast.Mutation, ast.Subscription} {
		sorted := slices.Clone(RootFields(s, ot))
		slices.SortFunc(sorted, func(a, b *ast.FieldDefinition) int { return cmp.Compare(a.Name, b.Name) })
		for _, f := range sorted {
			name := exportName(f.Name)
			if prev, ok := seen[name]; ok {
				return nil, fmt.Errorf("operation name collision %q: %s vs %s %s", name, prev, ot, f.Name)
			}
			seen[name] = string(ot) + " " + f.Name
			ops = append(ops, Operation{
				Name:  name,
				Op:    ot,
				Field: f.Name,
				Text:  fs.renderOperation(ot, name, f),
			})
		}
	}
	return ops, nil
}

// exportName upper-cases the first rune so the operation becomes an exported Go
// identifier. Root field names are camelCase with no separators.
func exportName(field string) string {
	r := []rune(field)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// safeVarName returns a GraphQL variable name for an argument that is a valid Go
// identifier: genqlient turns each variable into a function parameter, so an
// argument named after a Go keyword (e.g. type) is suffixed with an underscore.
// The forwarded field argument keeps its real name.
func safeVarName(arg string) string {
	if token.IsKeyword(arg) {
		return arg + "_"
	}
	return arg
}

// nonDeprecatedArgs returns a field's arguments with @deprecated ones removed.
func nonDeprecatedArgs(f *ast.FieldDefinition) ast.ArgumentDefinitionList {
	var out ast.ArgumentDefinitionList
	for _, a := range f.Arguments {
		if a.Directives.ForName("deprecated") == nil {
			out = append(out, a)
		}
	}
	return out
}

func (fs *FragmentSet) renderOperation(ot ast.Operation, name string, f *ast.FieldDefinition) string {
	args := nonDeprecatedArgs(f)
	var b strings.Builder

	fmt.Fprintf(&b, "%s %s", ot, name)
	if len(args) > 0 {
		b.WriteString("(")
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$%s: %s", safeVarName(a.Name), a.Type.String())
			if a.DefaultValue != nil {
				fmt.Fprintf(&b, " = %s", a.DefaultValue.String())
			}
		}
		b.WriteString(")")
	}
	b.WriteString(" {\n")
	fs.renderRootField(&b, f, args, "  ")
	b.WriteString("}\n")
	return b.String()
}

// renderRootField renders the operation's single root field: its forwarded
// arguments plus a return-type selection (none for scalars; the full <T>Fields
// for an entity; an inline __typename selection for an interface/union; an
// inlined full selection for a result-wrapper container).
func (fs *FragmentSet) renderRootField(b *strings.Builder, f *ast.FieldDefinition, args ast.ArgumentDefinitionList, indent string) {
	call := f.Name
	if len(args) > 0 {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = fmt.Sprintf("%s: $%s", a.Name, safeVarName(a.Name))
		}
		call = fmt.Sprintf("%s(%s)", f.Name, strings.Join(parts, ", "))
	}

	def := fs.schema.Types[BaseTypeName(f.Type)]
	switch {
	case def == nil || def.Kind == ast.Scalar || def.Kind == ast.Enum:
		fmt.Fprintf(b, "%s%s\n", indent, call)
	case def.Kind == ast.Union || def.Kind == ast.Interface:
		fmt.Fprintf(b, "%s%s {\n", indent, call)
		fs.writeAbstractBody(b, def, indent+"  ")
		fmt.Fprintf(b, "%s}\n", indent)
	case IsRefable(def):
		// Entity payload: flatten the single fragment spread so the response
		// field binds directly to the <T>Fields type. fieldsFor materialises the
		// fragment — an entity returned only directly (e.g. Receipt) is otherwise
		// referenced but never emitted — and routes the root field to a selection
		// variant if the caller configured one for it (e.g. findUser -> Public).
		frag := fs.fieldsFor(f.Name, def)
		fmt.Fprintf(b, "%s%s\n", indent, flattenDirective)
		fmt.Fprintf(b, "%s%s {\n", indent, call)
		fmt.Fprintf(b, "%s...%s\n", indent+"  ", frag)
		fmt.Fprintf(b, "%s}\n", indent)
	default:
		// Result-wrapper container: expand inline with entity edges full (the
		// query's whole point), nested entities still terminating at Refs. Track
		// the container on the inline-render path (onPath, not building) so a
		// self-referential field terminates without leaking into the canonical
		// fragments built underneath.
		fs.onPath[def.Name] = true
		fmt.Fprintf(b, "%s%s {\n", indent, call)
		fs.writeSelection(b, def, indent+"  ", true, nil)
		fmt.Fprintf(b, "%s}\n", indent)
		delete(fs.onPath, def.Name)
	}
}
