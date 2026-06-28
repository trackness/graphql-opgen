package genops

// Config carries everything schema-specific that the generator needs but cannot
// derive from the SDL alone: the exit-code vocabulary, the service's naming
// irregulars, the audited path-named exception set, and the two strings that
// stamp the emitted command table (the target package import and the operation
// const suffix). The core compiler is schema-agnostic; the caller supplies a
// Config describing its particular GraphQL service.
type Config struct {
	// ExitCodes is the caller's exit-code taxonomy. The catalog projects these
	// names into each command's exitCodes array.
	ExitCodes ExitCodeProvider
	// Naming holds the irregular plurals, multi-word singulars, pinned
	// subscription paths, and prefix/suffix vocabulary the CLI-path derivation
	// cannot infer from the regular structural rules.
	Naming NamingRules
	// PathNamedAllowlist is the audited set of object/union/interface type names
	// the generator may emit as path-named inline selections, each mapped to the
	// human-readable reason it is exempt from the "no path-named struct"
	// invariant.
	PathNamedAllowlist map[string]string
	// TargetPackageImport is the import path of the package that holds the
	// generated genqlient operation consts (e.g. the "<Op>_Operation" symbols the
	// emitted command table references).
	TargetPackageImport string
	// OperationConstSuffix is appended to an operation name to form its generated
	// query-const symbol (e.g. "_Operation" yields FindThings_Operation).
	OperationConstSuffix string
	// CommandTableDoc is the doc-comment block emitted above the generated
	// command table (each line WITHOUT the leading "// "). It is part of the
	// generated output, so the caller owns its service-specific wording; an empty
	// value falls back to a generic comment.
	CommandTableDoc []string
	// SelectionVariants defines named alternate selections for object types. A
	// variant is the type's canonical <T>Fields selection minus a set of fields,
	// materialised as a distinct fragment <T><Variant>Fields and emitted only
	// where an edge routes to it (see VariantEdges); the canonical <T>Fields is
	// left untouched. A variant lets a field that resolves only in some contexts
	// — an auth-gated field a server returns for a self lookup but not a third
	// party, or a field a server leaves null on one edge but not another — be
	// omitted exactly where it would break, without losing it where it works.
	//
	// The map is keyed by object type name, then by variant name. Every type,
	// field, and directive is validated against the schema at [Compile] time, so
	// a rename or removal upstream is a red build rather than a silent leak.
	SelectionVariants map[string]map[string]VariantExclude
	// VariantEdges routes a selection context to a named variant of the edge's
	// target type instead of the canonical <T>Fields. A key is either a root
	// field name (e.g. "findUser" — an operation's payload root) or a
	// "ParentType.field" object edge (e.g. "Edit.user", "SceneEdit.fingerprints").
	// The value names the variant as "Type/Variant" (e.g. "User/Public"); Type
	// must equal the edge's target type and Variant must be defined for it in
	// SelectionVariants. Every key and value is validated at [Compile] time.
	VariantEdges map[string]string
}

// VariantExclude specifies which of an object type's fields a selection variant
// omits, by explicit name and/or by directive. Naming a directive derives the
// excluded set from the SDL — a field gated by that directive (e.g. an
// owner-only @isUserOwner) is dropped from the variant, and a newly gated field
// upstream is excluded automatically rather than silently leaking into a
// selection that cannot resolve it. At least one of the two must match a field.
type VariantExclude struct {
	// Fields are explicit field names to omit from the variant.
	Fields []string
	// Directives names schema directives; any field on the type carrying one of
	// them is omitted from the variant.
	Directives []string
}

// ExitCodeProvider names the caller's exit codes. Base is the set every command
// can return, in frozen order; the remaining fields name the codes the catalog
// appends per command shape — NotFound for a nullable single-entity lookup,
// DestructiveRefused for a destructive op, and the JobFailed/StillRunning/
// Unconfirmed trio for a job-returning op.
type ExitCodeProvider struct {
	Base               []string
	NotFound           string
	DestructiveRefused string
	JobFailed          string
	StillRunning       string
	Unconfirmed        string
}

// NamingRules holds the service-specific naming data the CLI-path derivation
// needs. The structural rules (find/all/bulk/<entity><verb>, plural-mutation,
// prefix groups) live in the core; this struct supplies the irregular data those
// rules consult.
type NamingRules struct {
	// PluralEntity maps an irregular or multi-word plural noun (as it appears in
	// a root field name) to its singular kebab-case resource group. Regular
	// plurals (a trailing "s") are handled structurally and need no entry.
	PluralEntity map[string]string
	// SingularEntity maps a multi-word singular noun to its kebab-case resource
	// group. Single-word singulars fall through to a lower-cased default.
	SingularEntity map[string]string
	// SubscriptionPaths pins each subscription field to its CLI path.
	SubscriptionPaths map[string][]string
	// VerbSuffixes are the entity-mutation verbs recognised on a <entity><Verb>
	// field, each mapped to its kebab-case leaf command name.
	VerbSuffixes map[string]string
	// PluralMutationLeaf names the plural batch-mutation verbs and their leaf
	// command names (a "-many" suffix sets them apart from singular mutations).
	PluralMutationLeaf map[string]string
	// ExactFinds pins individual find<X> fields whose routing the structural
	// plural/singular rule cannot infer, mapping each field name to its CLI path.
	ExactFinds map[string][]string
	// IrregularPluralNoun maps an irregular lower-case plural noun (as it appears
	// before a plural-mutation verb) to its singular kebab-case group, for the
	// fields the trailing-"s" rule would mangle.
	IrregularPluralNoun map[string]string

	// ExactGroups pins individual fields whose routing no structural rule
	// produces, mapping each field name to its full CLI path. It is consulted in
	// the prefix-group stage, before PrefixGroups.
	ExactGroups map[string][]string
	// PrefixGroups is the ordered set of prefix-keyed routing rules (the
	// long-tail groups a service collects under a single resource, e.g.
	// configure*, import*, export*). Order matters: the first matching rule
	// wins, so a more specific prefix must precede a prefix it extends.
	PrefixGroups []PrefixGroupRule

	// FallbackGroup is the reserved resource group for fields no structural rule
	// claims (e.g. "misc"); it must never be produced by any other rule.
	FallbackGroup string
}

// PrefixGroupRule routes every field carrying Prefix under a single resource
// Group: the field's remainder (after Prefix) is kebab-cased and, with LeafPrefix
// prepended, becomes the leaf command, yielding [Group, LeafPrefix+kebab(rest)].
type PrefixGroupRule struct {
	// Prefix is the camelCase field prefix this rule claims.
	Prefix string
	// Group is the kebab-case resource group matched fields route under.
	Group string
	// LeafPrefix is prepended to the kebab-cased remainder to form the leaf
	// command (e.g. "submit-" for a submission family); usually empty.
	LeafPrefix string
	// MatchExact, when true, lets the rule match a field equal to Prefix (the
	// remainder is then empty); when false, the field must be strictly longer
	// than Prefix.
	MatchExact bool
}
