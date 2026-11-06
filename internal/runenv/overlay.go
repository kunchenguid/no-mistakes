// Package runenv applies immutable, run-scoped changes to subprocess
// environments without mutating the daemon process environment.
package runenv

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// Overlay describes environment variables to set and remove for one run.
// Set values take precedence when a key also appears in Unset.
type Overlay struct {
	Set   map[string]string
	Unset []string
}

// Clone returns an independent copy suitable for freezing into a run context.
func (o Overlay) Clone() Overlay {
	cloned := Overlay{Unset: append([]string(nil), o.Unset...)}
	if o.Set != nil {
		cloned.Set = make(map[string]string, len(o.Set))
		for key, value := range o.Set {
			cloned.Set[key] = value
		}
	}
	return cloned
}

// Apply returns base with the overlay applied. A nil base uses the current
// process environment. The input slice is never modified.
func (o Overlay) Apply(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	removed := make(map[string]struct{}, len(o.Unset)+len(o.Set))
	for _, key := range o.Unset {
		removed[normalizeKey(key)] = struct{}{}
	}
	for key := range o.Set {
		removed[normalizeKey(key)] = struct{}{}
	}

	result := make([]string, 0, len(base)+len(o.Set))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := removed[normalizeKey(key)]; skip {
			continue
		}
		result = append(result, entry)
	}

	keys := make([]string, 0, len(o.Set))
	for key := range o.Set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+o.Set[key])
	}
	return result
}

// String prints the overlay's variable names only. A Set value can be a
// credential (a forge profile's gh_user token), so formatting an overlay, or a
// struct holding one, must never reveal values.
func (o Overlay) String() string {
	keys := make([]string, 0, len(o.Set))
	for key := range o.Set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "runenv.Overlay{Set: [" + strings.Join(keys, " ") + "], Unset: [" + strings.Join(o.Unset, " ") + "]}"
}

// GoString keeps %#v as redacted as %v.
func (o Overlay) GoString() string {
	return o.String()
}

// Empty reports whether the overlay changes no variables.
func (o Overlay) Empty() bool {
	return len(o.Set) == 0 && len(o.Unset) == 0
}

func normalizeKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
