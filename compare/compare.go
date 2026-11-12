// Package compare tells whether two backups hold the same server contents,
// setting aside what a server regenerates on its own.
package compare

import (
	"bytes"
	"encoding/json"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
)

// Kinds of difference.
const (
	Missing = "missing" // in the first backup, not the second
	Changed = "changed" // in both, with different contents
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
}

// usag matches the per-user groups a server names after a user's internal
// id, which every server assigns afresh.
var usag = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Backups reports every object or file in a that is missing from b or differs
// from b's copy once regenerated values are set aside: org GUIDs, file URLs
// and their order, the URLs in listings, per-user group names, a policy
// group's uri and name, a cookbook's missing "frozen?" (erchef's default,
// false), and the order of members, actors and groups. Objects only in b are not reported, so a
// target may hold more than the backup restored into it.
func Backups(a, b *format.Reader, opts Options) ([]Difference, error) {
	skip := map[string]bool{}
	for _, u := range opts.SkipUsers {
		skip[u] = true
	}
	var out []Difference
	for _, e := range a.Manifest().Entries {
		p := e.Path
		if skipped(p, skip) {
			continue
		}
		if strings.HasPrefix(p, "blobs/") {
			if _, ok := b.Entry(p); !ok {
				out = append(out, Difference{Path: p, Kind: Missing})
			}
			continue
		}
		da, err := a.Object(p)
		if err != nil {
			return nil, err
		}
		if _, ok := b.Entry(p); !ok {
			out = append(out, Difference{Path: p, Kind: Missing})
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
		if !reflect.DeepEqual(na, nb) {
			out = append(out, Difference{Path: p, Kind: Changed, Before: string(da), After: string(db)})
		}
	}
	return out, nil
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
		if _, ok := m["frozen?"]; !ok {
			m["frozen?"] = false // erchef's default
		}
	case len(segs) == 4 && segs[2] == "policy_groups":
		// uri is the group's URL on whichever server; the name is the path's.
		delete(m, "uri")
		delete(m, "name")
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
