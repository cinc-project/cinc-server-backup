package refs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
)

// Selection is the part of a backup a partial restore writes: some of its
// organizations, and the users those depend on.
type Selection struct {
	// Orgs are the selected organizations, sorted.
	Orgs []string
	// Users are the backup's users the selected orgs name, sorted: members,
	// invitees, group members and ACL entries, and the users named in those
	// users' own ACLs.
	Users []string
	// All is true when nothing was left out.
	All bool

	orgs, users, blobs map[string]bool
}

// Select picks the organizations of r named in include (all when empty)
// minus those in exclude, and the users they depend on. Naming an
// organization the backup does not hold is an error.
func Select(r *format.Reader, include, exclude []string) (*Selection, error) {
	all := Orgs(r)
	have := map[string]bool{}
	for _, o := range all {
		have[o] = true
	}
	var unknown []string
	for _, o := range append(append([]string{}, include...), exclude...) {
		if !have[o] {
			unknown = append(unknown, o)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("the backup has no organization %s (it has %s)", strings.Join(unknown, ", "), strings.Join(all, ", "))
	}
	s := &Selection{orgs: map[string]bool{}, users: map[string]bool{}, blobs: map[string]bool{}}
	skip := map[string]bool{}
	for _, o := range exclude {
		skip[o] = true
	}
	want := map[string]bool{}
	for _, o := range include {
		want[o] = true
	}
	for _, o := range all {
		if (len(include) == 0 || want[o]) && !skip[o] {
			s.orgs[o] = true
			s.Orgs = append(s.Orgs, o)
		}
	}
	if len(s.Orgs) == 0 {
		return nil, fmt.Errorf("that leaves no organization to restore")
	}
	s.All = len(s.Orgs) == len(all)
	sort.Strings(s.Orgs)

	backupUsers := map[string]bool{}
	for _, u := range names(r, "server/users/") {
		backupUsers[u] = true
	}
	if s.All {
		s.users = backupUsers
	} else if err := s.dependentUsers(r, backupUsers); err != nil {
		return nil, err
	}
	for u := range s.users {
		s.Users = append(s.Users, u)
	}
	sort.Strings(s.Users)
	return s, s.collectBlobs(r)
}

// dependentUsers fills s.users with the backup users the selected orgs name,
// directly or through those users' own ACLs.
func (s *Selection) dependentUsers(r *format.Reader, backupUsers map[string]bool) error {
	var queue []string
	add := func(names ...string) {
		for _, n := range names {
			if backupUsers[n] && !s.users[n] {
				s.users[n] = true
				queue = append(queue, n)
			}
		}
	}
	addACL := func(p string) error {
		m, err := decode(r, p)
		if err != nil {
			return err
		}
		for _, ace := range m {
			a, _ := ace.(map[string]any)
			add(Strings(a["users"])...)
			add(Strings(a["actors"])...)
		}
		return nil
	}
	for _, o := range s.Orgs {
		if data, err := r.Object("orgs/" + o + "/members.json"); err == nil {
			var members []struct {
				User struct {
					Username string `json:"username"`
				} `json:"user"`
			}
			if err := json.Unmarshal(data, &members); err != nil {
				return fmt.Errorf("orgs/%s/members.json: %w", o, err)
			}
			for _, m := range members {
				add(m.User.Username)
			}
		}
		if data, err := r.Object("orgs/" + o + "/invitations.json"); err == nil {
			var invites []struct {
				Username string `json:"username"`
			}
			if err := json.Unmarshal(data, &invites); err != nil {
				return fmt.Errorf("orgs/%s/invitations.json: %w", o, err)
			}
			for _, inv := range invites {
				add(inv.Username)
			}
		}
		for _, g := range names(r, "orgs/"+o+"/groups/") {
			m, err := decode(r, "orgs/"+o+"/groups/"+g+".json")
			if err != nil {
				return err
			}
			add(Strings(m["users"])...)
		}
		for _, p := range r.Paths("orgs/" + o + "/acls/") {
			if err := addACL(p); err != nil {
				return err
			}
		}
		if _, ok := r.Entry("orgs/" + o + "/acl.json"); ok {
			if err := addACL("orgs/" + o + "/acl.json"); err != nil {
				return err
			}
		}
	}
	// A user's own ACL may name other users, who must then exist too.
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		p := "server/users/" + u + "/acl.json"
		if _, ok := r.Entry(p); ok {
			if err := addACL(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectBlobs records the cookbook files the selected orgs' manifests use.
func (s *Selection) collectBlobs(r *format.Reader) error {
	for _, o := range s.Orgs {
		for _, coll := range []string{"cookbooks", "cookbook_artifacts"} {
			for _, p := range r.Paths("orgs/" + o + "/" + coll + "/") {
				data, err := r.Object(p)
				if err != nil {
					return err
				}
				files, err := format.ManifestFiles(data)
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				for _, f := range files {
					s.blobs[f.Checksum] = true
				}
			}
		}
	}
	return nil
}

// Includes reports whether the backup member at p belongs to the selection:
// a selected user's objects, a selected org's, or a cookbook file one of
// those orgs uses. Anything else (host tier, secrets) belongs only to a
// selection of everything.
func (s *Selection) Includes(p string) bool {
	segs := strings.Split(p, "/")
	switch {
	case len(segs) >= 3 && segs[0] == "server" && segs[1] == "users":
		name, _, _ := strings.Cut(strings.TrimSuffix(segs[2], ".json"), "/")
		return s.users[name]
	case len(segs) >= 2 && segs[0] == "orgs":
		return s.orgs[segs[1]]
	case segs[0] == "blobs":
		return s.blobs[segs[len(segs)-1]]
	}
	return s.All
}

func decode(r *format.Reader, p string) (map[string]any, error) {
	data, err := r.Object(p)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return m, nil
}
