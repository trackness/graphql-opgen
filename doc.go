// Package genops compiles a GraphQL SDL into a typed surface for genqlient:
// one operation per root field, the operation-reachable fragments those
// operations spread, a thin manifest indexing the operations, a machine-facing
// catalog of inputs, enums, and deprecations, and a CLI command table.
//
// The compiler is schema-agnostic. It reads strictly from the schema AST
// (gqlparser/v2) and never carries a hand-maintained list of fields or edges,
// so a server upgrade that drifts a field is a red build rather than a silent
// nil. Everything that cannot be derived from the SDL alone is supplied by the
// caller through a [Config]:
//
//   - [ExitCodeProvider] — the caller's exit-code vocabulary. The catalog
//     projects these names into each command's exitCodes array, layering the
//     per-shape extensions (not-found, destructive-refused, the job trio) onto
//     the frozen base set.
//   - [NamingRules] — the irregular plurals, multi-word singulars, pinned
//     subscription paths, verb suffixes, and prefix groups the CLI-path
//     derivation cannot infer from the regular structural rules.
//   - PathNamedAllowlist — the audited set of object/union/interface type names
//     the generator may emit as path-named inline selections, each mapped to
//     the human-readable reason it is exempt from the "no path-named struct"
//     invariant.
//   - TargetPackageImport — the import path of the package holding the generated
//     genqlient operation consts the emitted command table references.
//   - SelectionVariants / VariantEdges — optional trimmed selections for object
//     types, each omitting a set of fields (by name or by directive), routed to
//     specific contexts (a root field or a "Type.field" edge). They let a field
//     a server resolves only in some contexts — an owner-only field, an edge a
//     resolver leaves null against the SDL's non-null typing — be dropped exactly
//     where it would break, leaving the canonical <T>Fields untouched elsewhere.
//
// Field enumeration distinguishes two surfaces:
//
//   - Root operations ([RootFields]) include every field of Query, Mutation,
//     and Subscription, deprecated ones included, so every operation stays
//     reachable from the CLI, with deprecations flagged in the catalog.
//   - Entity selections ([Edges] and [Scalars]) exclude @deprecated fields, so
//     the canonical fragment types carry the clean, current surface; the
//     deprecated fields are still recorded in the catalog.
//
// [Compile] is the single entry point: it loads the SDL directory and overlay,
// validates the overlay against the schema, and returns a [Compiled] holding
// all of the above derived from one schema load, so they cannot drift from each
// other. See README.md for the design rationale and a worked usage example.
package genops
