package genops

import "strings"

// Compiled is the full output of a genops run: the genqlient operation and
// fragment source, plus the manifest and catalog, all derived from one schema
// load so they cannot drift from each other.
type Compiled struct {
	Fragments  string // every operation-reachable fragment, sorted by name
	Operations string // every root-field operation, sorted by name
	Manifest   *Manifest
	Catalog    *Catalog
}

// Compile loads the vendored SDL and curated overlay and produces the complete
// generated surface.
//
// The canonical fragment universe is built first, in sorted type order, so a
// fragment's shape never depends on which operation first needs it (a lazy,
// operation-driven build can break a value-type cycle like Address<->Region
// the other way, or leak result-wrapper path state into a fragment). Operations
// then spread those pre-built fragments, and only the fragments actually reached
// from an operation are emitted — genqlient rejects unused fragments, and the
// type universe is far larger than the reachable set.
func Compile(schemaDir, overlayPath, schemaVersion string, cfg Config) (*Compiled, error) {
	s, err := LoadSchema(schemaDir)
	if err != nil {
		return nil, err
	}
	ov, err := LoadOverlay(overlayPath)
	if err != nil {
		return nil, err
	}
	if err := ov.Validate(s); err != nil {
		return nil, err
	}

	fs := BuildFragments(s)
	ops, err := BuildOperations(s, fs)
	if err != nil {
		return nil, err
	}

	var frag strings.Builder
	for _, name := range reachableFragments(fs, ops) {
		body, _ := fs.Fragment(name)
		frag.WriteString(body)
		frag.WriteByte('\n')
	}

	var oper strings.Builder
	for _, op := range ops {
		oper.WriteString(op.Text)
		oper.WriteByte('\n')
	}

	manifest, err := BuildManifest(s, ov, schemaVersion, cfg)
	if err != nil {
		return nil, err
	}
	catalog, err := BuildCatalog(s, ov, schemaVersion, cfg)
	if err != nil {
		return nil, err
	}

	return &Compiled{
		Fragments:  frag.String(),
		Operations: oper.String(),
		Manifest:   manifest,
		Catalog:    catalog,
	}, nil
}
