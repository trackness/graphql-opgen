package genops

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// applyVariants resolves and validates the caller's selection-variant config
// against the schema and stores it on the set, so operation and fragment
// rendering can route an edge to a variant fragment (see fieldsFor). It is
// called once, after the canonical fragment universe is built and before
// operations are rendered. Every type, field, directive, and routing context is
// checked against the schema; an unresolved reference is an error, so drift
// upstream surfaces as a red build rather than a silent leak.
func (fs *FragmentSet) applyVariants(variants map[string]map[string]VariantExclude, edges map[string]string) error {
	for typeName, byVariant := range variants {
		def := fs.schema.Types[typeName]
		if def == nil || def.Kind != ast.Object {
			return fmt.Errorf("selection variant: type %q is not an object type in the schema", typeName)
		}
		for variant, spec := range byVariant {
			set, err := resolveExclude(def, variant, spec)
			if err != nil {
				return err
			}
			if fs.variantExcludes[typeName] == nil {
				fs.variantExcludes[typeName] = map[string]map[string]bool{}
			}
			fs.variantExcludes[typeName][variant] = set
		}
	}

	for ctx, route := range edges {
		typeName, variant, ok := strings.Cut(route, "/")
		if !ok || typeName == "" || variant == "" {
			return fmt.Errorf("variant edge %q: route %q must be \"Type/Variant\"", ctx, route)
		}
		if fs.variantExcludes[typeName] == nil || fs.variantExcludes[typeName][variant] == nil {
			return fmt.Errorf("variant edge %q -> %q: no such variant defined in SelectionVariants", ctx, route)
		}
		target, err := fs.edgeTargetType(ctx)
		if err != nil {
			return err
		}
		if target != typeName {
			return fmt.Errorf("variant edge %q targets type %s but route %q names %s", ctx, target, route, typeName)
		}
		fs.variantEdges[ctx] = route
	}
	return nil
}

// resolveExclude turns a VariantExclude into the concrete set of field names to
// omit, validating that every explicit field exists on def and that every named
// directive gates at least one field. A variant that excludes nothing — or
// everything — is an error: the former is a no-op that masks a typo, the latter
// leaves an empty selection.
func resolveExclude(def *ast.Definition, variant string, spec VariantExclude) (map[string]bool, error) {
	set := map[string]bool{}
	for _, name := range spec.Fields {
		if def.Fields.ForName(name) == nil {
			return nil, fmt.Errorf("selection variant %s/%s: field %q does not exist on type %s", def.Name, variant, name, def.Name)
		}
		set[name] = true
	}
	for _, dir := range spec.Directives {
		matched := false
		for _, f := range def.Fields {
			if f.Directives.ForName(dir) != nil {
				set[f.Name] = true
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("selection variant %s/%s: no field on type %s carries directive @%s", def.Name, variant, def.Name, dir)
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("selection variant %s/%s excludes no fields", def.Name, variant)
	}
	// Count the selectable fields the variant would keep; a variant that strips
	// the entire selectable surface yields an empty fragment, which is invalid.
	kept := 0
	for _, f := range def.Fields {
		if selectable(f) && !set[f.Name] {
			kept++
		}
	}
	if kept == 0 {
		return nil, fmt.Errorf("selection variant %s/%s excludes every selectable field, leaving an empty selection", def.Name, variant)
	}
	return set, nil
}

// edgeTargetType resolves the target object type of a routing context: a bare
// root field name (matched against Query/Mutation/Subscription) or a
// "ParentType.field" object edge. It returns the base type name of the field, or
// an error if the context names no such field or the field is not an object
// type (only object selections take a variant).
func (fs *FragmentSet) edgeTargetType(ctx string) (string, error) {
	parent, field, isEdge := strings.Cut(ctx, ".")
	var fd *ast.FieldDefinition
	if isEdge {
		pdef := fs.schema.Types[parent]
		if pdef == nil {
			return "", fmt.Errorf("variant edge %q: type %q does not exist in the schema", ctx, parent)
		}
		if fd = pdef.Fields.ForName(field); fd == nil {
			return "", fmt.Errorf("variant edge %q: type %s has no field %q", ctx, parent, field)
		}
	} else {
		for _, op := range []ast.Operation{ast.Query, ast.Mutation, ast.Subscription} {
			if f := RootFields(fs.schema, op).ForName(ctx); f != nil {
				fd = f
				break
			}
		}
		if fd == nil {
			return "", fmt.Errorf("variant edge %q: not a root field or \"Type.field\" object edge in the schema", ctx)
		}
	}
	target := fs.schema.Types[BaseTypeName(fd.Type)]
	if target == nil || target.Kind != ast.Object {
		return "", fmt.Errorf("variant edge %q: target is not an object type", ctx)
	}
	return target.Name, nil
}

// fieldsFor returns the fragment name to spread for an object selection at the
// given routing context (a root field name or "ParentType.field" edge): a
// routed variant <T><Variant>Fields if one is configured for the context, else
// the canonical <T>Fields. The variant was validated to target t at config time.
func (fs *FragmentSet) fieldsFor(ctx string, t *ast.Definition) string {
	if route, ok := fs.variantEdges[ctx]; ok {
		_, variant, _ := strings.Cut(route, "/")
		return fs.ensureVariantFields(t, variant)
	}
	return fs.ensureFields(t)
}

// ensureVariantFields materialises <T><Variant>Fields: the canonical <T>Fields
// selection minus the variant's excluded fields. It is built with the
// inline-render path cleared, exactly like ensureFields, so the body is
// independent of which operation first triggers it. Nested edges resolve through
// the normal path (their own canonical fragments, or a further routed variant).
func (fs *FragmentSet) ensureVariantFields(def *ast.Definition, variant string) string {
	name := def.Name + variant + "Fields"
	if _, ok := fs.bodies[name]; ok {
		return name
	}
	exclude := fs.variantExcludes[def.Name][variant]
	savedOnPath := fs.onPath
	fs.onPath = map[string]bool{}
	var b strings.Builder
	fmt.Fprintf(&b, "fragment %s on %s {\n", name, def.Name)
	fs.writeSelection(&b, def, "  ", false, exclude)
	b.WriteString("}\n")
	fs.onPath = savedOnPath
	fs.bodies[name] = b.String()
	return name
}
