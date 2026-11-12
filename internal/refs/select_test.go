package refs

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/cinc-project/cinc-server-backup/format"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func build(t *testing.T, objs map[string]string, blobs ...string) *format.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{})
	for p, data := range objs {
		if err := w.PutObject(p, []byte(data), format.Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range blobs {
		if err := w.PutBlob(md5hex(b), strings.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := format.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Two orgs. acme names alice (member), carol (node ACL only), frank
// (invitee) and gina (group member); carol's own ACL names dave. beta names
// bob. erin is named by nobody.
func twoOrgs(t *testing.T) *format.Reader {
	user := func(n string) string { return `{"username":"` + n + `"}` }
	return build(t, map[string]string{
		"server/users/alice.json":              user("alice"),
		"server/users/bob.json":                user("bob"),
		"server/users/carol.json":              user("carol"),
		"server/users/dave.json":               user("dave"),
		"server/users/erin.json":               user("erin"),
		"server/users/frank.json":              user("frank"),
		"server/users/gina.json":               user("gina"),
		"server/users/carol/acl.json":          `{"read":{"actors":["carol","dave"],"groups":[]}}`,
		"server/users/carol/keys/default.json": `{"name":"default"}`,
		"orgs/acme/org.json":                   `{"name":"acme"}`,
		"orgs/acme/members.json":               `[{"user":{"username":"alice"}}]`,
		"orgs/acme/invitations.json":           `[{"id":"x","username":"frank"}]`,
		"orgs/acme/groups/devs.json":           `{"groupname":"devs","users":["gina"],"clients":[],"groups":[]}`,
		"orgs/acme/nodes/web01.json":           `{"name":"web01"}`,
		"orgs/acme/acls/nodes/web01.json":      `{"update":{"actors":[],"users":["carol","ghost"],"clients":[],"groups":[]}}`,
		"orgs/acme/cookbooks/app/1.0.0.json":   `{"all_files":[{"path":"a","checksum":"` + md5hex("acme file") + `"}]}`,
		"orgs/beta/org.json":                   `{"name":"beta"}`,
		"orgs/beta/members.json":               `[{"user":{"username":"bob"}}]`,
		"orgs/beta/cookbooks/lib/1.0.0.json":   `{"all_files":[{"path":"b","checksum":"` + md5hex("beta file") + `"}]}`,
	}, "acme file", "beta file")
}

func TestSelectPullsInTheUsersAnOrgNames(t *testing.T) {
	r := twoOrgs(t)
	s, err := Select(r, []string{"acme"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.All || !reflect.DeepEqual(s.Orgs, []string{"acme"}) {
		t.Errorf("orgs = %v (all %v)", s.Orgs, s.All)
	}
	if want := []string{"alice", "carol", "dave", "frank", "gina"}; !reflect.DeepEqual(s.Users, want) {
		t.Errorf("users = %v, want %v", s.Users, want)
	}
	for p, want := range map[string]bool{
		"server/users/carol.json":              true,
		"server/users/carol/keys/default.json": true,
		"server/users/bob.json":                false,
		"server/users/erin.json":               false,
		"orgs/acme/nodes/web01.json":           true,
		"orgs/beta/org.json":                   false,
		format.BlobPath(md5hex("acme file")):   true,
		format.BlobPath(md5hex("beta file")):   false,
		"host/users.json":                      false,
	} {
		if got := s.Includes(p); got != want {
			t.Errorf("Includes(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestSelectExcludeAndEverything(t *testing.T) {
	r := twoOrgs(t)
	s, err := Select(r, nil, []string{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Orgs, []string{"beta"}) || !reflect.DeepEqual(s.Users, []string{"bob"}) {
		t.Errorf("--exclude-org acme: orgs %v, users %v", s.Orgs, s.Users)
	}
	all, err := Select(r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !all.All || len(all.Users) != 7 || !all.Includes("host/users.json") || !all.Includes("server/users/erin.json") {
		t.Errorf("everything: all %v, users %v", all.All, all.Users)
	}
}

func TestSelectRefusesUnknownOrEmpty(t *testing.T) {
	r := twoOrgs(t)
	if _, err := Select(r, []string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown org: err = %v", err)
	}
	if _, err := Select(r, []string{"acme"}, []string{"acme"}); err == nil {
		t.Error("selecting nothing was accepted")
	}
}
