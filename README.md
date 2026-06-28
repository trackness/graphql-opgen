# graphql-opgen

`graphql-opgen` (package `genops`) is a generic GraphQL code generator. Given a
GraphQL SDL, a small curated overlay, and a `Config`, it derives a typed client
surface straight from the schema:

- **Operations** — one [genqlient](https://github.com/Khan/genqlient) operation
  per root field (query, mutation, subscription), with variables forwarded and
  deprecated arguments dropped.
- **Fragments** — a `<T>Ref` leaf for every referenceable entity and a
  `<T>Fields` fragment for every expandable object, with nested entity edges
  terminating at a `Ref` so the fragment graph stays a finite, acyclic DAG.
- **Manifest** — a thin per-operation index (kind, input/return types, hazard
  flags) keyed to the schema version it was built from.
- **Catalog** — a machine-facing description of every operation plus the
  transitive closure of input objects and enums it references, including
  deprecations and a per-command exit-code list.
- **Command table** — a generated Go table of command specs, one per operation,
  ready for a cobra-style CLI to assemble.

Everything is derived structurally from the schema AST (via
[gqlparser/v2](https://github.com/vektah/gqlparser)). The generator carries no
hand-maintained list of fields or edges, so a server upgrade that drifts a field
is a red build rather than a silent nil — the output feeds genqlient, which then
produces the typed Go bindings.

## Install

```sh
go get github.com/trackness/graphql-opgen
```

## Usage

`genops.Compile` is the single entry point. It loads a directory of `*.graphql`
files and an overlay YAML, validates the overlay against the schema, and returns
everything above derived from one schema load:

```go
package main

import (
	"log"
	"os"

	"github.com/trackness/graphql-opgen"
)

func main() {
	cfg := genops.Config{
		// The caller's exit-code vocabulary; the catalog projects these names
		// into each command's exitCodes list, per command shape.
		ExitCodes: genops.ExitCodeProvider{
			Base:               []string{"OK", "USAGE", "RUNTIME"},
			NotFound:           "NOT_FOUND",
			DestructiveRefused: "REFUSED",
			JobFailed:          "JOB_FAILED",
			StillRunning:       "STILL_RUNNING",
			Unconfirmed:        "UNCONFIRMED",
		},
		// Irregular naming the structural CLI-path rules can't infer.
		Naming: genops.NamingRules{
			SubscriptionPaths: map[string][]string{"orderUpdated": {"order", "watch"}},
			VerbSuffixes:      map[string]string{"Create": "create", "Update": "update"},
			PluralMutationLeaf: map[string]string{
				"Destroy": "destroy-many",
			},
			FallbackGroup: "misc",
		},
		// Audited types the generator may emit as path-named inline selections,
		// each mapped to the reason it is exempt from the no-path-named invariant.
		PathNamedAllowlist: map[string]string{
			"OrderLine": "junction wrapper with no id, inlined",
		},
		// Where the generated operation consts live, and the suffix that forms
		// each one (e.g. OrderCreate_Operation).
		TargetPackageImport:  "example.com/app/ops",
		OperationConstSuffix: "_Operation",
	}

	out, err := genops.Compile("schema", "overlay.yaml", "2026.1", cfg)
	if err != nil {
		log.Fatal(err)
	}

	os.WriteFile("gen_operations.graphql", []byte(out.Operations), 0o644)
	os.WriteFile("gen_fragments.graphql", []byte(out.Fragments), 0o644)

	man, _ := out.Manifest.JSON()
	os.WriteFile("manifest.json", man, 0o644)

	cat, _ := out.Catalog.JSON()
	os.WriteFile("catalog.json", cat, 0o644)

	tbl, err := genops.EmitCommands(out.Manifest, cfg)
	if err != nil {
		log.Fatal(err)
	}
	os.WriteFile("gen_commands.go", tbl, 0o644)
}
```

A minimal overlay names only what the SDL cannot express — which root fields are
data-destroying and which return a background job id rather than their result
inline, both keyed by root-field name:

```yaml
destructive:
  - ordersDestroy
jobReturning:
  - reindexCatalog
```

Unknown keys and unknown field names are rejected, not ignored, so a typo or a
stale entry is a build error.

## Config injection

The compiler core is schema-agnostic; everything service-specific is supplied
through `Config` rather than baked into the generator:

- **`ExitCodeProvider`** — the exit-code vocabulary. The catalog layers per-shape
  codes (not-found for a nullable single-entity lookup, destructive-refused for a
  destructive op, the job trio for a job-returning op) onto the frozen base set.
- **`NamingRules`** — irregular plurals, multi-word singulars, pinned
  subscription paths, mutation verb suffixes, and ordered prefix groups the
  CLI-path derivation cannot infer from the regular structural rules.
- **`PathNamedAllowlist`** — the audited set of object/union/interface type names
  the generator may emit as path-named inline selections, each mapped to the
  reason it is exempt. A path-named type not on the list surfaces as drift.
- **`TargetPackageImport`** / **`OperationConstSuffix`** — the import path and
  suffix that stamp the generated command table's references to the operation
  consts.
- **`SelectionVariants`** / **`VariantEdges`** — optional trimmed selections for
  object types. The full-field selection genops derives from the SDL can hit a
  field a server resolves only in some contexts — an owner-only field, or an edge
  whose resolver returns null against the schema's non-null typing. A variant
  omits exactly those fields (named explicitly, or derived from a directive they
  carry) and is materialised as a distinct `<T><Variant>Fields` fragment, routed
  to the breaking contexts (a root field, or a `Type.field` edge) via
  `VariantEdges`; the canonical `<T>Fields` is untouched everywhere else. Every
  type, field, directive, and route is validated against the schema at compile
  time, so a drifted reference is a red build, not a silent leak.

## License

MIT — see [LICENSE](LICENSE).
