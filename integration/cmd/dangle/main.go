// Command dangle copies a backup and makes it name a user and a client it
// does not hold, as a backup of an erchef server does when someone was
// deleted after being granted access: erchef leaves the names in ACLs and
// groups, while cinc-server-ng removes them, so a real ng source never has
// any. .github/scripts/e2e-ng.sh uses it to exercise restore's --dangling handling.
//
// In the first org, the first node's update ACL gets the user ghost-user and
// the client ghost-client, and the admins group gets ghost-user. Every other
// member is copied byte for byte. It prints one line per injected name, in
// the form restore reports it.
//
// Usage: dangle <in.csb> <out.csb>
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
)

const (
	ghostUser   = "ghost-user"
	ghostClient = "ghost-client"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: dangle <in.csb> <out.csb>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dangle:", err)
		os.Exit(1)
	}
}

// target picks the node ACL and group to edit: the first org's first node
// that has an ACL, and that org's admins group.
func target(r *format.Reader) (acl, group string, err error) {
	for _, p := range r.Paths("orgs/") {
		segs := strings.Split(p, "/")
		if len(segs) == 5 && segs[2] == "acls" && segs[3] == "nodes" {
			o := segs[1]
			group = "orgs/" + o + "/groups/admins.json"
			if _, ok := r.Entry(group); !ok {
				return "", "", fmt.Errorf("org %s has a node ACL but no admins group", o)
			}
			return p, group, nil
		}
	}
	return "", "", fmt.Errorf("the backup has no node ACL to edit")
}

func appendName(m map[string]any, key, name string) {
	l, _ := m[key].([]any)
	m[key] = append(l, name)
}

func run(in, out string, report io.Writer) error {
	r, fh, err := format.Open(in)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }() // read-only
	aclPath, groupPath, err := target(r)
	if err != nil {
		return err
	}
	edit := func(p string, data []byte) ([]byte, error) {
		var m map[string]any
		switch p {
		case aclPath:
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			ace, ok := m["update"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s has no update permission", p)
			}
			appendName(ace, "users", ghostUser)
			appendName(ace, "clients", ghostClient)
			_, _ = fmt.Fprintf(report, "acl.dangling_dropped %s update: user %s\n", p, ghostUser)
			_, _ = fmt.Fprintf(report, "acl.dangling_dropped %s update: client %s\n", p, ghostClient)
		case groupPath:
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			appendName(m, "users", ghostUser)
			_, _ = fmt.Fprintf(report, "group.dangling_dropped %s user %s\n", p, ghostUser)
		default:
			return data, nil
		}
		return json.Marshal(m)
	}

	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	m := r.Manifest()
	w := format.NewWriter(f, m.Source)
	w.SetTool(m.Tool)
	for _, warn := range m.Warnings {
		w.Warn(warn)
	}
	if err := copyEntries(r, w, m.Entries, edit); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func copyEntries(r *format.Reader, w *format.Writer, entries []format.Entry, edit func(string, []byte) ([]byte, error)) error {
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "blobs/") {
			sum := filepath.Base(e.Path)
			rc, err := r.Blob(sum)
			if err != nil {
				return err
			}
			err = w.PutBlob(sum, rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
			continue
		}
		data, err := r.Object(e.Path)
		if err != nil {
			return err
		}
		if data, err = edit(e.Path, data); err != nil {
			return err
		}
		meta := format.Meta{Source: e.Source, APIVersion: e.APIVersion, FetchedAt: e.FetchedAt}
		if err := w.PutObject(e.Path, data, meta); err != nil {
			return err
		}
	}
	return nil
}
