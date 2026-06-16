package genops

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"slices"
	"strings"
)

// Command is the CLI routing record for one operation: a cobra command path
// plus the hazard flags and type names the runtime needs to execute it. It is
// derived deterministically from a [ManifestEntry] by [BuildCommands].
type Command struct {
	// Path is the cobra command path, resource-then-verb, e.g.
	// [resource, "list"]. The final element is the leaf command name; the
	// preceding elements are group commands.
	Path []string
	// OpName is the exported operation name (e.g. FindThings), which is also the
	// generated query const name: <pkg>.<OpName><suffix>.
	OpName string
	// Field is the schema root-field name (e.g. findThings).
	Field string
	// Kind is "query", "mutation", or "subscription".
	Kind string
	// InputType is the base type of the "input" argument, or "" if none.
	InputType string
	// ReturnType is the base named type the field returns.
	ReturnType string
	// Destructive flags an operation the overlay marked as data-destroying.
	Destructive bool
	// JobReturning flags an operation that enqueues an async job.
	JobReturning bool
	// Deprecated flags a field carrying @deprecated in the schema.
	Deprecated bool
}

// entityGroup returns the resource group for a noun extracted from a field
// name. It consults the caller's curated irregular maps first, then falls back
// to the regular rules: a trailing "s" makes the noun plural (drop it,
// lower-case the stem), otherwise the noun is taken singular and lower-cased. A
// multi-word CamelCase noun is kebab-cased throughout.
func entityGroup(noun string, n NamingRules) string {
	if g, ok := n.PluralEntity[noun]; ok {
		return g
	}
	if g, ok := n.SingularEntity[noun]; ok {
		return g
	}
	// Regular plural: a trailing "s" on a noun longer than one rune (e.g.
	// Things, Orders -> thing, order).
	if strings.HasSuffix(noun, "s") && len(noun) > 1 {
		return kebab(noun[:len(noun)-1])
	}
	return kebab(noun)
}

// kebab converts a CamelCase or mixed identifier to kebab-case: each
// upper-case run starts a new segment, runs of upper-case letters (acronyms
// like HTTP, URL, SQL, API) stay together, and segments join with a hyphen.
// Examples: archiveOrder->archive-order (caller strips the prefix),
// PurgeCache->purge-cache, HTTPServer->http-server, parseURL->parse-url.
func kebab(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		isUpper := c >= 'A' && c <= 'Z'
		if isUpper && i > 0 {
			prev := r[i-1]
			prevUpper := prev >= 'A' && prev <= 'Z'
			next := rune(0)
			if i+1 < len(r) {
				next = r[i+1]
			}
			nextLower := next >= 'a' && next <= 'z'
			// Start a new segment at a lower->upper boundary (parseFoo) or at the
			// last letter of an acronym that begins a new word (HTTPServer: the
			// P before "erver" starts "server").
			if !prevUpper || nextLower {
				b.WriteByte('-')
			}
		}
		if isUpper {
			b.WriteRune(c - 'A' + 'a')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// derivePath returns the cobra path for one operation field. It is total and
// deterministic: every field resolves to a path by the first matching rule, and
// the fallback guarantees a path for anything the structured rules miss.
//
// The rules, in order:
//
//   - Subscriptions use the caller's pinned NamingRules.SubscriptionPaths.
//   - find<E>s / find<E> -> [entity, "list"] / [entity, "get"].
//   - all<E>s -> [entity, "all"].
//   - bulk<E>Update -> [entity, "bulk-update"].
//   - <E>sDestroy / <E>sUpdate (plural) -> [entity, "destroy-many" / "update-many"].
//   - <entity><Verb> for each NamingRules.VerbSuffixes verb -> [entity, verb].
//   - the caller's prefix groups (configure*, import*, export*, ...).
//   - Fallback: [NamingRules.FallbackGroup, kebab(field)] — a deterministic,
//     unique two-segment path that never collides with a structured group.
func derivePath(field, kind string, n NamingRules) []string {
	if kind == "subscription" {
		if p, ok := n.SubscriptionPaths[field]; ok {
			return p
		}
	}

	if p := deriveFind(field, n); p != nil {
		return p
	}
	if p := deriveAll(field, n); p != nil {
		return p
	}
	if p := deriveBulk(field, n); p != nil {
		return p
	}
	if p := derivePluralMutation(field, n); p != nil {
		return p
	}
	if p := deriveEntityVerb(field, n); p != nil {
		return p
	}
	if p := derivePrefixGroup(field, n); p != nil {
		return p
	}
	// Fallback: a stable two-segment path under a reserved fallback group. This
	// catches the long tail (stats, version, systemStatus, runReport, plugins,
	// directory, logs, setup, ...) without a hardcoded list, and cannot collide
	// with a structured path because the fallback group is never produced by any
	// rule above.
	return []string{n.FallbackGroup, kebab(field)}
}

// deriveFind handles the find<E>s / find<E> family. A plural target (the noun
// after "find" ends in a plural the entity vocabulary recognises) lists; a
// singular target gets. The caller's NamingRules.ExactFinds pins the handful of
// irregular finds whose routing the structural rule cannot infer.
func deriveFind(field string, n NamingRules) []string {
	if !strings.HasPrefix(field, "find") {
		return nil
	}
	rest := field[len("find"):]
	if p, ok := n.ExactFinds[field]; ok {
		return p
	}
	// Plural -> list, singular -> get, using the entity vocabulary to decide.
	if g, ok := n.PluralEntity[rest]; ok {
		return []string{g, "list"}
	}
	if strings.HasSuffix(rest, "s") && len(rest) > 1 {
		return []string{entityGroup(rest, n), "list"}
	}
	return []string{entityGroup(rest, n), "get"}
}

// deriveAll handles all<E>s -> [entity, "all"].
func deriveAll(field string, n NamingRules) []string {
	if !strings.HasPrefix(field, "all") || len(field) == len("all") {
		return nil
	}
	rest := field[len("all"):]
	if rest[0] < 'A' || rest[0] > 'Z' {
		return nil
	}
	return []string{entityGroup(rest, n), "all"}
}

// deriveBulk handles bulk<E>Update -> [entity, "bulk-update"].
func deriveBulk(field string, n NamingRules) []string {
	if !strings.HasPrefix(field, "bulk") || !strings.HasSuffix(field, "Update") {
		return nil
	}
	mid := field[len("bulk") : len(field)-len("Update")]
	if mid == "" {
		return nil
	}
	return []string{entityGroup(mid, n), "bulk-update"}
}

// derivePluralMutation handles plural batch mutations <E>sDestroy / <E>sUpdate /
// <E>sMerge (thingsDestroy, ordersUpdate, customersMerge) -> [entity, "<verb>-many"].
// The noun ends in "s" before the verb; singular <entity><Verb> is left to
// deriveEntityVerb. NamingRules.IrregularPluralNoun handles irregular plurals
// (e.g. an "-ies" plural mapping back to its "-y" singular) the trailing-"s"
// rule would mangle.
func derivePluralMutation(field string, n NamingRules) []string {
	for verb, leaf := range n.PluralMutationLeaf {
		if !strings.HasSuffix(field, verb) {
			continue
		}
		noun := field[:len(field)-len(verb)]
		if g, ok := n.IrregularPluralNoun[noun]; ok {
			return []string{g, leaf}
		}
		if strings.HasSuffix(noun, "s") && len(noun) > 1 && isLowerWord(noun) {
			return []string{kebab(noun[:len(noun)-1]), leaf}
		}
	}
	return nil
}

// deriveEntityVerb handles <entity><Verb> for the caller's entity mutation
// verbs. The entity is the camelCase prefix before the verb; it is lower-cased
// and kebab-cased (subThingCreate -> [sub-thing, create]).
func deriveEntityVerb(field string, n NamingRules) []string {
	for verb, leaf := range n.VerbSuffixes {
		if !strings.HasSuffix(field, verb) || len(field) == len(verb) {
			continue
		}
		ent := field[:len(field)-len(verb)]
		// The prefix must be a non-empty entity name; a field that merely ends in
		// the verb word but is not an entity mutation is excluded.
		if ent == "" {
			continue
		}
		return []string{kebab(ent), leaf}
	}
	return nil
}

// derivePrefixGroup handles the caller's prefix-keyed groups: first the exact
// field pins (NamingRules.ExactGroups), then the ordered prefix rules
// (NamingRules.PrefixGroups), the first match winning.
func derivePrefixGroup(field string, n NamingRules) []string {
	if p, ok := n.ExactGroups[field]; ok {
		return p
	}
	for _, r := range n.PrefixGroups {
		if r.Prefix == "" || !strings.HasPrefix(field, r.Prefix) {
			continue
		}
		if !r.MatchExact && len(field) == len(r.Prefix) {
			continue
		}
		return []string{r.Group, r.LeafPrefix + kebab(field[len(r.Prefix):])}
	}
	return nil
}

// isLowerWord reports whether s starts with a lower-case ASCII letter, marking
// a camelCase root-field prefix (as opposed to an exported type name).
func isLowerWord(s string) bool {
	return s != "" && s[0] >= 'a' && s[0] <= 'z'
}

// BuildCommands derives one [Command] per manifest entry, deterministically and
// with every path unique. It fails if two operations resolve to the same path,
// rather than silently overwriting one — a collision means the derivation rules
// need a disambiguator, and a red build is the right signal. The result is
// sorted by OpName for stable output.
func BuildCommands(m *Manifest, cfg Config) ([]Command, error) {
	cmds := make([]Command, 0, len(m.Operations))
	for _, e := range m.Operations {
		cmds = append(cmds, Command{
			Path:         derivePath(e.Field, e.Kind, cfg.Naming),
			OpName:       e.Name,
			Field:        e.Field,
			Kind:         e.Kind,
			InputType:    e.InputType,
			ReturnType:   e.ReturnType,
			Destructive:  e.Destructive,
			JobReturning: e.JobReturning,
			Deprecated:   e.Deprecated,
		})
	}
	slices.SortFunc(cmds, func(a, b Command) int { return cmp.Compare(a.OpName, b.OpName) })

	seen := make(map[string]string, len(cmds))
	for _, c := range cmds {
		key := strings.Join(c.Path, " ")
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf("genops: command path collision %q: %s and %s", key, prev, c.OpName)
		}
		seen[key] = c.OpName
	}
	return cmds, nil
}

// EmitCommands renders the generated table of commandSpec literals the CLI
// runtime assembles into its cobra tree. Each spec references the generated
// query const cfg.TargetPackageImport's <OpName><cfg.OperationConstSuffix>
// rather than re-embedding the query text, so the two generated surfaces cannot
// drift. The output is run through go/format, so it is gofmt-clean.
func EmitCommands(m *Manifest, cfg Config) ([]byte, error) {
	cmds, err := BuildCommands(m, cfg)
	if err != nil {
		return nil, err
	}

	pkg := importBaseName(cfg.TargetPackageImport)

	var b strings.Builder
	b.WriteString("// Code generated by genops; DO NOT EDIT.\n\n")
	b.WriteString("package main\n\n")
	fmt.Fprintf(&b, "import %q\n\n", cfg.TargetPackageImport)
	for _, line := range commandTableDoc(cfg) {
		fmt.Fprintf(&b, "// %s\n", line)
	}
	b.WriteString("var generatedCommands = []commandSpec{\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "\t{Path: %s, OpName: %q, Query: %s.%s%s, Kind: %q, InputType: %q, ReturnType: %q, Destructive: %t, JobReturning: %t, Deprecated: %t},\n",
			pathLiteral(c.Path), c.OpName, pkg, c.OpName, cfg.OperationConstSuffix, c.Kind, c.InputType, c.ReturnType, c.Destructive, c.JobReturning, c.Deprecated)
	}
	b.WriteString("}\n")

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("genops: formatting gen_commands.go: %w", err)
	}
	return src, nil
}

// commandTableDoc returns the doc-comment lines for the generated command
// table, falling back to a generic comment when the caller supplies none. Each
// line is emitted with a leading "// ".
func commandTableDoc(cfg Config) []string {
	if len(cfg.CommandTableDoc) > 0 {
		return cfg.CommandTableDoc
	}
	return []string{
		"generatedCommands is the full operation table, one spec per root",
		"field, sorted by OpName. buildRootCommand assembles these into the cobra",
		"tree. Query is the generated operation const, so the query text lives in",
		"exactly one place.",
	}
}

// importBaseName returns the last path element of an import path, the identifier
// the package is referenced by when imported without an alias.
func importBaseName(importPath string) string {
	if i := strings.LastIndex(importPath, "/"); i >= 0 {
		return importPath[i+1:]
	}
	return importPath
}

// pathLiteral renders a path slice as a Go composite literal:
// []string{"resource", "list"}.
func pathLiteral(path []string) string {
	var b bytes.Buffer
	b.WriteString("[]string{")
	for i, seg := range path {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", seg)
	}
	b.WriteString("}")
	return b.String()
}
