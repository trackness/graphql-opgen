package genops

import (
	"fmt"
	"maps"
	"slices"
)

// IsPathNamedAllowed reports whether a type is in the caller's audited
// exception set (cfg.PathNamedAllowlist).
func IsPathNamedAllowed(cfg Config, typeName string) bool {
	_, ok := cfg.PathNamedAllowlist[typeName]
	return ok
}

// PathNamedReason returns the audited reason a type is path-named, or "".
func PathNamedReason(cfg Config, typeName string) string {
	return cfg.PathNamedAllowlist[typeName]
}

// AllowedPathNamed returns the allowlisted type names in sorted order.
func AllowedPathNamed(cfg Config) []string {
	return slices.Sorted(maps.Keys(cfg.PathNamedAllowlist))
}

// UnlistedPathNamed returns the path-named types produced by fs that are not in
// the audited allowlist. A non-empty result is a generation drift.
func UnlistedPathNamed(cfg Config, fs *FragmentSet) []string {
	var out []string
	for _, name := range fs.PathNamedTypes() {
		if !IsPathNamedAllowed(cfg, name) {
			out = append(out, name)
		}
	}
	return out
}

// AuditPathNamed returns one human-readable line per path-named type fs emits,
// in sorted type order: an allowlisted type is annotated with its audited reason
// (via [PathNamedReason]); an unlisted type is flagged as drift. It turns a bare
// UnlistedPathNamed drift list into an explanatory report — the allowed reason
// next to each shape — so a reviewer staring at a drift can see why the listed
// shapes are sanctioned and which one is new.
func AuditPathNamed(cfg Config, fs *FragmentSet) []string {
	names := fs.PathNamedTypes()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if reason := PathNamedReason(cfg, name); reason != "" {
			out = append(out, fmt.Sprintf("%s: allowed — %s", name, reason))
		} else {
			out = append(out, fmt.Sprintf("%s: UNLISTED (generation drift)", name))
		}
	}
	return out
}
