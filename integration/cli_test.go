package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cinc-project/cinc-server-backup/internal/cli"
)

// The command line end to end: back up a seeded server, then verify and
// inspect the file it wrote.
func TestCLIBackupVerifyInspect(t *testing.T) {
	s := startNG(t, "acme")
	seed(t, s)
	dir := t.TempDir()
	key := filepath.Join(dir, "pivotal.pem")
	if err := os.WriteFile(key, s.key, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "chef.csb")

	runCLI := func(args ...string) string {
		t.Helper()
		var o, e bytes.Buffer
		if code := cli.Run(args, &o, &e); code != 0 {
			t.Fatalf("%v = %d: %s", args, code, e.String())
		}
		return o.String()
	}
	runCLI("backup", "--server", s.url, "--user", s.admin, "--key", key, "-o", out)
	if got := runCLI("verify", out); !strings.HasPrefix(got, "ok:") {
		t.Fatalf("verify: %s", got)
	}
	report := runCLI("inspect", out)
	for _, want := range []string{"cookbook_versions", "nodes", "users", s.url} {
		if !strings.Contains(report, want) {
			t.Errorf("inspect lacks %q:\n%s", want, report)
		}
	}
}

// backup then restore through the command line, into an empty server.
func TestCLIRestoreIntoEmptyServer(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	b := startNG(t, "unused")
	dir := t.TempDir()
	keyA, keyB := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	_ = os.WriteFile(keyA, a.key, 0o600)
	_ = os.WriteFile(keyB, b.key, 0o600)
	file := filepath.Join(dir, "chef.csb")

	runCLI := func(args ...string) string {
		t.Helper()
		var o, e bytes.Buffer
		if code := cli.Run(args, &o, &e); code != 0 {
			t.Fatalf("%v = %d: %s", args, code, e.String())
		}
		return o.String()
	}
	runCLI("backup", "--server", a.url, "--user", a.admin, "--key", keyA, "-o", file)
	out := runCLI("restore", file, "--server", b.url, "--user", b.admin, "--key", keyB)
	if !strings.Contains(out, "nodes") {
		t.Errorf("restore report lacks counts:\n%s", out)
	}
	if _, _, err := b.client(t, "").Do(context.Background(), "GET", "/organizations/acme/nodes/web01", nil); err != nil {
		t.Fatalf("web01 missing on the target: %v", err)
	}
}

// restore, and verify --server after it: exit 0 when the target holds everything,
// 3 when it holds everything but the dangling names restore left out, and 1
// once it drifts.
func TestCLIVerifyServerFidelity(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) (int, string) {
		t.Helper()
		var o, e bytes.Buffer
		code := cli.Run(args, &o, &e)
		return code, o.String() + e.String()
	}
	keyOf := func(name string, s *ngServer) string {
		k := filepath.Join(dir, name+".pem")
		if err := os.WriteFile(k, s.key, 0o600); err != nil {
			t.Fatal(err)
		}
		return k
	}

	// The dangling backup restored with the default (drop).
	file := filepath.Join(dir, "dangling.csb")
	if err := os.WriteFile(file, danglingBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	b := startNG(t, "unused")
	keyB := keyOf("b", b)
	// Restore checks its own result: exit 3, everything but the dropped names.
	code, out := run("restore", file, "--server", b.url, "--user", b.admin, "--key", keyB)
	if code != 3 || !regexp.MustCompile(`fidelity:[\s\S]*dropped\s+3\s[\s\S]*differences\s+0`).MatchString(out) {
		t.Fatalf("restore = %d, want 3 with a fidelity report dropping 3 names:\n%s", code, out)
	}
	code, out = run("verify", file, "--server", b.url, "--user", b.admin, "--key", keyB)
	if code != 3 {
		t.Fatalf("verify = %d, want 3:\n%s", code, out)
	}
	for _, want := range []string{`preserved\s+\d+`, `nodes\s+1`, `dropped\s+3\s`, `web01.json\s+update: user ghost`,
		`web01.json\s+update: client gone01`, `devs.json\s+user ghost`, `differences\s+0`} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("report lacks %s:\n%s", want, out)
		}
	}

	// Drift: a node deleted on the target is a difference.
	if _, _, err := b.client(t, "").Do(context.Background(), "DELETE", "/organizations/acme/nodes/web01", nil); err != nil {
		t.Fatal(err)
	}
	code, out = run("verify", file, "--server", b.url, "--user", b.admin, "--key", keyB)
	if code != 1 || !strings.Contains(out, "missing  orgs/acme/nodes/web01.json") {
		t.Fatalf("verify after drift = %d, want 1 naming web01:\n%s", code, out)
	}

	// A clean backup and its restore: exit 0.
	a := startNG(t, "acme")
	seed(t, a)
	clean := filepath.Join(dir, "clean.csb")
	if code, out := run("backup", "--server", a.url, "--user", a.admin, "--key", keyOf("a", a), "-o", clean); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	c := startNG(t, "unused")
	keyC := keyOf("c", c)
	if code, out := run("restore", clean, "--server", c.url, "--user", c.admin, "--key", keyC); code != 0 ||
		!regexp.MustCompile(`fidelity:[\s\S]*differences\s+0`).MatchString(out) {
		t.Fatalf("restore = %d, want 0 with a fidelity report:\n%s", code, out)
	}
	if code, out := run("verify", clean, "--server", c.url, "--user", c.admin, "--key", keyC); code != 0 || !regexp.MustCompile(`dropped\s+0\s`).MatchString(out) {
		t.Fatalf("verify of a clean restore = %d, want 0:\n%s", code, out)
	}
}

// --no-verify leaves out the check after a restore, and with it exit 3.
func TestCLIRestoreNoVerify(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "dangling.csb")
	if err := os.WriteFile(file, danglingBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	b := startNG(t, "unused")
	key := filepath.Join(dir, "b.pem")
	if err := os.WriteFile(key, b.key, 0o600); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	code := cli.Run([]string{"restore", file, "--server", b.url, "--user", b.admin, "--key", key, "--no-verify"}, &o, &e)
	if code != 0 || strings.Contains(o.String(), "fidelity") {
		t.Fatalf("restore --no-verify = %d, want 0 and no fidelity report:\n%s%s", code, o.String(), e.String())
	}
}

// restore --org acme writes acme and the users it names, and nothing of
// beta; its own check compares only that part of the backup.
func TestCLIRestoreOneOrg(t *testing.T) {
	a := startNG(t, "acme", "beta")
	seed(t, a)
	ctx := context.Background()
	ca := a.client(t, "")
	for _, req := range [][2]string{
		{"/users", `{"username":"bob","email":"bob@example.com","first_name":"B","last_name":"O","display_name":"Bob","password":"sekrit123","create_key":true}`},
		{"/organizations/beta/users", `{"username":"bob"}`},
		{"/organizations/beta/nodes", `{"name":"db01","json_class":"Chef::Node","chef_type":"node","chef_environment":"_default","run_list":[],"normal":{},"automatic":{},"default":{},"override":{}}`},
	} {
		if _, _, err := ca.Do(ctx, "POST", req[0], []byte(req[1])); err != nil {
			t.Fatalf("POST %s: %v", req[0], err)
		}
	}
	dir := t.TempDir()
	keyA, keyB := filepath.Join(dir, "a.pem"), filepath.Join(dir, "b.pem")
	_ = os.WriteFile(keyA, a.key, 0o600)
	b := startNG(t, "unused")
	_ = os.WriteFile(keyB, b.key, 0o600)
	file := filepath.Join(dir, "chef.csb")
	run := func(args ...string) (int, string) {
		var o, e bytes.Buffer
		code := cli.Run(args, &o, &e)
		return code, o.String() + e.String()
	}
	if code, out := run("backup", "--server", a.url, "--user", a.admin, "--key", keyA, "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}

	code, out := run("restore", file, "--server", b.url, "--user", b.admin, "--key", keyB, "--org", "acme")
	if code != 0 || !strings.Contains(out, "selected orgs: acme") || !regexp.MustCompile(`differences\s+0`).MatchString(out) {
		t.Fatalf("restore --org acme = %d, want 0 with a clean report:\n%s", code, out)
	}
	cb := b.client(t, "")
	for p, want := range map[string]bool{
		"/organizations/acme/nodes/web01": true,
		"/users/alice":                    true,
		"/organizations/beta":             false,
		"/users/bob":                      false,
	} {
		_, _, err := cb.Do(ctx, "GET", p, nil)
		if got := err == nil; got != want {
			t.Errorf("GET %s on the target: exists = %v, want %v (err %v)", p, got, want, err)
		}
	}
	if code, out := run("verify", file, "--server", b.url, "--user", b.admin, "--key", keyB, "--exclude-org", "beta"); code != 0 {
		t.Errorf("verify --exclude-org beta = %d, want 0:\n%s", code, out)
	}
	// The whole backup does not match: beta and bob are missing.
	if code, _ := run("verify", file, "--server", b.url, "--user", b.admin, "--key", keyB); code != 1 {
		t.Errorf("verify of the whole backup = %d, want 1", code)
	}
}

// cliEnv runs the CLI against servers whose keys it writes to a temp dir.
type cliEnv struct {
	t   *testing.T
	dir string
}

func (c cliEnv) key(name string, s *ngServer) string {
	k := filepath.Join(c.dir, name+".pem")
	if err := os.WriteFile(k, s.key, 0o600); err != nil {
		c.t.Fatal(err)
	}
	return k
}

func (c cliEnv) run(args ...string) (int, string) {
	var o, e bytes.Buffer
	code := cli.Run(args, &o, &e)
	return code, o.String() + e.String()
}

// Two orgs that share a user restore one at a time: the second needs
// --merge skip, which keeps the shared user and adds the rest.
func TestCLIRestoreSecondOrgSharingAUser(t *testing.T) {
	a := startNG(t, "acme", "beta")
	seed(t, a) // alice, a member of acme
	ctx := context.Background()
	ca := a.client(t, "")
	for _, req := range [][2]string{
		{"/organizations/beta/users", `{"username":"alice"}`},
		{"/organizations/beta/nodes", `{"name":"db01","json_class":"Chef::Node","chef_type":"node","chef_environment":"_default","run_list":[],"normal":{},"automatic":{},"default":{},"override":{}}`},
	} {
		if _, _, err := ca.Do(ctx, "POST", req[0], []byte(req[1])); err != nil {
			t.Fatalf("POST %s: %v", req[0], err)
		}
	}
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "unused")
	keyB := env.key("b", b)
	target := []string{"--server", b.url, "--user", b.admin, "--key", keyB}

	if code, out := env.run(append([]string{"restore", file, "--org", "acme"}, target...)...); code != 0 {
		t.Fatalf("restore --org acme = %d:\n%s", code, out)
	}
	code, out := env.run(append([]string{"restore", file, "--org", "beta"}, target...)...)
	if code != 1 || !strings.Contains(out, "user alice") || !strings.Contains(out, "--merge skip") {
		t.Fatalf("restore --org beta without --merge = %d, want 1 naming alice and suggesting --merge skip:\n%s", code, out)
	}
	code, out = env.run(append([]string{"restore", file, "--org", "beta", "--merge", "skip"}, target...)...)
	if code != 0 || !regexp.MustCompile(`already on the target[\s\S]*users\s+1`).MatchString(out) {
		t.Fatalf("restore --org beta --merge skip = %d, want 0 with alice skipped:\n%s", code, out)
	}
	if _, _, err := b.client(t, "").Do(ctx, "GET", "/organizations/beta/nodes/db01", nil); err != nil {
		t.Errorf("beta's node missing: %v", err)
	}
	if code, out := env.run(append([]string{"verify", file}, target...)...); code != 0 {
		t.Errorf("verify of the whole backup = %d, want 0:\n%s", code, out)
	}
}

// A rerun with --merge skip puts back what is missing from the target and
// leaves alone what it holds, reporting an object it holds differently.
func TestCLIRestoreMergeSkipRerun(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "unused")
	target := []string{"--server", b.url, "--user", b.admin, "--key", env.key("b", b)}
	if code, out := env.run(append([]string{"restore", file}, target...)...); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}

	// Lose some of the restore, and change a role.
	ctx := context.Background()
	cb := b.client(t, "")
	for _, p := range []string{"/organizations/acme/nodes/web01", "/organizations/acme/data/secrets/db", "/organizations/acme/cookbooks/nginx/1.1.0"} {
		if _, _, err := cb.Do(ctx, "DELETE", p, nil); err != nil {
			t.Fatalf("DELETE %s: %v", p, err)
		}
	}
	changed := `{"name":"web","json_class":"Chef::Role","chef_type":"role","run_list":["recipe[apache]"]}`
	if _, _, err := cb.Do(ctx, "PUT", "/organizations/acme/roles/web", []byte(changed)); err != nil {
		t.Fatal(err)
	}

	code, out := env.run(append([]string{"restore", file, "--merge", "skip"}, target...)...)
	if code != 1 {
		t.Fatalf("rerun = %d, want 1 (the changed role):\n%s", code, out)
	}
	diffs := regexp.MustCompile(`(?m)^(missing|changed)\s+(\S+)`).FindAllStringSubmatch(out, -1)
	if len(diffs) != 1 || diffs[0][2] != "orgs/acme/roles/web.json" {
		t.Errorf("differences = %v, want only the changed role:\n%s", diffs, out)
	}
	for _, p := range []string{"/organizations/acme/nodes/web01", "/organizations/acme/data/secrets/db", "/organizations/acme/cookbooks/nginx/1.1.0"} {
		if _, _, err := cb.Do(ctx, "GET", p, nil); err != nil {
			t.Errorf("%s not put back: %v", p, err)
		}
	}
	data, _, err := cb.Do(ctx, "GET", "/organizations/acme/roles/web", nil)
	if err != nil || !strings.Contains(string(data), "apache") {
		t.Errorf("the target's role was not kept: %s %v", data, err)
	}
	if !regexp.MustCompile(`already on the target[\s\S]*roles\s+1`).MatchString(out) {
		t.Errorf("the role is not reported as skipped:\n%s", out)
	}
}

// A target user who shares a backup user's name but not its key is someone
// else: --merge skip gives it none of the backup's memberships or grants, says
// so, and the check reports what is missing.
func TestCLIMergeSkipWithholdsGrantsFromAStranger(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedExtras(t, a) // the user alice is in devs and web01's update ACL; a client alice is in its delete ACL
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}

	b := startNG(t, "unused")
	ctx := context.Background()
	cb := b.client(t, "")
	// A different alice, with her own key.
	if _, _, err := cb.Do(ctx, "POST", "/users", []byte(`{"username":"alice","email":"other@example.com","first_name":"O","last_name":"A","display_name":"Other Alice","password":"sekrit123","create_key":true}`)); err != nil {
		t.Fatal(err)
	}
	code, out := env.run("restore", file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b), "--merge", "skip")
	if code != 1 || !strings.Contains(out, "merge.different_user") {
		t.Fatalf("restore = %d, want 1 with a different_user warning:\n%s", code, out)
	}

	if _, _, err := cb.Do(ctx, "GET", "/organizations/acme/users/alice", nil); err == nil {
		t.Error("the other alice was made a member of acme")
	}
	data, _, err := cb.Do(ctx, "GET", "/organizations/acme/groups/devs", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"alice"`) && regexp.MustCompile(`"users":\[[^\]]*"alice"`).Match(data) {
		t.Errorf("the other alice was added to devs: %s", data)
	}
	data, _, err = cb.Do(ctx, "GET", "/organizations/acme/nodes/web01/_acl?detail=granular", nil)
	if err != nil {
		t.Fatal(err)
	}
	var acl map[string]struct {
		Users   []string `json:"users"`
		Clients []string `json:"clients"`
	}
	if err := json.Unmarshal(data, &acl); err != nil {
		t.Fatal(err)
	}
	for _, u := range acl["update"].Users {
		if u == "alice" {
			t.Errorf("the other alice was granted update on web01: %s", data)
		}
	}
	// The client alice is the backup's own, created by this run: it keeps its grant.
	if !strings.Contains(strings.Join(acl["delete"].Clients, ","), "alice") {
		t.Errorf("the client alice lost its delete grant: %s", data)
	}
}

// Public keys are public: a target user carrying the backup user's public key
// as its default, plus an extra key of its own to sign with, is still someone
// else. Only the backup's exact key set counts as the same actor.
func TestCLIMergeSkipMatchingOneKeyIsNotEnough(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedExtras(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	ctx := context.Background()
	data, _, err := a.client(t, "").Do(ctx, "GET", "/users/alice/keys/default", nil)
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(data, &k); err != nil {
		t.Fatal(err)
	}

	b := startNG(t, "unused")
	cb := b.client(t, "")
	body, _ := json.Marshal(map[string]string{
		"username": "alice", "email": "other@example.com", "first_name": "O", "last_name": "A",
		"display_name": "Other Alice", "password": "sekrit123", "public_key": k.PublicKey,
	})
	if _, _, err := cb.Do(ctx, "POST", "/users", body); err != nil {
		t.Fatal(err)
	}
	// Her own key, which she can sign with.
	if _, _, err := cb.Do(ctx, "POST", "/users/alice/keys", []byte(`{"name":"mine","create_key":true,"expiration_date":"infinity"}`)); err != nil {
		t.Fatal(err)
	}

	code, out := env.run("restore", file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b), "--merge", "skip")
	if code != 1 || !strings.Contains(out, "merge.different_user") {
		t.Fatalf("restore = %d, want 1 with a different_user warning:\n%s", code, out)
	}
	if _, _, err := cb.Do(ctx, "GET", "/organizations/acme/users/alice", nil); err == nil {
		t.Error("the other alice was made a member of acme")
	}
}
