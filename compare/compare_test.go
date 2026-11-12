package compare

import (
	"bytes"
	"sort"
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

// A policy group's uri is a URL on the server it was read from.
func TestPolicyGroupURIIsSetAsideButFrozenIsNot(t *testing.T) {
	a := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[],"frozen?":false}`,
		"orgs/acme/policy_groups/prod.json":    `{"policies":{"web":{"revision_id":"1"}},"uri":"http://old/organizations/acme/policy_groups/prod"}`,
	})
	b := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[],"frozen?":false}`,
		"orgs/acme/policy_groups/prod.json":    `{"uri":"http://new/organizations/acme/policy_groups/prod","policies":{"web":{"revision_id":"1"}}}`,
	})
	if diffs, err := Backups(a, b, Options{}); err != nil || len(diffs) != 0 {
		t.Fatalf("diffs = %+v, %v", diffs, err)
	}
	// A frozen cookbook restored unfrozen, or a group pointing elsewhere, is
	// a difference; so is a missing "frozen?", which erchef always sends.
	c := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[],"frozen?":true}`,
		"orgs/acme/policy_groups/prod.json":    `{"uri":"x","policies":{"web":{"revision_id":"2"}}}`,
	})
	if diffs, _ := Backups(a, c, Options{}); len(diffs) != 2 {
		t.Fatalf("diffs = %+v, want the frozen flag and the revision", diffs)
	}
	d := build(t, map[string]string{
		"orgs/acme/cookbooks/nginx/1.0.0.json": `{"name":"nginx-1.0.0","all_files":[]}`,
		"orgs/acme/policy_groups/prod.json":    `{"uri":"y","policies":{"web":{"revision_id":"1"}}}`,
	})
	if diffs, _ := Backups(a, d, Options{}); len(diffs) != 1 || diffs[0].Path != "orgs/acme/cookbooks/nginx/1.0.0.json" {
		t.Fatalf("diffs = %+v, want the cookbook missing frozen?", diffs)
	}
}

// A name the first backup's ACL or group holds but the backup itself does
// not (a deleted user) is dropped, not a difference, when the second lacks
// it. Anything else missing from the same object is still a difference.
func TestDanglingNamesTheTargetLacksAreDropped(t *testing.T) {
	objs := with(map[string]string{
		"orgs/acme/acls/nodes/web01.json": `{"update":{"actors":[],"users":["alice","ghost"],"clients":["gone01"],"groups":["admins"]}}`,
		"orgs/acme/groups/admins.json":    `{"groupname":"admins","users":["alice","ghost"],"clients":[],"groups":[]}`,
	})
	a := build(t, objs)
	b := build(t, with(map[string]string{
		"orgs/acme/acls/nodes/web01.json": `{"update":{"actors":[],"users":["alice"],"clients":[],"groups":["admins"]}}`,
		"orgs/acme/groups/admins.json":    `{"groupname":"admins","users":["alice"],"clients":[],"groups":[]}`,
	}))
	res, err := Compare(a, b, Options{SkipUsers: []string{"pivotal"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Differences) != 0 {
		t.Fatalf("differences = %+v", res.Differences)
	}
	var got []string
	for _, d := range res.Dropped {
		got = append(got, d.Path+" "+d.Detail)
	}
	want := []string{
		"orgs/acme/acls/nodes/web01.json update: user ghost",
		"orgs/acme/acls/nodes/web01.json update: client gone01",
		"orgs/acme/groups/admins.json user ghost",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("dropped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The same backup compared with its own source: nothing was dropped.
	if res, _ := Compare(a, a, Options{}); len(res.Dropped) != 0 || len(res.Differences) != 0 {
		t.Errorf("against itself: dropped %+v, differences %+v", res.Dropped, res.Differences)
	}

	// The target also lost alice, whom the backup holds: a difference.
	c := build(t, with(map[string]string{
		"orgs/acme/acls/nodes/web01.json": `{"update":{"actors":[],"users":[],"clients":[],"groups":["admins"]}}`,
		"orgs/acme/groups/admins.json":    `{"groupname":"admins","users":["alice"],"clients":[],"groups":[]}`,
	}))
	res, _ = Compare(a, c, Options{SkipUsers: []string{"pivotal"}})
	if len(res.Differences) != 1 || res.Differences[0].Path != "orgs/acme/acls/nodes/web01.json" {
		t.Errorf("differences = %+v, want web01's ACL", res.Differences)
	}
}

func TestCompareCountsPreservedRegeneratedAndSkipped(t *testing.T) {
	a := build(t, base, "file")
	b := build(t, with(map[string]string{
		"orgs/acme/org.json": `{"name":"acme","guid":"bbb"}`,
	}), "file")
	res, err := Compare(a, b, Options{SkipUsers: []string{"pivotal"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Preserved["nodes"] != 1 || res.Preserved["blobs"] != 1 || res.Preserved["orgs"] != 1 {
		t.Errorf("preserved = %v", res.Preserved)
	}
	if res.Regenerated != 1 {
		t.Errorf("regenerated = %d, want 1 (the org guid)", res.Regenerated)
	}
	if res.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (pivotal and the per-user group)", res.Skipped)
	}
}

// With Extras, what only the second backup holds is a difference too, within
// Include; without it, it isn't.
func TestExtrasAreReportedWhenAsked(t *testing.T) {
	a := build(t, base)
	b := build(t, with(map[string]string{
		"orgs/acme/nodes/extra.json":              `{"name":"extra"}`,
		"orgs/other/nodes/x.json":                 `{"name":"x"}`,
		"server/users/stranger.json":              `{"username":"stranger"}`,
		"orgs/acme/groups/" + usagName + "1.json": `{}`,
	}))
	if diffs, _ := Backups(a, b, Options{SkipUsers: []string{"pivotal"}}); len(diffs) != 0 {
		t.Fatalf("without Extras: %+v", diffs)
	}
	res, err := Compare(a, b, Options{SkipUsers: []string{"pivotal"}, Extras: true,
		Include: func(p string) bool { return !strings.HasPrefix(p, "orgs/other/") }})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range res.Differences {
		got = append(got, d.Kind+" "+d.Path)
	}
	sort.Strings(got)
	want := []string{"extra orgs/acme/groups/" + usagName + "1.json", "extra orgs/acme/nodes/extra.json", "extra server/users/stranger.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("extras = %v, want %v", got, want)
	}
}
