// Package compare tells whether two backups hold the same server contents,
// setting aside what a server regenerates on its own.
package compare

import (
	"bytes"
	"encoding/json"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
	"github.com/cinc-project/cinc-server-backup/internal/refs"
)

// Kinds of difference.
const (
	Missing = "missing" // in the first backup, not the second
	Changed = "changed" // in both, with different contents
	Extra   = "extra"   // in the second backup, not the first (Options.Extras)
)

// Difference is one way the second backup falls short of the first.
type Difference struct {
	Path   string
	Kind   string
	Before string // the first backup's object (Changed only)
	After  string // the second backup's object (Changed only)
}

// Options tune a comparison.
type Options struct {
	// SkipUsers are users left out, such as each server's own superuser,
	// whose keys differ by design.
	SkipUsers []string
	// Include, when set, limits the comparison to the paths of a it
	// accepts, such as the orgs a partial restore wrote.
	Include func(path string) bool
	// Extras also reports objects only b holds (within Include), such as
	// what a prune should have deleted. Cookbook files are left out: b holds
	// exactly those its manifests use, so an extra manifest already shows.
	Extras bool
}

// usag matches the per-user groups a server names after a user's internal
// id, which every server assigns afresh.
var usag = refs.USAG

// Dropped is a name in one of the first backup's ACLs or groups that the
// backup does not hold (a dangling reference, such as a deleted user) and
// that the second lacks, as a restore leaves it out.
type Dropped struct {
	Path   string
	Detail string // "update: user ghost" for an ACL, "user ghost" for a group
}

// Result is what a comparison found.
type Result struct {
	// Differences are what the second backup lacks or holds differently.
	Differences []Difference
	// Preserved counts the objects and files that match, by the kind the
	// manifest counts them under ("other" for uncounted paths).
	Preserved map[string]int
	// Regenerated counts the objects among those that match only once the
	// values a server regenerates are set aside.
	Regenerated int
	// Dropped lists the dangling names the second backup lacks.
	Dropped []Dropped
	// Skipped counts what is not compared: skipped users' objects, and
	// per-user groups.
	Skipped int
}

// Backups reports every object or file in a that is missing from b or differs
// from b's copy once regenerated values are set aside: org GUIDs, file URLs
// and their order, the URLs in listings, per-user group names, a policy
// group's uri, and the order of members, actors and groups. Objects only in
// b are not reported, so a target may hold more than the backup restored
// into it.
//
// Names in a's ACLs and groups that a does not hold are dangling. When b
// lacks them, as it does after a restore left them out, the object still
// matches and the names are reported as dropped; Backups returns only the
// differences, and Compare everything.
func Backups(a, b *format.Reader, opts Options) ([]Difference, error) {
	res, err := Compare(a, b, opts)
	if err != nil {
		return nil, err
	}
	return res.Differences, nil
}

// Compare is Backups with the full result.
func Compare(a, b *format.Reader, opts Options) (*Result, error) {
	skip := map[string]bool{}
	for _, u := range opts.SkipUsers {
		skip[u] = true
	}
	known := refs.FromBackup(a)
	res := &Result{Preserved: map[string]int{}}
	preserved := func(p string) {
		k := format.CountKey(p)
		if k == "" {
			k = "other"
		}
		res.Preserved[k]++
	}
	for _, e := range a.Manifest().Entries {
		p := e.Path
		if opts.Include != nil && !opts.Include(p) {
			continue
		}
		if skipped(p, skip) {
			res.Skipped++
			continue
		}
		if strings.HasPrefix(p, "blobs/") {
			if _, ok := b.Entry(p); !ok {
				res.Differences = append(res.Differences, Difference{Path: p, Kind: Missing})
			} else {
				preserved(p)
			}
			continue
		}
		da, err := a.Object(p)
		if err != nil {
			return nil, err
		}
		if _, ok := b.Entry(p); !ok {
			res.Differences = append(res.Differences, Difference{Path: p, Kind: Missing})
			continue
		}
		db, err := b.Object(p)
		if err != nil {
			return nil, err
		}
		na, err := normalize(p, da)
		if err != nil {
			return nil, err
		}
		nb, err := normalize(p, db)
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(na, nb) {
			preserved(p)
			if !bytes.Equal(da, db) {
				res.Regenerated++
			}
			continue
		}
		// Unequal: equal once a's dangling names are left out?
		without, dropped, err := withoutDangling(known, p, da)
		if err != nil {
			return nil, err
		}
		if len(dropped) > 0 {
			nw, err := normalize(p, without)
			if err != nil {
				return nil, err
			}
			if reflect.DeepEqual(nw, nb) {
				preserved(p)
				res.Dropped = append(res.Dropped, dropped...)
				continue
			}
		}
		res.Differences = append(res.Differences, Difference{Path: p, Kind: Changed, Before: string(da), After: string(db)})
	}
	if opts.Extras {
		for _, e := range b.Manifest().Entries {
			p := e.Path
			if strings.HasPrefix(p, "blobs/") || (opts.Include != nil && !opts.Include(p)) || skipped(p, skip) {
				continue
			}
			if _, ok := a.Entry(p); !ok {
				res.Differences = append(res.Differences, Difference{Path: p, Kind: Extra})
			}
		}
	}
	return res, nil
}

// listKey is the member list each kind of dangling name sits in.
var listKey = map[string]string{"user": "users", "client": "clients", "actor": "actors", "group": "groups"}

// withoutDangling returns the ACL or group stored at p without the names
// the backup does not hold, and those names. Any other object comes back
// unchanged, with none.
func withoutDangling(k *refs.Known, p string, data []byte) ([]byte, []Dropped, error) {
	o, isACL, isGroup, ok := refs.Scope(p)
	if !ok {
		return data, nil, nil
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, nil, err
	}
	var dropped []Dropped
	remove := func(lists map[string]any, labels []string, prefix string) {
		for _, l := range labels {
			kind, name, _ := strings.Cut(l, " ")
			key := listKey[kind]
			var kept []any
			for _, v := range refs.Strings(lists[key]) {
				if v != name {
					kept = append(kept, v)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			lists[key] = kept
			dropped = append(dropped, Dropped{Path: p, Detail: prefix + l})
		}
	}
	switch {
	case isACL:
		perms := make([]string, 0, len(m))
		for perm := range m {
			perms = append(perms, perm)
		}
		sort.Strings(perms)
		for _, perm := range perms {
			ace, ok := m[perm].(map[string]any)
			if !ok {
				continue
			}
			_, _, d := k.ACE(o, ace)
			remove(ace, d, perm+": ")
		}
	case isGroup:
		_, _, _, d := k.Members(o, m)
		remove(m, d, "")
	}
	if len(dropped) == 0 {
		return data, nil, nil
	}
	out, err := json.Marshal(m)
	return out, dropped, err
}

// skipped reports paths that are not compared: a skipped user and their keys
// and ACL, and per-user groups.
func skipped(p string, users map[string]bool) bool {
	if rest, ok := strings.CutPrefix(p, "server/users/"); ok {
		name, _, _ := strings.Cut(strings.TrimSuffix(rest, ".json"), "/")
		return users[name]
	}
	segs := strings.Split(p, "/")
	return len(segs) == 4 && segs[0] == "orgs" && segs[2] == "groups" && usag.MatchString(strings.TrimSuffix(segs[3], ".json"))
}

// normalize decodes a stored object and removes what a server regenerates.
func normalize(p string, data []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	segs := strings.Split(p, "/")
	m, _ := v.(map[string]any)
	switch {
	case path.Base(p) == "org.json":
		delete(m, "guid")
	case len(segs) >= 3 && (segs[2] == "cookbooks" || segs[2] == "cookbook_artifacts"):
		for _, list := range []string{"all_files", "recipes", "definitions", "libraries", "attributes", "files", "templates", "resources", "providers", "root_files"} {
			files, _ := m[list].([]any)
			for _, f := range files {
				if fm, ok := f.(map[string]any); ok {
					delete(fm, "url")
				}
			}
			sort.Slice(files, func(i, j int) bool { return fileKey(files[i]) < fileKey(files[j]) })
		}
	case len(segs) == 4 && segs[2] == "policy_groups":
		// uri is the group's URL on whichever server.
		delete(m, "uri")
	case len(segs) == 4 && segs[2] == "groups":
		m["groups"] = dropUSAGs(m["groups"])
		delete(m, "actors") // derived from users and clients
		for _, k := range []string{"users", "clients", "groups"} {
			m[k] = sortedStrings(m[k])
		}
	case path.Base(p) == "members.json" || path.Base(p) == "invitations.json":
		return sortedJSON(v), nil
	case len(segs) == 4 && segs[2] == "data_bags":
		// A bag's listing maps item names to URLs on the source's host.
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		return names, nil
	}
	if (len(segs) >= 3 && segs[2] == "acls") || path.Base(p) == "acl.json" {
		for _, ace := range m {
			a, ok := ace.(map[string]any)
			if !ok {
				continue
			}
			a["groups"] = dropUSAGs(a["groups"])
			for k := range a {
				a[k] = sortedStrings(a[k])
			}
		}
	}
	return v, nil
}

func fileKey(f any) string {
	m, _ := f.(map[string]any)
	p, _ := m["path"].(string)
	n, _ := m["name"].(string)
	return p + "\x00" + n
}

func sortedStrings(v any) any {
	l, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, _ := e.(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func dropUSAGs(v any) any {
	l, _ := v.([]any)
	out := []any{}
	for _, e := range l {
		if s, _ := e.(string); !usag.MatchString(s) {
			out = append(out, e)
		}
	}
	return out
}

// sortedJSON returns a list's elements as sorted JSON strings, so order does
// not matter.
func sortedJSON(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		b, _ := json.Marshal(e)
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

// Equal reports whether a and b, two copies of the object stored at path p in
// a backup (one perhaps read from a live server), hold the same thing once the
// values a server regenerates are set aside, as Backups compares them.
func Equal(p string, a, b []byte) (bool, error) {
	na, err := normalize(p, a)
	if err != nil {
		return false, err
	}
	nb, err := normalize(p, b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(na, nb), nil
}
