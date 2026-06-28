package genops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

const (
	schemaDir   = "testdata/schema"
	overlayPath = "testdata/overlay.yaml"
	schemaVer   = "test-1"
)

// toyConfig is the per-test Config: a small exit-code vocabulary, naming rules
// wired to the toy SDL's field shapes, an audited path-named allowlist matching
// the value-cycle/mixed-wrapper/abstract types the toy schema produces, and the
// two stamp strings the command table needs.
func toyConfig() Config {
	return Config{
		ExitCodes: ExitCodeProvider{
			Base:               []string{"OK", "USAGE", "RUNTIME"},
			NotFound:           "NOT_FOUND",
			DestructiveRefused: "REFUSED",
			JobFailed:          "JOB_FAILED",
			StillRunning:       "STILL_RUNNING",
			Unconfirmed:        "UNCONFIRMED",
		},
		Naming: NamingRules{
			PluralEntity:   map[string]string{},
			SingularEntity: map[string]string{},
			VerbSuffixes:   map[string]string{"Create": "create", "Update": "update"},
			PluralMutationLeaf: map[string]string{
				"Destroy": "destroy-many",
				"Update":  "update-many",
			},
			SubscriptionPaths: map[string][]string{"orderUpdated": {"order", "watch"}},
			ExactGroups:       map[string][]string{"reindexCatalog": {"catalog", "reindex"}},
			FallbackGroup:     "misc",
		},
		// Matches exactly the path-named types BuildFragments materialises over the
		// toy schema. SearchHit (a union) is only path-named when reached via an
		// operation selection, so it is not in the standalone fragment universe.
		PathNamedAllowlist: map[string]string{
			"Locale":    "value-type cycle Region<->Locale, terminated scalars-only",
			"Region":    "value-type cycle Region<->Locale, terminated scalars-only",
			"OrderLine": "junction wrapper with no id, inlined",
			"Receipt":   "value type expanded inline under a root payload",
			"Channel":   "interface edge, rendered as __typename + per-member inline fragments",
			"Payment":   "conflicting union edge, rendered with member-prefixed aliases",
		},
		TargetPackageImport:  "example.com/app/ops",
		OperationConstSuffix: "_Operation",
	}
}

func mustCompile(t *testing.T) *Compiled {
	t.Helper()
	c, err := Compile(schemaDir, overlayPath, schemaVer, toyConfig())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func mustSchema(t *testing.T) *ast.Schema {
	t.Helper()
	s, err := LoadSchema(schemaDir)
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	return s
}

func mustOverlay(t *testing.T) *Overlay {
	t.Helper()
	o, err := LoadOverlay(overlayPath)
	if err != nil {
		t.Fatalf("LoadOverlay: %v", err)
	}
	return o
}

// TestCompileDeterministic asserts two independent compiles of the same inputs
// are byte-identical across every output surface — the property the whole
// design rests on.
func TestCompileDeterministic(t *testing.T) {
	a := mustCompile(t)
	b := mustCompile(t)

	if a.Fragments != b.Fragments {
		t.Error("fragments differ between runs")
	}
	if a.Operations != b.Operations {
		t.Error("operations differ between runs")
	}

	aMan, err := a.Manifest.JSON()
	if err != nil {
		t.Fatalf("manifest a JSON: %v", err)
	}
	bMan, err := b.Manifest.JSON()
	if err != nil {
		t.Fatalf("manifest b JSON: %v", err)
	}
	if string(aMan) != string(bMan) {
		t.Error("manifest JSON differs between runs")
	}

	aCat, err := a.Catalog.JSON()
	if err != nil {
		t.Fatalf("catalog a JSON: %v", err)
	}
	bCat, err := b.Catalog.JSON()
	if err != nil {
		t.Fatalf("catalog b JSON: %v", err)
	}
	if string(aCat) != string(bCat) {
		t.Error("catalog JSON differs between runs")
	}
}

// TestOperationGeneration checks one operation per root field, correct
// query/mutation/subscription keyword, exported names, variable forwarding, the
// flatten directive for entity payloads, and that a deprecated root field is
// still emitted (deprecated args dropped) while a scalar-returning mutation has
// no selection block.
func TestOperationGeneration(t *testing.T) {
	c := mustCompile(t)
	ops := c.Operations

	wantContains := []string{
		"query Order($id: ID!) {",
		"query Orders($filter: OrderFilter) {",
		"mutation OrderCreate($input: OrderInput!) {",
		"subscription OrderUpdated {",
		"...OrderFields",
		flattenDirective,
		"query LegacyOrders {", // deprecated root field still reachable
	}
	for _, w := range wantContains {
		if !strings.Contains(ops, w) {
			t.Errorf("operations missing %q", w)
		}
	}

	// legacyOrders carries a deprecated `page` argument; it must be dropped from
	// the generated operation's variable list.
	for _, line := range strings.Split(ops, "\n") {
		if strings.HasPrefix(line, "query LegacyOrders") && strings.Contains(line, "page") {
			t.Errorf("deprecated arg leaked into operation: %q", line)
		}
	}

	// A scalar-returning mutation (ordersDestroy -> Boolean!) has no selection set.
	if !strings.Contains(ops, "ordersDestroy(ids: $ids)\n") {
		t.Error("scalar-returning mutation should have no selection block")
	}

	// A union-returning query renders __typename plus a per-member inline fragment.
	for _, w := range []string{"__typename", "... on Product {", "... on Customer {", "... on Order {"} {
		if !strings.Contains(ops, w) {
			t.Errorf("union operation Search missing %q", w)
		}
	}

	// A field argument named after a Go keyword (`type`) is forwarded under a
	// safe variable name (`$type_`).
	if !strings.Contains(ops, "query Search($term: String!, $type_: String)") {
		t.Error("keyword argument not renamed to a safe variable")
	}
	if !strings.Contains(ops, "search(term: $term, type: $type_)") {
		t.Error("keyword argument call site not using the safe variable")
	}

	// Exactly one operation per root field across all three root types.
	s := mustSchema(t)
	want := len(rootFieldNames(s))
	got := strings.Count(ops, "query ") + strings.Count(ops, "mutation ") + strings.Count(ops, "subscription ")
	if got != want {
		t.Errorf("operation count = %d, want %d (one per root field)", got, want)
	}
}

// TestFragmentGeneration checks ref/fields fragments, deprecated-field exclusion
// from the canonical surface, and nested ref-able edges terminating at <T>Ref.
func TestFragmentGeneration(t *testing.T) {
	c := mustCompile(t)
	frag := c.Fragments

	for _, w := range []string{
		"fragment CustomerRef on Customer {",
		"fragment CustomerFields on Customer {",
		"fragment ProductRef on Product {",
		"fragment ProductFields on Product {",
	} {
		if !strings.Contains(frag, w) {
			t.Errorf("fragments missing %q", w)
		}
	}

	// CustomerFields must NOT carry the deprecated legacyCode field.
	if cf := fragmentBody(t, frag, "CustomerFields"); strings.Contains(cf, "legacyCode") {
		t.Errorf("deprecated field leaked into CustomerFields:\n%s", cf)
	}

	// A ref-able nested edge inside a fragment terminates at a Ref, not Fields.
	pf := fragmentBody(t, frag, "ProductFields")
	if !strings.Contains(pf, "...CategoryRef") {
		t.Errorf("ProductFields.category should spread CategoryRef:\n%s", pf)
	}
	if strings.Contains(pf, "...CategoryFields") {
		t.Errorf("ProductFields.category must not recurse into CategoryFields:\n%s", pf)
	}

	// An interface-typed edge (Customer.channel) renders __typename + a spread per
	// implementor; the members agree on field types, so each spreads its Fields.
	cf := fragmentBody(t, frag, "CustomerFields")
	for _, w := range []string{"channel {", "__typename", "... on EmailChannel {", "...EmailChannelFields"} {
		if !strings.Contains(cf, w) {
			t.Errorf("CustomerFields interface edge missing %q:\n%s", w, cf)
		}
	}

	// A union edge whose members disagree on a shared field's type (Order.payment:
	// CardPayment.amount Int vs CashPayment.amount String) is rendered with
	// member-prefixed response aliases, not fragment spreads.
	of := fragmentBody(t, frag, "OrderFields")
	for _, w := range []string{"payment {", "CardPayment_amount: amount", "CashPayment_amount: amount"} {
		if !strings.Contains(of, w) {
			t.Errorf("OrderFields conflicting-union edge missing %q:\n%s", w, of)
		}
	}
	if strings.Contains(of, "...CardPaymentFields") {
		t.Errorf("conflicting union member must not be spread as a fragment:\n%s", of)
	}
}

// TestEdgesAndScalars checks the two entity-surface enumerators split a type's
// fields by underlying kind and exclude deprecated fields.
func TestEdgesAndScalars(t *testing.T) {
	s := mustSchema(t)
	order := s.Types["Order"]

	edges := Edges(s, order)
	edgeNames := fieldNames(edges)
	for _, w := range []string{"customer", "lines", "payment"} {
		if !slices.Contains(edgeNames, w) {
			t.Errorf("Edges(Order) missing %q, got %v", w, edgeNames)
		}
	}
	if slices.Contains(edgeNames, "status") {
		t.Error("Edges should not include the enum field status")
	}

	scalars := fieldNames(Scalars(s, order))
	for _, w := range []string{"id", "name", "status"} {
		if !slices.Contains(scalars, w) {
			t.Errorf("Scalars(Order) missing %q, got %v", w, scalars)
		}
	}

	// Deprecated fields are excluded from both surfaces.
	cust := s.Types["Customer"]
	if slices.Contains(fieldNames(Scalars(s, cust)), "legacyCode") {
		t.Error("Scalars should exclude the deprecated legacyCode field")
	}
}

func fieldNames(fs []*ast.FieldDefinition) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// TestFragmentCycleHandling exercises the recursion/cycle machinery on three
// distinct shapes the toy schema provides:
//   - a self-referential ref-able type (Category.parent) -> terminated at a Ref;
//   - a value-type mutual cycle (Region<->Locale) -> terminated scalars-only and
//     recorded as path-named;
//
// and asserts the resulting fragment graph is acyclic.
func TestFragmentCycleHandling(t *testing.T) {
	fs := BuildFragments(mustSchema(t))

	// The fragment spread graph must be a DAG.
	if cycles := FragmentCycles(fs); len(cycles) != 0 {
		t.Fatalf("fragment graph has cycles: %v", cycles)
	}

	// Self-referential ref-able type: CategoryFields.parent/children -> CategoryRef.
	cf, ok := fs.Fragment("CategoryFields")
	if !ok {
		t.Fatal("CategoryFields not generated")
	}
	if !strings.Contains(cf, "...CategoryRef") {
		t.Errorf("CategoryFields should terminate self-reference at CategoryRef:\n%s", cf)
	}
	if strings.Count(cf, "...CategoryFields") != 0 {
		t.Errorf("CategoryFields must not spread itself:\n%s", cf)
	}

	// Value-type cycle Region<->Locale must be recorded as path-named.
	pn := fs.PathNamedTypes()
	for _, want := range []string{"Locale", "Region"} {
		if !slices.Contains(pn, want) {
			t.Errorf("expected %q in path-named types, got %v", want, pn)
		}
	}
}

// TestOverlayValidation covers the happy path plus every failure mode:
// unknown root-field names, in-list duplicates, and strict rejection of an
// unknown YAML key.
func TestOverlayValidation(t *testing.T) {
	s := mustSchema(t)

	t.Run("valid", func(t *testing.T) {
		if err := mustOverlay(t).Validate(s); err != nil {
			t.Errorf("valid overlay rejected: %v", err)
		}
	})

	t.Run("unknown root field", func(t *testing.T) {
		ov := &Overlay{Destructive: []string{"noSuchField"}}
		err := ov.Validate(s)
		if err == nil || !strings.Contains(err.Error(), "unknown noSuchField") {
			t.Errorf("expected unknown-field error, got %v", err)
		}
	})

	t.Run("duplicate within list", func(t *testing.T) {
		ov := &Overlay{Destructive: []string{"ordersDestroy", "ordersDestroy"}}
		err := ov.Validate(s)
		if err == nil || !strings.Contains(err.Error(), "duplicate ordersDestroy") {
			t.Errorf("expected duplicate error, got %v", err)
		}
	})

	t.Run("cross-list membership allowed", func(t *testing.T) {
		ov := &Overlay{
			Destructive:  []string{"reindexCatalog"},
			JobReturning: []string{"reindexCatalog"},
		}
		if err := ov.Validate(s); err != nil {
			t.Errorf("a field in both lists is legal, got %v", err)
		}
	})

	t.Run("strict unknown YAML key rejected", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte("destructive: []\nbogusKey: [x]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadOverlay(p)
		if err == nil || !strings.Contains(err.Error(), "bogusKey") {
			t.Errorf("expected strict unknown-key rejection, got %v", err)
		}
	})

	t.Run("Compile surfaces overlay error", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "ov.yaml")
		if err := os.WriteFile(p, []byte("destructive:\n  - notAField\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(schemaDir, p, schemaVer, toyConfig()); err == nil {
			t.Error("Compile should fail on an invalid overlay")
		}
	})
}

// TestNamingDerivation exercises kebab, entityGroup, and the full derivePath
// rule set against synthetic field names that hit each branch.
func TestNamingDerivation(t *testing.T) {
	t.Run("kebab", func(t *testing.T) {
		cases := map[string]string{
			"orderCreate": "order-create",
			"HTTPServer":  "http-server",
			"parseURL":    "parse-url",
			"exportCSV":   "export-csv", // trailing acronym run stays together
			"Product":     "product",
			"":            "",
		}
		for in, want := range cases {
			if got := kebab(in); got != want {
				t.Errorf("kebab(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("entityGroup", func(t *testing.T) {
		n := NamingRules{
			PluralEntity:   map[string]string{"Categories": "category"},
			SingularEntity: map[string]string{"OrderLine": "order-line"},
		}
		cases := map[string]string{
			"Categories": "category",   // irregular plural via map
			"OrderLine":  "order-line", // multi-word singular via map
			"Products":   "product",    // regular trailing-s plural
			"Customer":   "customer",   // single-word singular fallthrough
		}
		for in, want := range cases {
			if got := entityGroup(in, n); got != want {
				t.Errorf("entityGroup(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("derivePath", func(t *testing.T) {
		n := NamingRules{
			PluralEntity:       map[string]string{},
			SingularEntity:     map[string]string{},
			VerbSuffixes:       map[string]string{"Create": "create", "Update": "update"},
			PluralMutationLeaf: map[string]string{"Destroy": "destroy-many", "Update": "update-many"},
			SubscriptionPaths:  map[string][]string{"orderUpdated": {"order", "watch"}},
			ExactFinds:         map[string][]string{"findOrderByCode": {"order", "by-code"}},
			ExactGroups:        map[string][]string{"reindexCatalog": {"catalog", "reindex"}},
			PrefixGroups: []PrefixGroupRule{
				{Prefix: "configure", Group: "config", LeafPrefix: "set-"},
			},
			FallbackGroup: "misc",
		}
		cases := []struct {
			field string
			kind  string
			want  []string
		}{
			{"findOrders", "query", []string{"order", "list"}},         // find<E>s -> list
			{"findOrder", "query", []string{"order", "get"}},           // find<E> -> get
			{"findOrderByCode", "query", []string{"order", "by-code"}}, // ExactFinds pin
			{"allOrders", "query", []string{"order", "all"}},           // all<E>s -> all
			{"bulkOrderUpdate", "mutation", []string{"order", "bulk-update"}},
			{"ordersDestroy", "mutation", []string{"order", "destroy-many"}},  // plural mutation
			{"orderCreate", "mutation", []string{"order", "create"}},          // entity verb
			{"configureLimits", "mutation", []string{"config", "set-limits"}}, // prefix group + leaf prefix
			{"reindexCatalog", "mutation", []string{"catalog", "reindex"}},    // exact group
			{"orderUpdated", "subscription", []string{"order", "watch"}},      // pinned subscription
			{"systemStatus", "query", []string{"misc", "system-status"}},      // fallback
		}
		for _, c := range cases {
			got := derivePath(c.field, c.kind, n)
			if !slices.Equal(got, c.want) {
				t.Errorf("derivePath(%q, %q) = %v, want %v", c.field, c.kind, got, c.want)
			}
		}
	})

	t.Run("path collision is an error", func(t *testing.T) {
		m := &Manifest{Operations: []ManifestEntry{
			{Name: "AlphaThing", Field: "alphaThing", Kind: "query"},
			{Name: "BetaThing", Field: "betaThing", Kind: "query"},
		}}
		// Both fall through to the fallback group with distinct kebab leaves, so no
		// collision; force a collision by pointing two ops at the same path.
		cfg := toyConfig()
		cfg.Naming.ExactGroups = map[string][]string{
			"alphaThing": {"x", "y"},
			"betaThing":  {"x", "y"},
		}
		if _, err := BuildCommands(m, cfg); err == nil {
			t.Error("expected a command path collision error")
		}
	})
}

// TestExitCodeMapping checks the catalog layers the caller's exit-code vocabulary
// per command shape: a nullable single-entity query gets NotFound, a destructive
// op gets DestructiveRefused, a job op gets the job trio, and a plain list query
// gets only the base set.
func TestExitCodeMapping(t *testing.T) {
	cfg := toyConfig()
	cat, err := BuildCatalog(mustSchema(t), mustOverlay(t), schemaVer, cfg)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}

	base := cfg.ExitCodes.Base
	cases := map[string][]string{
		"Order":          append(slices.Clone(base), "NOT_FOUND"),                                  // nullable single-entity lookup
		"Orders":         slices.Clone(base),                                                       // list query, never a miss
		"OrdersDestroy":  append(slices.Clone(base), "REFUSED"),                                    // destructive
		"ReindexCatalog": append(slices.Clone(base), "JOB_FAILED", "STILL_RUNNING", "UNCONFIRMED"), // job
	}
	for op, want := range cases {
		cmd, ok := cat.Commands[op]
		if !ok {
			t.Errorf("catalog missing command %q", op)
			continue
		}
		if !slices.Equal(cmd.ExitCodes, want) {
			t.Errorf("%s exitCodes = %v, want %v", op, cmd.ExitCodes, want)
		}
	}
}

// TestCatalogDefsAndDeprecations checks the catalog records deprecations
// (root field, argument, input field, enum value) and resolves the transitive
// $defs closure including a self-referential input object.
func TestCatalogDefsAndDeprecations(t *testing.T) {
	cat, err := BuildCatalog(mustSchema(t), mustOverlay(t), schemaVer, toyConfig())
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}

	if r := cat.Commands["LegacyOrders"].Deprecated; r != "use orders" {
		t.Errorf("LegacyOrders deprecation = %q, want %q", r, "use orders")
	}

	// The self-referential OrderFilter input must appear once in $defs.
	of, ok := cat.Defs["OrderFilter"]
	if !ok {
		t.Fatal("OrderFilter missing from $defs")
	}
	if of.Kind != "input" {
		t.Errorf("OrderFilter kind = %q, want input", of.Kind)
	}

	// The OrderStatus enum must be reachable (via OrderFilter.status / OrderInput)
	// and carry its deprecated value.
	os, ok := cat.Defs["OrderStatus"]
	if !ok {
		t.Fatal("OrderStatus missing from $defs")
	}
	var sawDeprecatedValue bool
	for _, v := range os.Values {
		if v.Value == "CANCELLED" && v.Deprecated != "" {
			sawDeprecatedValue = true
		}
	}
	if !sawDeprecatedValue {
		t.Error("deprecated enum value CANCELLED not recorded in catalog")
	}
}

// TestManifestEntries checks the manifest indexes every root field with its
// overlay-derived hazard flags and deprecation reason.
func TestManifestEntries(t *testing.T) {
	m, err := BuildManifest(mustSchema(t), mustOverlay(t), schemaVer, toyConfig())
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.SchemaVersion != schemaVer {
		t.Errorf("SchemaVersion = %q, want %q", m.SchemaVersion, schemaVer)
	}

	by := map[string]ManifestEntry{}
	for _, e := range m.Operations {
		by[e.Name] = e
	}
	if e := by["OrdersDestroy"]; !e.Destructive {
		t.Error("OrdersDestroy should be flagged destructive")
	}
	if e := by["ReindexCatalog"]; !e.JobReturning {
		t.Error("ReindexCatalog should be flagged job-returning")
	}
	if e := by["OrderCreate"]; e.InputType != "OrderInput" {
		t.Errorf("OrderCreate InputType = %q, want OrderInput", e.InputType)
	}
	if e := by["LegacyOrders"]; !e.Deprecated || e.DeprecationReason != "use orders" {
		t.Errorf("LegacyOrders deprecation not recorded: %+v", e)
	}

	// Sorted by Name.
	names := make([]string, len(m.Operations))
	for i, e := range m.Operations {
		names[i] = e.Name
	}
	if !slices.IsSorted(names) {
		t.Errorf("manifest operations not sorted: %v", names)
	}
}

// TestPathNamedAllowlist exercises the audited exception set: a complete
// allowlist clears drift, an empty allowlist reports every path-named type, and
// the audit report annotates allowed types with their reason while flagging the
// unlisted ones.
func TestPathNamedAllowlist(t *testing.T) {
	fs := BuildFragments(mustSchema(t))
	produced := fs.PathNamedTypes()
	if len(produced) == 0 {
		t.Fatal("expected the toy schema to produce path-named types")
	}

	t.Run("complete allowlist clears drift", func(t *testing.T) {
		cfg := toyConfig()
		if drift := UnlistedPathNamed(cfg, fs); len(drift) != 0 {
			t.Errorf("complete allowlist should have no drift, got %v", drift)
		}
		for _, name := range produced {
			if !IsPathNamedAllowed(cfg, name) {
				t.Errorf("%q produced but not allowed", name)
			}
			if PathNamedReason(cfg, name) == "" {
				t.Errorf("%q allowed but carries no reason", name)
			}
		}
		if !slices.Equal(AllowedPathNamed(cfg), produced) {
			t.Errorf("AllowedPathNamed = %v, produced = %v", AllowedPathNamed(cfg), produced)
		}
	})

	t.Run("empty allowlist reports all as drift", func(t *testing.T) {
		cfg := toyConfig()
		cfg.PathNamedAllowlist = map[string]string{}
		if drift := UnlistedPathNamed(cfg, fs); !slices.Equal(drift, produced) {
			t.Errorf("empty allowlist drift = %v, want %v", drift, produced)
		}
	})

	t.Run("audit annotates allowed and flags unlisted", func(t *testing.T) {
		cfg := toyConfig()
		delete(cfg.PathNamedAllowlist, "Receipt") // make exactly one type drift
		report := AuditPathNamed(cfg, fs)
		var sawAllowed, sawUnlisted bool
		for _, line := range report {
			if strings.HasPrefix(line, "Region: allowed — ") {
				sawAllowed = true
			}
			if strings.HasPrefix(line, "Receipt: UNLISTED") {
				sawUnlisted = true
			}
		}
		if !sawAllowed {
			t.Errorf("audit did not annotate an allowed type with its reason:\n%s", strings.Join(report, "\n"))
		}
		if !sawUnlisted {
			t.Errorf("audit did not flag the unlisted type as drift:\n%s", strings.Join(report, "\n"))
		}
	})
}

// TestEmitCommands checks the generated command table is gofmt-clean Go that
// references the configured target package and operation-const suffix.
func TestEmitCommands(t *testing.T) {
	m, err := BuildManifest(mustSchema(t), mustOverlay(t), schemaVer, toyConfig())
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	src, err := EmitCommands(m, toyConfig())
	if err != nil {
		t.Fatalf("EmitCommands: %v", err)
	}
	out := string(src)
	for _, w := range []string{
		"// Code generated by genops; DO NOT EDIT.",
		`import "example.com/app/ops"`,
		"ops.OrderCreate_Operation",
		"var generatedCommands = []commandSpec{",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("command table missing %q", w)
		}
	}

	// A caller-supplied CommandTableDoc replaces the generic fallback comment.
	cfg := toyConfig()
	cfg.CommandTableDoc = []string{"first custom doc line", "second custom doc line"}
	custom, err := EmitCommands(m, cfg)
	if err != nil {
		t.Fatalf("EmitCommands (custom doc): %v", err)
	}
	co := string(custom)
	if !strings.Contains(co, "// first custom doc line") || !strings.Contains(co, "// second custom doc line") {
		t.Errorf("custom CommandTableDoc not emitted:\n%s", co)
	}
	if strings.Contains(co, "buildRootCommand assembles") {
		t.Error("generic fallback doc emitted despite a custom CommandTableDoc")
	}
}

// TestLoadSchemaErrors covers the schema loader's failure paths.
func TestLoadSchemaErrors(t *testing.T) {
	if _, err := LoadSchema(t.TempDir()); err == nil {
		t.Error("empty schema dir should error")
	}
	if _, err := LoadSchema(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing schema dir should error")
	}
}

// fragmentBody returns the single fragment block named name out of a multi-
// fragment blob, for substring assertions scoped to one fragment.
func fragmentBody(t *testing.T, blob, name string) string {
	t.Helper()
	marker := "fragment " + name + " on "
	i := strings.Index(blob, marker)
	if i < 0 {
		t.Fatalf("fragment %q not found", name)
	}
	rest := blob[i:]
	// A fragment block ends at the first line that is a lone "}".
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+3]
}

// opBlock returns the source of the named operation (e.g. "Customer" -> the
// `query Customer(...) { ... }` block), up to its closing brace line.
func opBlock(t *testing.T, blob, name string) string {
	t.Helper()
	for _, kw := range [...]string{"query " + name, "mutation " + name, "subscription " + name} {
		for _, marker := range [...]string{kw + " {", kw + "("} {
			if i := strings.Index(blob, marker); i >= 0 {
				rest := blob[i:]
				if end := strings.Index(rest, "\n}\n"); end >= 0 {
					return rest[:end+3]
				}
				return rest
			}
		}
	}
	t.Fatalf("operation %q not found", name)
	return ""
}

// variantConfig augments the toy config with selection variants: a
// directive-derived public variant of Customer (dropping the @private email) and
// a field-based minimal variant of Address (dropping city), routed at the
// `customer` root payload and the Customer.address edge respectively.
func variantConfig() Config {
	cfg := toyConfig()
	cfg.SelectionVariants = map[string]map[string]VariantExclude{
		"Customer": {"Public": {Directives: []string{"private"}}},
		"Address":  {"Minimal": {Fields: []string{"city"}}},
	}
	cfg.VariantEdges = map[string]string{
		"customer":         "Customer/Public",
		"Customer.address": "Address/Minimal",
	}
	return cfg
}

// TestSelectionVariants checks that a routed variant replaces the canonical
// <T>Fields exactly at the configured context — at a root payload and at a
// nested edge — drops the right fields (by directive and by name), still routes
// nested edges, leaves the canonical fragment intact, and stays deterministic.
func TestSelectionVariants(t *testing.T) {
	// Canonical (no variants): the customer payload spreads the full CustomerFields,
	// which carries email and a full Address edge carrying city.
	base, err := Compile(schemaDir, overlayPath, schemaVer, toyConfig())
	if err != nil {
		t.Fatalf("Compile base: %v", err)
	}
	if !strings.Contains(base.Operations, "...CustomerFields") {
		t.Error("canonical customer op should spread CustomerFields")
	}
	if cf := fragmentBody(t, base.Fragments, "CustomerFields"); !strings.Contains(cf, "email") {
		t.Errorf("canonical CustomerFields should carry email:\n%s", cf)
	}
	if af := fragmentBody(t, base.Fragments, "AddressFields"); !strings.Contains(af, "city") {
		t.Errorf("canonical AddressFields should carry city:\n%s", af)
	}

	// With variants: the customer payload spreads CustomerPublicFields (no email),
	// whose address edge spreads AddressMinimalFields (no city).
	c, err := Compile(schemaDir, overlayPath, schemaVer, variantConfig())
	if err != nil {
		t.Fatalf("Compile variants: %v", err)
	}
	// The routing is scoped to the customer payload: that operation block spreads
	// the variant, while the canonical CustomerFields stays in use elsewhere (the
	// `search` union member ... on Customer, which is not routed).
	custOp := opBlock(t, c.Operations, "Customer")
	if !strings.Contains(custOp, "...CustomerPublicFields") {
		t.Errorf("customer op should spread the routed variant CustomerPublicFields:\n%s", custOp)
	}
	if strings.Contains(custOp, "...CustomerFields") {
		t.Errorf("customer op must spread the variant, not the canonical CustomerFields:\n%s", custOp)
	}
	if !strings.Contains(c.Operations, "...CustomerFields") {
		t.Error("canonical CustomerFields should still be spread by the unrouted search union member")
	}

	cpf := fragmentBody(t, c.Fragments, "CustomerPublicFields")
	if strings.Contains(cpf, "email") {
		t.Errorf("CustomerPublicFields must drop the @private email:\n%s", cpf)
	}
	for _, w := range []string{"id", "name", "...AddressMinimalFields"} {
		if !strings.Contains(cpf, w) {
			t.Errorf("CustomerPublicFields missing %q (a nested edge must still route):\n%s", w, cpf)
		}
	}

	amf := fragmentBody(t, c.Fragments, "AddressMinimalFields")
	if strings.Contains(amf, "city") {
		t.Errorf("AddressMinimalFields must drop city:\n%s", amf)
	}
	if !strings.Contains(amf, "street") {
		t.Errorf("AddressMinimalFields should keep street:\n%s", amf)
	}

	c2, err := Compile(schemaDir, overlayPath, schemaVer, variantConfig())
	if err != nil {
		t.Fatalf("Compile variants (2): %v", err)
	}
	if c.Fragments != c2.Fragments || c.Operations != c2.Operations {
		t.Error("variant compile is not deterministic")
	}
}

// TestSelectionVariantValidation checks that every malformed variant or route is
// a Compile error — drift in the config is a red build, not a silent leak.
func TestSelectionVariantValidation(t *testing.T) {
	cases := []struct {
		name     string
		variants map[string]map[string]VariantExclude
		edges    map[string]string
		wantErr  string
	}{
		{"unknown type", map[string]map[string]VariantExclude{"Nope": {"V": {Fields: []string{"x"}}}}, nil, "not an object type"},
		{"unknown field", map[string]map[string]VariantExclude{"Customer": {"V": {Fields: []string{"nope"}}}}, nil, "does not exist"},
		{"directive matches nothing", map[string]map[string]VariantExclude{"Customer": {"V": {Directives: []string{"nope"}}}}, nil, "carries directive"},
		{"excludes nothing", map[string]map[string]VariantExclude{"Customer": {"V": {}}}, nil, "excludes no fields"},
		{"excludes everything", map[string]map[string]VariantExclude{"Address": {"V": {Fields: []string{"street", "city"}}}}, nil, "every selectable field"},
		{
			"bad route format",
			map[string]map[string]VariantExclude{"Customer": {"Public": {Directives: []string{"private"}}}},
			map[string]string{"customer": "Customer-Public"}, "Type/Variant",
		},
		{
			"unknown variant in edge",
			map[string]map[string]VariantExclude{"Customer": {"Public": {Directives: []string{"private"}}}},
			map[string]string{"customer": "Customer/Nope"}, "no such variant",
		},
		{
			"context not in schema",
			map[string]map[string]VariantExclude{"Customer": {"Public": {Directives: []string{"private"}}}},
			map[string]string{"nosuchfield": "Customer/Public"}, "not a root field",
		},
		{
			"edge target mismatch",
			map[string]map[string]VariantExclude{"Customer": {"Public": {Directives: []string{"private"}}}},
			map[string]string{"order": "Customer/Public"}, "targets type Order",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := toyConfig()
			cfg.SelectionVariants = tc.variants
			cfg.VariantEdges = tc.edges
			if _, err := Compile(schemaDir, overlayPath, schemaVer, cfg); err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
