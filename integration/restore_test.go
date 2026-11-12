package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/format"
	"github.com/cinc-project/cinc-server-backup/restore"
)

// seedPolicies adds a pushed policy revision (with its cookbook artifact) in
// group prod, and a second revision no group uses.
func seedPolicies(t *testing.T, s *ngServer) {
	t.Helper()
	ctx := context.Background()
	c := s.client(t, "acme")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "recipes"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"name":"app","version":"1.0.0"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "recipes", "default.rb"), []byte("log 'app'\n"), 0o644)
	cb, err := cinc.LocalCookbookFromDir(dir, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	const ident = "0123456789abcdef0123456789abcdef01234567"
	lock := func(rev string) []byte {
		return []byte(`{"revision_id":"` + rev + `","name":"web","run_list":["recipe[app::default]"],"named_run_lists":{},
			"cookbook_locks":{"app":{"version":"1.0.0","identifier":"` + ident + `","dotted_decimal_identifier":"1.2.3","cache_key":null,"source_options":{"path":"."}}},
			"default_attributes":{},"override_attributes":{},"solution_dependencies":{"Policyfile":[],"dependencies":{}}}`)
	}
	if _, _, err := c.Policies.PushRevision(ctx, lock(strings.Repeat("a", 40)), "prod", map[string]*cinc.LocalCookbook{"app": cb}); err != nil {
		t.Fatalf("PushRevision: %v", err)
	}
	if _, _, err := c.Do(ctx, "POST", "/organizations/acme/policies/web/revisions", lock(strings.Repeat("b", 40))); err != nil {
		t.Fatalf("create unused revision: %v", err)
	}
}

// seedKeys gives web01 a second key and an ACL that names alice, and
// returns web01's original private key.
func seedExtras(t *testing.T, s *ngServer) []byte {
	t.Helper()
	ctx := context.Background()
	c := s.client(t, "acme")
	body, _, err := c.Do(ctx, "POST", "/organizations/acme/clients", []byte(`{"name":"db01","create_key":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ChefKey struct {
			PrivateKey string `json:"private_key"`
		} `json:"chef_key"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ChefKey.PrivateKey == "" {
		t.Fatalf("db01 create response %s: %v", body, err)
	}
	pub, _, err := c.Do(ctx, "GET", "/organizations/acme/clients/web01/keys/default", nil)
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		PublicKey string `json:"public_key"`
	}
	_ = json.Unmarshal(pub, &k)
	extra, _ := json.Marshal(map[string]string{"name": "rotated", "public_key": k.PublicKey, "expiration_date": "2099-12-31T00:00:00Z"})
	if _, _, err := c.Do(ctx, "POST", "/organizations/acme/clients/db01/keys", extra); err != nil {
		t.Fatalf("add second key: %v", err)
	}
	if _, _, err := c.Do(ctx, "PUT", "/organizations/acme/groups/devs",
		[]byte(`{"groupname":"devs","actors":{"users":["alice"],"clients":["db01"],"groups":[]}}`)); err != nil {
		t.Fatalf("devs membership: %v", err)
	}
	// A client sharing the user alice's name, as Chef allows. The node ACL
	// grants update to the user and delete to the client, so restore must
	// keep the two apart (the granular users/clients form).
	if _, _, err := c.Do(ctx, "POST", "/organizations/acme/clients", []byte(`{"name":"alice","create_key":true}`)); err != nil {
		t.Fatalf("client alice: %v", err)
	}
	if _, _, err := c.Do(ctx, "PUT", "/organizations/acme/nodes/web01/_acl/update",
		[]byte(`{"update":{"actors":[],"users":["alice"],"clients":[],"groups":["admins","devs"]}}`)); err != nil {
		t.Fatalf("node ACL update: %v", err)
	}
	if _, _, err := c.Do(ctx, "PUT", "/organizations/acme/nodes/web01/_acl/delete",
		[]byte(`{"delete":{"actors":[],"users":[],"clients":["alice"],"groups":["admins"]}}`)); err != nil {
		t.Fatalf("node ACL delete: %v", err)
	}
	return []byte(created.ChefKey.PrivateKey)
}

func backupBytes(t *testing.T, s *ngServer) *format.Reader {
	t.Helper()
	return backup(t, s)
}

func restoreInto(t *testing.T, r *format.Reader, s *ngServer) *restore.Report {
	t.Helper()
	rep, err := restore.ToAPI(context.Background(), r, s.client(t, ""), restore.Options{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return rep
}

var usag = regexp.MustCompile(`^[0-9a-f]{32}$`)

// normalize decodes a stored object and drops what a target regenerates, so
// two backups of equivalent servers compare equal.
func normalize(t *testing.T, path string, data []byte) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	m, _ := v.(map[string]any)
	switch {
	case strings.HasSuffix(path, "/org.json"):
		delete(m, "guid")
	case strings.Contains(path, "/data_bags/") && strings.Count(path, "/") == 3:
		// A bag's item listing: names to URLs on the source server's host.
		var names []string
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		return names
	case strings.Contains(path, "/cookbooks/") || strings.Contains(path, "/cookbook_artifacts/"):
		for _, f := range asList(m["all_files"]) {
			delete(f.(map[string]any), "url")
		}
		sortBy(m["all_files"], "path")
	case strings.Contains(path, "/groups/"):
		m["groups"] = dropUSAGs(m["groups"])
		delete(m, "actors")
		for _, k := range []string{"users", "clients", "groups"} {
			m[k] = sortedStrings(m[k])
		}
	case strings.HasSuffix(path, "/members.json"), strings.HasSuffix(path, "/invitations.json"):
		var names []string
		for _, e := range asList(v) {
			b, _ := json.Marshal(e)
			names = append(names, string(b))
		}
		sort.Strings(names)
		return names
	}
	if strings.Contains(path, "/acls/") || strings.HasSuffix(path, "/acl.json") {
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
	return v
}

func asList(v any) []any { l, _ := v.([]any); return l }

func sortedStrings(v any) any {
	l := asList(v)
	if l == nil {
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
	var out []any
	for _, e := range asList(v) {
		if s, _ := e.(string); !usag.MatchString(s) {
			out = append(out, e)
		}
	}
	return out
}

func sortBy(v any, key string) {
	l := asList(v)
	sort.Slice(l, func(i, j int) bool {
		a, _ := l[i].(map[string]any)[key].(string)
		b, _ := l[j].(map[string]any)[key].(string)
		return a < b
	})
}

// The node ACL keeps the user alice and the client alice apart after restore.
func TestRestoreKeepsSameNamedUserAndClientApart(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedExtras(t, a)
	b := startNG(t, "unused")
	restoreInto(t, backupBytes(t, a), b)

	data, _, err := b.client(t, "").Do(context.Background(), "GET", "/organizations/acme/nodes/web01/_acl?detail=granular", nil)
	if err != nil {
		t.Fatal(err)
	}
	var acl map[string]struct {
		Users   []string `json:"users"`
		Clients []string `json:"clients"`
	}
	if err := json.Unmarshal(data, &acl); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	has := func(l []string) bool { return strings.Contains(","+strings.Join(l, ",")+",", ",alice,") }
	if !has(acl["update"].Users) || has(acl["update"].Clients) {
		t.Errorf("update = %+v; want the user alice only", acl["update"])
	}
	if !has(acl["delete"].Clients) || has(acl["delete"].Users) {
		t.Errorf("delete = %+v; want the client alice only", acl["delete"])
	}
}

// The restore round trip: back up A, restore into an empty B, back up B. Every
// object in A's backup must be in B's and equal once regenerated fields are
// set aside. The superuser is the target's own and is not restored.
func TestRestoreRoundTripsEveryObject(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedPolicies(t, a)
	seedExtras(t, a)
	ra := backupBytes(t, a)

	b := startNG(t, "unused")
	rep := restoreInto(t, ra, b)
	rb := backupBytes(t, b)

	skip := func(p string) bool {
		return strings.HasPrefix(p, "server/users/"+a.admin+"/") || p == "server/users/"+a.admin+".json" ||
			strings.HasPrefix(p, "orgs/acme/groups/") && usag.MatchString(strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	checked := 0
	for _, e := range ra.Manifest().Entries {
		if strings.HasPrefix(e.Path, "blobs/") || skip(e.Path) {
			continue
		}
		da, _ := ra.Object(e.Path)
		db, err := rb.Object(e.Path)
		if err != nil {
			t.Errorf("%s: missing after restore", e.Path)
			continue
		}
		if na, nb := normalize(t, e.Path, da), normalize(t, e.Path, db); !reflect.DeepEqual(na, nb) {
			t.Errorf("%s differs after restore:\n before %s\n  after %s", e.Path, da, db)
		}
		checked++
	}
	for _, e := range ra.Manifest().Entries {
		if strings.HasPrefix(e.Path, "blobs/") && !rb.HasBlob(filepath.Base(e.Path)) {
			t.Errorf("blob %s missing after restore", filepath.Base(e.Path))
		}
	}
	if checked < 40 {
		t.Fatalf("compared only %d objects; the fixture is not exercising the restore", checked)
	}
	if rep == nil {
		t.Fatal("no report")
	}
}

// A node's existing client.pem must keep working: restore sets the
// backed-up public key, so the original private key signs against B.
func TestRestoredClientKeepsItsKey(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	priv := seedExtras(t, a)
	ra := backupBytes(t, a)

	b := startNG(t, "unused")
	restoreInto(t, ra, b)

	key, err := cinc.ParseKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cinc.NewClient(cinc.Config{ServerURL: b.url, Org: "acme", ClientName: "db01", Key: key}, cinc.WithHTTPClient(b.hc))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Do(context.Background(), "GET", "/organizations/acme/clients/db01", nil); err != nil {
		t.Fatalf("db01's original key does not authenticate after restore: %v", err)
	}
}

// Restore refuses a target that already holds an org from the backup.
func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	ra := backupBytes(t, a)
	b := startNG(t, "acme")
	_, err := restore.ToAPI(context.Background(), ra, b.client(t, ""), restore.Options{})
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("err = %v, want a refusal naming org acme", err)
	}
}

// edit returns a copy of r with fn applied to every object's bytes.
func edit(t *testing.T, r *format.Reader, fn func(path string, data []byte) []byte) *format.Reader {
	t.Helper()
	var buf bytes.Buffer
	m := r.Manifest()
	w := format.NewWriter(&buf, m.Source)
	for _, warn := range m.Warnings {
		w.Warn(warn)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Path, "blobs/") {
			sum := filepath.Base(e.Path)
			rc, err := r.Blob(sum)
			if err != nil {
				t.Fatal(err)
			}
			err = w.PutBlob(sum, rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		data, err := r.Object(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		meta := format.Meta{Source: e.Source, APIVersion: e.APIVersion, FetchedAt: e.FetchedAt}
		if err := w.PutObject(e.Path, fn(e.Path, data), meta); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := format.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// withDangling backs up a seeded server, then makes web01's update ACL name
// the user ghost and the client gone01, and devs name the user ghost, as a
// backup of a server where they were deleted after being granted access.
func withDangling(t *testing.T) *format.Reader {
	t.Helper()
	a := startNG(t, "acme")
	seed(t, a)
	seedExtras(t, a)
	return edit(t, backupBytes(t, a), func(p string, data []byte) []byte {
		add := func(m map[string]any, key, name string) {
			l, _ := m[key].([]any)
			m[key] = append(l, name)
		}
		var m map[string]any
		switch p {
		case "orgs/acme/acls/nodes/web01.json":
			_ = json.Unmarshal(data, &m)
			ace := m["update"].(map[string]any)
			add(ace, "users", "ghost")
			add(ace, "clients", "gone01")
		case "orgs/acme/groups/devs.json":
			_ = json.Unmarshal(data, &m)
			add(m, "users", "ghost")
		default:
			return data
		}
		out, _ := json.Marshal(m)
		return out
	})
}

// By default restore leaves dangling names out, restores the rest of each
// ACL and group, and reports every name it dropped.
func TestRestoreDropsDanglingNames(t *testing.T) {
	b := startNG(t, "unused")
	rep := restoreInto(t, withDangling(t), b)

	var got []string
	for _, w := range rep.Warnings {
		if strings.Contains(w.Code, "dangling") {
			got = append(got, w.Code+" "+w.Path+" "+w.Detail)
		}
	}
	want := []string{
		"acl.dangling_dropped orgs/acme/acls/nodes/web01.json update: user ghost",
		"acl.dangling_dropped orgs/acme/acls/nodes/web01.json update: client gone01",
		"group.dangling_dropped orgs/acme/groups/devs.json user ghost",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dangling warnings:\n got %q\nwant %q", got, want)
	}

	c := b.client(t, "")
	ctx := context.Background()
	data, _, err := c.Do(ctx, "GET", "/organizations/acme/nodes/web01/_acl?detail=granular", nil)
	if err != nil {
		t.Fatal(err)
	}
	var acl map[string]map[string]any
	if err := json.Unmarshal(data, &acl); err != nil {
		t.Fatal(err)
	}
	upd := acl["update"]
	if u := sortedStrings(upd["users"]); !reflect.DeepEqual(u, []string{"alice"}) {
		t.Errorf("web01 update users = %v, want [alice]", u)
	}
	if g := sortedStrings(upd["groups"]); !reflect.DeepEqual(g, []string{"admins", "devs"}) {
		t.Errorf("web01 update groups = %v, want [admins devs]", g)
	}
	data, _, err = c.Do(ctx, "GET", "/organizations/acme/groups/devs", nil)
	if err != nil {
		t.Fatal(err)
	}
	var devs map[string]any
	_ = json.Unmarshal(data, &devs)
	if u := sortedStrings(devs["users"]); !reflect.DeepEqual(u, []string{"alice"}) {
		t.Errorf("devs users = %v, want [alice]", u)
	}
}

// --dangling=fail names every dangling reference and writes nothing.
func TestRestoreDanglingFailWritesNothing(t *testing.T) {
	b := startNG(t, "unused")
	_, err := restore.ToAPI(context.Background(), withDangling(t), b.client(t, ""), restore.Options{Dangling: restore.DanglingFail})
	if err == nil {
		t.Fatal("restore succeeded; want a refusal")
	}
	for _, s := range []string{"3 users, clients or groups", "nodes/web01.json: update: user ghost", "client gone01", "groups/devs.json: user ghost"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error does not mention %q:\n%v", s, err)
		}
	}
	for _, p := range []string{"/organizations/acme", "/users/alice"} {
		if _, _, err := b.client(t, "").Do(context.Background(), "GET", p, nil); !errors.Is(err, cinc.ErrNotFound) {
			t.Errorf("GET %s after a refused restore: err = %v, want not found", p, err)
		}
	}
}
