package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cinc "github.com/cinc-project/cinc-api"
	"github.com/cinc-project/cinc-server-backup/capture"
	"github.com/cinc-project/cinc-server-backup/format"
)

// seed fills org acme on srv with one of everything the crawler must find.
func seed(t *testing.T, s *ngServer) {
	t.Helper()
	ctx := context.Background()
	c := s.client(t, "acme")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := func(method, path, body string) {
		t.Helper()
		_, _, err := c.Do(ctx, method, path, []byte(body))
		must(err)
	}
	raw("POST", "/users", `{"username":"alice","email":"alice@example.com","first_name":"A","last_name":"L","display_name":"Alice","password":"sekrit123","create_key":true}`)
	raw("POST", "/organizations/acme/users", `{"username":"alice"}`)
	raw("POST", "/organizations/acme/clients", `{"name":"web01","create_key":true}`)
	// Exact bytes the crawler must store unchanged.
	raw("POST", "/organizations/acme/nodes", nodeDoc)
	raw("POST", "/organizations/acme/roles", `{"name":"web","json_class":"Chef::Role","chef_type":"role","run_list":["recipe[nginx]"]}`)
	raw("POST", "/organizations/acme/environments", `{"name":"prod","json_class":"Chef::Environment","chef_type":"environment","cookbook_versions":{"nginx":"= 1.0.0"}}`)
	raw("POST", "/organizations/acme/data", `{"name":"secrets"}`)
	raw("POST", "/organizations/acme/data/secrets", `{"id":"db","password":"hunter2"}`)
	raw("POST", "/organizations/acme/groups", `{"groupname":"devs"}`)

	dir := t.TempDir()
	must(os.MkdirAll(filepath.Join(dir, "recipes"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "recipes", "default.rb"), []byte(recipe), 0o644))
	// Both versions share recipes/default.rb, so the backup stores it once.
	for _, v := range []string{"1.0.0", "1.1.0"} {
		must(os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"name":"nginx","version":"`+v+`"}`), 0o644))
		cb, err := cinc.LocalCookbookFromDir(dir, v)
		must(err)
		must(c.Cookbooks.Upload(ctx, cb))
	}
}

const (
	nodeDoc = `{"name":"web01","json_class":"Chef::Node","chef_type":"node","chef_environment":"prod","run_list":["role[web]"],"normal":{"big":9007199254740993,"ratio":1000.0},"automatic":{},"default":{},"override":{}}`
	recipe  = "package 'nginx'\n"
)

func backup(t *testing.T, s *ngServer) *format.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{URL: s.url})
	if err := capture.FromAPI(context.Background(), s.client(t, ""), w, capture.Options{}); err != nil {
		t.Fatalf("FromAPI: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := format.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return r
}

func TestBackupCapturesEveryObjectType(t *testing.T) {
	s := startNG(t, "acme")
	seed(t, s)
	r := backup(t, s)

	for _, p := range []string{
		"server/users/alice.json",
		"server/users/alice/keys/default.json",
		"server/users/alice/acl.json",
		"orgs/acme/org.json",
		"orgs/acme/members.json",
		"orgs/acme/invitations.json",
		"orgs/acme/acl.json",
		"orgs/acme/clients/web01.json",
		"orgs/acme/clients/web01/keys/default.json",
		"orgs/acme/clients/acme-validator.json",
		"orgs/acme/groups/devs.json",
		"orgs/acme/groups/admins.json",
		"orgs/acme/containers/nodes.json",
		"orgs/acme/nodes/web01.json",
		"orgs/acme/roles/web.json",
		"orgs/acme/environments/prod.json",
		"orgs/acme/environments/_default.json",
		"orgs/acme/data_bags/secrets.json",
		"orgs/acme/data_bags/secrets/db.json",
		"orgs/acme/cookbooks/nginx/1.0.0.json",
		"orgs/acme/cookbooks/nginx/1.1.0.json",
		"orgs/acme/acls/nodes/web01.json",
		"orgs/acme/acls/clients/web01.json",
		"orgs/acme/acls/groups/devs.json",
		"orgs/acme/acls/containers/nodes.json",
		"orgs/acme/acls/roles/web.json",
		"orgs/acme/acls/environments/prod.json",
		"orgs/acme/acls/data/secrets.json",
		"orgs/acme/acls/cookbooks/nginx.json",
	} {
		if _, ok := r.Entry(p); !ok {
			t.Errorf("missing %s", p)
		}
	}
}

func TestBackupStoresServerBytesUnchanged(t *testing.T) {
	s := startNG(t, "acme")
	seed(t, s)
	want, _, err := s.client(t, "").Do(context.Background(), "GET", "/organizations/acme/nodes/web01", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := backup(t, s)
	got, err := r.Object("orgs/acme/nodes/web01.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stored node differs from server response:\n got %s\nwant %s", got, want)
	}
	if !bytes.Contains(got, []byte("9007199254740993")) || !bytes.Contains(got, []byte("1000.0")) {
		t.Fatalf("number fidelity lost: %s", got)
	}
}

func TestBackupStoresEachCookbookFileOnce(t *testing.T) {
	s := startNG(t, "acme")
	seed(t, s)
	r := backup(t, s)

	sum := md5hex(recipe)
	rc, err := r.Blob(sum)
	if err != nil {
		t.Fatalf("recipe blob: %v", err)
	}
	defer rc.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if b.String() != recipe {
		t.Fatalf("blob = %q", b.String())
	}
	// One shared recipe plus a metadata.json per version.
	if n := r.Manifest().Counts["blobs"]; n != 3 {
		t.Fatalf("blobs = %d, want 3 (shared recipe + two metadata.json)", n)
	}
	if n := r.Manifest().Counts["cookbook_versions"]; n != 2 {
		t.Fatalf("cookbook_versions = %d, want 2", n)
	}
}

func TestBackupOfManyOrgsCountsMatchServer(t *testing.T) {
	s := startNG(t, "acme", "beta")
	seed(t, s)
	c := s.client(t, "beta")
	for _, n := range []string{"n1", "n2", "n3"} {
		if _, _, err := c.Do(context.Background(), "POST", "/organizations/beta/nodes",
			[]byte(`{"name":"`+n+`","json_class":"Chef::Node","chef_type":"node"}`)); err != nil {
			t.Fatal(err)
		}
	}
	r := backup(t, s)
	if n := len(r.Paths("orgs/beta/nodes/")); n != 3 {
		t.Fatalf("beta nodes = %d, want 3", n)
	}
	if n := r.Manifest().Counts["orgs"]; n != 2 {
		t.Fatalf("orgs = %d, want 2", n)
	}
}

func TestBackupMembersAreUsernames(t *testing.T) {
	s := startNG(t, "acme")
	seed(t, s)
	r := backup(t, s)
	b, err := r.Object("orgs/acme/members.json")
	if err != nil {
		t.Fatal(err)
	}
	var members []map[string]map[string]string
	if err := json.Unmarshal(b, &members); err != nil {
		t.Fatalf("members.json: %v (%s)", err, b)
	}
	var names []string
	for _, m := range members {
		names = append(names, m["user"]["username"])
	}
	if !strings.Contains(strings.Join(names, ","), "alice") {
		t.Fatalf("members = %v", names)
	}
}

func TestBackupCapturesPoliciesArtifactsAndEveryRevision(t *testing.T) {
	s := startNG(t, "acme")
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
	revInGroup := strings.Repeat("a", 40)
	revUnused := strings.Repeat("b", 40)
	if _, _, err := c.Policies.PushRevision(ctx, lock(revInGroup), "prod", map[string]*cinc.LocalCookbook{"app": cb}); err != nil {
		t.Fatalf("PushRevision: %v", err)
	}
	// A revision no group points at must still be captured.
	if _, _, err := c.Do(ctx, "POST", "/organizations/acme/policies/web/revisions", lock(revUnused)); err != nil {
		t.Fatalf("create unused revision: %v", err)
	}

	r := backup(t, s)
	for _, p := range []string{
		"orgs/acme/policies/web/" + revInGroup + ".json",
		"orgs/acme/policies/web/" + revUnused + ".json",
		"orgs/acme/policy_groups/prod.json",
		"orgs/acme/cookbook_artifacts/app/" + ident + ".json",
		"orgs/acme/acls/policies/web.json",
		"orgs/acme/acls/policy_groups/prod.json",
		"orgs/acme/acls/cookbook_artifacts/app.json",
	} {
		if _, ok := r.Entry(p); !ok {
			t.Errorf("missing %s", p)
		}
	}
	if !r.HasBlob(md5hex("log 'app'\n")) {
		t.Error("artifact recipe blob missing")
	}
}
