package compare

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cinc-project/cinc-server-backup/format"
)

const usagName = "0123456789abcdef0123456789abcdef"

// build writes a backup holding objs (path -> JSON) and blobs.
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

var base = map[string]string{
	"server/users/pivotal.json":              `{"username":"pivotal","key":"A"}`,
	"server/users/alice.json":                `{"username":"alice"}`,
	"orgs/acme/org.json":                     `{"name":"acme","guid":"aaa"}`,
	"orgs/acme/nodes/web01.json":             `{"name":"web01","normal":{"n":1000.0}}`,
	"orgs/acme/members.json":                 `[{"user":{"username":"alice"}},{"user":{"username":"bob"}}]`,
	"orgs/acme/groups/users.json":            `{"groupname":"users","actors":["alice"],"users":["alice","bob"],"clients":[],"groups":["` + usagName + `"]}`,
	"orgs/acme/groups/" + usagName + ".json": `{"groupname":"` + usagName + `","users":["alice"]}`,
	"orgs/acme/acls/nodes/web01.json":        `{"read":{"actors":["b","a"],"groups":["admins","` + usagName + `"]}}`,
	"orgs/acme/data_bags/secrets.json":       `{"db":"http://old-host/organizations/acme/data/secrets/db"}`,
	"orgs/acme/cookbooks/nginx/1.0.0.json":   `{"all_files":[{"path":"b","checksum":"x","url":"http://old/b"},{"path":"a","checksum":"y","url":"http://old/a"}]}`,
}

func with(over map[string]string, drop ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

func TestEquivalentBackupsHaveNoDifferences(t *testing.T) {
	a := build(t, base, "file")
	// Everything a target regenerates differs: org guid, file URLs and their
	// order, URL hosts in listings, per-user group names, member and actor
	// order. The superuser differs too and is skipped.
	b := build(t, with(map[string]string{
		"server/users/pivotal.json":            `{"username":"pivotal","key":"B"}`,
		"orgs/acme/org.json":                   `{"name":"acme","guid":"bbb"}`,
		"orgs/acme/members.json":               `[{"user":{"username":"bob"}},{"user":{"username":"alice"}}]`,
		"orgs/acme/groups/users.json":          `{"groupname":"users","actors":["bob"],"users":["bob","alice"],"clients":[],"groups":["fedcba9876543210fedcba9876543210"]}`,
		"orgs/acme/acls/nodes/web01.json":      `{"read":{"actors":["a","b"],"groups":["admins","fedcba9876543210fedcba9876543210"]}}`,
		"orgs/acme/data_bags/secrets.json":     `{"db":"http://new-host/organizations/acme/data/secrets/db"}`,
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"all_files":[{"path":"a","checksum":"y","url":"http://new/a"},{"path":"b","checksum":"x","url":"http://new/b"}]}`,
	}, "orgs/acme/groups/"+usagName+".json"), "file")

	diffs, err := Backups(a, b, Options{SkipUsers: []string{"pivotal"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("diffs = %+v", diffs)
	}
}

func TestChangedMissingAndBlobDifferencesAreReported(t *testing.T) {
	a := build(t, base, "file")
	b := build(t, with(map[string]string{
		"orgs/acme/nodes/web01.json": `{"name":"web01","normal":{"n":1000}}`,
	}, "server/users/alice.json"))
	diffs, err := Backups(a, b, Options{SkipUsers: []string{"pivotal"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range diffs {
		got[d.Path] = d.Kind
	}
	want := map[string]string{
		"orgs/acme/nodes/web01.json":    Changed,
		"server/users/alice.json":       Missing,
		format.BlobPath(md5hex("file")): Missing,
	}
	if len(got) != len(want) {
		t.Fatalf("diffs = %+v, want %v", diffs, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: kind %q, want %q (all: %+v)", p, got[p], k, diffs)
		}
	}
}

// Without SkipUsers the superuser's differences count like any other.
func TestSuperuserIsComparedUnlessSkipped(t *testing.T) {
	a := build(t, base)
	b := build(t, with(map[string]string{"server/users/pivotal.json": `{"username":"pivotal","key":"B"}`}))
	diffs, err := Backups(a, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "server/users/pivotal.json" {
		t.Fatalf("diffs = %+v", diffs)
	}
}

// A cookbook without "frozen?" is not frozen (erchef's default), and a
// policy group's uri is a URL the target regenerates, its name the path's.
func TestDefaultsAndDerivedFieldsAreSetAside(t *testing.T) {
	a := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[]}`,
		"orgs/acme/policy_groups/prod.json":    `{"name":"prod","uri":"","policies":{"web":{"revision_id":"1"}}}`,
	})
	b := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[],"frozen?":false}`,
		"orgs/acme/policy_groups/prod.json":    `{"uri":"http://new/organizations/acme/policy_groups/prod","policies":{"web":{"revision_id":"1"}}}`,
	})
	if diffs, err := Backups(a, b, Options{}); err != nil || len(diffs) != 0 {
		t.Fatalf("diffs = %+v, %v", diffs, err)
	}
	// A frozen cookbook restored unfrozen, or a group pointing elsewhere, is
	// still a difference.
	c := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[],"frozen?":true}`,
		"orgs/acme/policy_groups/prod.json":    `{"uri":"x","policies":{"web":{"revision_id":"2"}}}`,
	})
	if diffs, _ := Backups(a, c, Options{}); len(diffs) != 2 {
		t.Fatalf("diffs = %+v, want the frozen flag and the revision", diffs)
	}
}
