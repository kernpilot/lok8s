package kapply

import (
	"regexp"
	"sort"
)

// Conflict markers kubectl emits when an apply cannot proceed without a
// recreate (bash: _KAPPLY_IMMUTABLE_RE / _KAPPLY_TERMINATING_RE). SHARED
// with the bootstrap scheduler: its reap-park detection matches the UNION of
// these so it parks exactly the conflicts Applier.Apply heals — add a new
// conflict class HERE, in one place, or the two would silently drift
// (park-but-not-heal, or heal-but-not-park).
var (
	ImmutableRe   = regexp.MustCompile(`field is immutable`)
	TerminatingRe = regexp.MustCompile(`object is being deleted|being deleted:|because it is being terminated`)
)

var terminatingNsRe = regexp.MustCompile(`in namespace [a-z0-9][a-z0-9-]* because it is being terminated`)

// TerminatingNamespaces reads an apply's error output and returns the
// namespaces a terminating-heal would FORCE-FINALIZE, deduped and sorted
// (bash: kapply::terminating_namespaces — grep | sed | sort -u). The
// apiserver's 403 on any write INTO a terminating namespace names it, and
// that name is the whole input to the force-finalize.
//
// ONE definition because two callers need this exact set: the terminating
// heal finalizes it, and bootstrap's batched recreate prompt must NAME it
// before asking for consent. Deriving it twice is how a consent prompt and
// the action it authorises drift apart — and that prompt is the only thing
// standing between an operator and an irreversible deletion.
func TerminatingNamespaces(out string) []string {
	seen := map[string]bool{}
	for _, m := range terminatingNsRe.FindAllString(out, -1) {
		ns := m[len("in namespace ") : len(m)-len(" because it is being terminated")]
		seen[ns] = true
	}
	var names []string
	for ns := range seen {
		names = append(names, ns)
	}
	sort.Strings(names)
	return names
}
