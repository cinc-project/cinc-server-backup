package restore

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
)

// Dangling says what restore does with an ACL entry or group member that
// names a user, client or group the backup does not hold, such as a user
// deleted after being granted access to a node.
type Dangling string

const (
	// DanglingDrop leaves the name out and reports it. A server refuses a
	// whole ACL that names one unknown actor, so keeping it would lose every
	// other entry of that ACL.
	DanglingDrop Dangling = "drop"
	// DanglingFail stops before anything is written, listing every one.
	DanglingFail Dangling = "fail"
)

// ParseDangling checks a --dangling value.
func ParseDangling(s string) (Dangling, error) {
	switch d := Dangling(s); d {
	case DanglingDrop, DanglingFail:
		return d, nil
	}
	return "", fmt.Errorf("unknown dangling mode %q (want drop or fail)", s)
}

// aclPaths lists the backed-up ACLs of org o's objects, leaving out those of
// per-user groups, which restore does not create.
func (rs *restorer) aclPaths(o string) []string {
	prefix := "orgs/" + o + "/acls/"
	var out []string
	for _, p := range rs.r.Paths(prefix) {
		rest := strings.TrimPrefix(p, prefix)
		if strings.Count(rest, "/") != 1 {
			continue
		}
		typ, file := path.Split(rest)
		if typ == "groups/" && usag.MatchString(strings.TrimSuffix(file, ".json")) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// resolve checks every ACL and group in the backup against the names the
// target will have, before anything is written. It returns a warning for
// each name restore leaves out: per-user groups in ACLs, and dangling names.
// With DanglingFail, any dangling name is an error instead.
func (rs *restorer) resolve(ctx context.Context, mode Dangling) ([]format.Warning, error) {
	var warns, dangling []format.Warning
	checkACL := func(p, o string) error {
		acl, err := rs.object(p)
		if err != nil {
			return err
		}
		for _, perm := range perms {
			ace, ok := acl[perm].(map[string]any)
			if !ok {
				continue
			}
			_, usags, d := rs.refs.ACE(o, ace)
			for _, g := range usags {
				warns = append(warns, format.Warning{Code: "acl.usag_dropped", Path: p, Detail: perm + ": per-user group " + g})
			}
			for _, n := range d {
				dangling = append(dangling, format.Warning{Code: "acl.dangling_dropped", Path: p, Detail: perm + ": " + n})
			}
		}
		return nil
	}
	for _, o := range rs.orgs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, p := range rs.aclPaths(o) {
			if err := checkACL(p, o); err != nil {
				return nil, err
			}
		}
		if _, ok := rs.r.Entry("orgs/" + o + "/acl.json"); ok {
			if err := checkACL("orgs/"+o+"/acl.json", o); err != nil {
				return nil, err
			}
		}
		for _, g := range rs.names("orgs/" + o + "/groups/") {
			if usag.MatchString(g) {
				continue
			}
			p := "orgs/" + o + "/groups/" + g + ".json"
			doc, err := rs.object(p)
			if err != nil {
				return nil, err
			}
			_, _, _, d := rs.refs.Members(o, doc)
			for _, n := range d {
				dangling = append(dangling, format.Warning{Code: "group.dangling_dropped", Path: p, Detail: n})
			}
		}
	}
	for _, u := range rs.users() {
		p := "server/users/" + u + "/acl.json"
		if _, ok := rs.r.Entry(p); ok {
			if err := checkACL(p, ""); err != nil {
				return nil, err
			}
		}
	}
	if len(dangling) > 0 && mode == DanglingFail {
		return nil, danglingError(dangling)
	}
	return append(warns, dangling...), nil
}

func danglingError(ws []format.Warning) error {
	sort.Slice(ws, func(i, j int) bool { return ws[i].Path+ws[i].Detail < ws[j].Path+ws[j].Detail })
	const show = 20
	var lines []string
	for i, w := range ws {
		if i == show {
			lines = append(lines, fmt.Sprintf("  and %d more", len(ws)-show))
			break
		}
		lines = append(lines, "  "+w.Path+": "+w.Detail)
	}
	return fmt.Errorf("the backup names %d users, clients or groups it does not hold, so nothing was restored (--dangling=drop leaves them out):\n%s",
		len(ws), strings.Join(lines, "\n"))
}
