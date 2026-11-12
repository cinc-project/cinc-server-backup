package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	cinc "github.com/cinc-project/cinc-api"
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
	for _, mode := range []string{"skip", "overwrite"} {
		t.Run(mode, func(t *testing.T) { testWithholdsGrantsFromAStranger(t, mode) })
	}
}

func testWithholdsGrantsFromAStranger(t *testing.T, mode string) {
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
	code, out := env.run("restore", file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b), "--merge", mode)
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

// Matching keys aren't enough for a user who signs in as someone else: a
// different external login (SSO/LDAP uid) makes the target's alice a stranger,
// even with exactly the backup alice's keys.
func TestCLIMergeSkipDifferentExternalLogin(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
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

	newTarget := func(uid string) *ngServer {
		b := startNG(t, "unused")
		doc := map[string]string{
			"username": "alice", "email": "other@example.com", "first_name": "O", "last_name": "A",
			"display_name": "Other Alice", "password": "sekrit123", "public_key": k.PublicKey,
		}
		if uid != "" {
			doc["external_authentication_uid"] = uid
		}
		body, _ := json.Marshal(doc)
		if _, _, err := b.client(t, "").Do(ctx, "POST", "/users", body); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Exactly the backup's keys and no external login: the same alice. She joins
	// acme; her own user document is left as it is, and is the one difference.
	same := newTarget("")
	code, out := env.run("restore", file, "--server", same.url, "--user", same.admin, "--key", env.key("same", same), "--merge", "skip")
	diffs := regexp.MustCompile(`(?m)^(missing|changed)\s+(\S+)`).FindAllStringSubmatch(out, -1)
	if code != 1 || strings.Contains(out, "merge.different_user") || len(diffs) != 1 || diffs[0][2] != "server/users/alice.json" {
		t.Fatalf("restore with a matching alice = %d, want 1 with her user document the only difference and no warning:\n%s", code, out)
	}
	if _, _, err := same.client(t, "").Do(ctx, "GET", "/organizations/acme/users/alice", nil); err != nil {
		t.Errorf("the matching alice was not made a member of acme: %v", err)
	}

	// The same keys, but she signs in as someone else.
	other := newTarget("someone-else")
	code, out = env.run("restore", file, "--server", other.url, "--user", other.admin, "--key", env.key("other", other), "--merge", "skip")
	if code != 1 || !strings.Contains(out, "merge.different_user") {
		t.Fatalf("restore = %d, want 1 with a different_user warning:\n%s", code, out)
	}
	if _, _, err := other.client(t, "").Do(ctx, "GET", "/organizations/acme/users/alice", nil); err == nil {
		t.Error("the other alice was made a member of acme")
	}
}

// planOf runs restore --plan with args and returns the restore's counts from
// the JSON plan, the exit code and the output.
func (c cliEnv) planOf(args ...string) (map[string]map[string]int, int, string) {
	c.t.Helper()
	path := filepath.Join(c.dir, fmt.Sprintf("plan-%d.json", time.Now().UnixNano()))
	code, out := c.run(append(append([]string{"restore"}, args...), "--plan", "--plan-out", path)...)
	data, err := os.ReadFile(path)
	if err != nil {
		c.t.Fatalf("plan file: %v\n%s", err, out)
	}
	var p struct {
		Restore map[string]int `json:"restore"`
		Skip    map[string]int `json:"skip"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		c.t.Fatal(err)
	}
	return map[string]map[string]int{"restore": p.Restore, "skip": p.Skip}, code, out
}

// countsOf reads a restore report's counts, under the given heading, from its
// output.
func countsOf(t *testing.T, out, heading string) map[string]int {
	t.Helper()
	m := map[string]int{}
	i := strings.Index(out, heading+"\n")
	if i < 0 {
		return m
	}
	for _, line := range strings.Split(out[i+len(heading)+1:], "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(line, "  ") {
			break
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			break
		}
		m[f[0]] = n
	}
	return m
}

// The plan is the restore it describes: run against the same target, the real
// restore restores and skips exactly what the plan said, and the plan itself
// changes nothing. Checked for an empty target and for --merge skip into a
// target missing some of the backup.
func TestCLIPlanMatchesTheRestore(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedPolicies(t, a)
	seedExtras(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "unused")
	target := []string{file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b)}
	ctx := context.Background()
	cb := b.client(t, "")

	check := func(name string, extra ...string) {
		t.Helper()
		plan, code, out := env.planOf(append(append([]string{}, target...), extra...)...)
		if code != 0 {
			t.Fatalf("%s: plan = %d:\n%s", name, code, out)
		}
		code, out = env.run(append(append([]string{"restore"}, target...), extra...)...)
		if code != 0 {
			t.Fatalf("%s: restore = %d:\n%s", name, code, out)
		}
		restored, skipped := countsOf(t, out, "restored"), countsOf(t, out, "already on the target, left as it was")
		// Files: into an empty org the plan counts each once, while a real run
		// counts each time a sandbox asks for it, so concurrent versions sharing
		// one may count it twice. Into an org the target already has, the plan
		// can't see which the target holds, so it's an upper bound.
		if len(extra) == 0 && restored["files"] < plan["restore"]["files"] ||
			len(extra) > 0 && restored["files"] > plan["restore"]["files"] {
			t.Errorf("%s: restored %d files, planned %d", name, restored["files"], plan["restore"]["files"])
		}
		delete(restored, "files")
		delete(plan["restore"], "files")
		if !reflect.DeepEqual(restored, plan["restore"]) {
			t.Errorf("%s: restored %v\n planned %v", name, restored, plan["restore"])
		}
		if len(skipped) > 0 || len(plan["skip"]) > 0 {
			if !reflect.DeepEqual(skipped, plan["skip"]) {
				t.Errorf("%s: skipped %v\n planned %v", name, skipped, plan["skip"])
			}
		}
	}

	// The plan changes nothing.
	if _, code, out := env.planOf(target...); code != 0 {
		t.Fatalf("plan = %d:\n%s", code, out)
	}
	if _, _, err := cb.Do(ctx, "GET", "/organizations/acme", nil); err == nil {
		t.Fatal("the plan created org acme on the target")
	}

	check("empty target")

	// Lose part of it; plan and rerun with --merge skip.
	for _, p := range []string{"/organizations/acme/nodes/web01", "/organizations/acme/data/secrets/db", "/organizations/acme/cookbooks/nginx/1.1.0", "/organizations/acme/clients/db01"} {
		if _, _, err := cb.Do(ctx, "DELETE", p, nil); err != nil {
			t.Fatalf("DELETE %s: %v", p, err)
		}
	}
	check("--merge skip", "--merge", "skip")
}

// A plan lists every reason the restore would refuse to start, exits 1, and
// still shows what it would do were they allowed.
func TestCLIPlanReportsConflicts(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "acme")
	_, code, out := env.planOf(file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b))
	if code != 1 || !strings.Contains(out, "would refuse to start") || !strings.Contains(out, "organization acme") ||
		!strings.Contains(out, "plan: would restore") {
		t.Fatalf("plan into a target with acme = %d, want 1 naming the conflict after the plan:\n%s", code, out)
	}
}

// --dangling fail under --plan is a reason to refuse, listed with the plan,
// not an error before it.
func TestCLIPlanDanglingFail(t *testing.T) {
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "dangling.csb")
	if err := os.WriteFile(file, danglingBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	b := startNG(t, "unused")
	_, code, out := env.planOf(file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b), "--dangling", "fail")
	if code != 1 || !strings.Contains(out, "3 users, clients or groups it doesn't hold (--dangling fail)") ||
		!strings.Contains(out, "would leave out (3)") {
		t.Fatalf("plan --dangling fail = %d, want 1 listing the 3 names and the reason:\n%s", code, out)
	}
	if _, _, err := b.client(t, "").Do(context.Background(), "GET", "/organizations/acme", nil); err == nil {
		t.Fatal("the plan created org acme")
	}
}

// --merge overwrite makes a target that drifted from the backup match it again:
// changed objects are replaced, groups, ACLs and policy group assignments are
// set back, a frozen cookbook version is kept unless --force-frozen, and the
// plan shows exactly what the run then overwrites.
func TestCLIMergeOverwriteMakesTheTargetMatch(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedPolicies(t, a)
	seedExtras(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "unused")
	target := []string{file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b)}
	if code, out := env.run(append([]string{"restore"}, target...)...); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}

	// Drift.
	ctx := context.Background()
	cb := b.client(t, "")
	do := func(method, p, body string) []byte {
		t.Helper()
		data, _, err := cb.Do(ctx, method, p, []byte(body))
		if err != nil {
			t.Fatalf("%s %s: %v", method, p, err)
		}
		return data
	}
	do("PUT", "/organizations/acme/roles/web", `{"name":"web","json_class":"Chef::Role","chef_type":"role","run_list":["recipe[apache]"]}`)
	do("PUT", "/organizations/acme/nodes/web01", `{"name":"web01","json_class":"Chef::Node","chef_type":"node","chef_environment":"_default","run_list":[],"normal":{},"automatic":{},"default":{},"override":{}}`)
	do("PUT", "/organizations/acme/data/secrets/db", `{"id":"db","password":"changed"}`)
	do("PUT", "/organizations/acme/groups/devs", `{"groupname":"devs","actors":{"users":[],"clients":["db01"],"groups":[]}}`)
	do("PUT", "/organizations/acme/nodes/web01/_acl/update", `{"update":{"actors":[],"users":[],"clients":[],"groups":["admins"]}}`)
	do("PUT", "/organizations/acme/policy_groups/prod/policies/web", string(do("GET", "/organizations/acme/policies/web/revisions/"+strings.Repeat("b", 40), "")))
	user := map[string]any{}
	_ = json.Unmarshal(do("GET", "/users/alice", ""), &user)
	user["display_name"] = "Someone Else"
	ub, _ := json.Marshal(user)
	do("PUT", "/users/alice", string(ub))
	// nginx 1.0.0: changed and frozen.
	var cbk map[string]any
	_ = json.Unmarshal(do("GET", "/organizations/acme/cookbooks/nginx/1.0.0", ""), &cbk)
	md, _ := cbk["metadata"].(map[string]any)
	md["description"] = "changed on the target"
	cbk["frozen?"] = true
	cbb, _ := json.Marshal(cbk)
	do("PUT", "/organizations/acme/cookbooks/nginx/1.0.0?force=true", string(cbb))

	// Overwrite without --force-frozen: everything but the frozen cookbook.
	code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite")...)
	diffs := regexp.MustCompile(`(?m)^(missing|changed)\s+(\S+)`).FindAllStringSubmatch(out, -1)
	if code != 1 || !strings.Contains(out, "merge.frozen_kept") || len(diffs) != 1 || diffs[0][2] != "orgs/acme/cookbooks/nginx/1.0.0.json" {
		t.Fatalf("overwrite = %d, want 1 with the frozen cookbook kept as the only difference:\n%s", code, out)
	}
	got := countsOf(t, out, "overwritten to match the backup")
	for _, k := range []string{"roles", "nodes", "data_bag_items", "groups", "acls", "policy_groups", "users"} {
		if got[k] < 1 {
			t.Errorf("overwritten %v: no %s", got, k)
		}
	}
	if got["cookbook_versions"] != 0 {
		t.Errorf("the frozen cookbook was overwritten without --force-frozen: %v", got)
	}

	// The plan with --force-frozen: only that cookbook is left to overwrite.
	path := filepath.Join(env.dir, "overwrite-plan.json")
	if code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--force-frozen", "--plan", "--plan-out", path)...); code != 0 {
		t.Fatalf("plan = %d:\n%s", code, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Overwrite map[string]int `json:"overwrite"`
	}
	_ = json.Unmarshal(data, &plan)
	if !reflect.DeepEqual(plan.Overwrite, map[string]int{"cookbook_versions": 1}) {
		t.Errorf("planned overwrites = %v, want only the frozen cookbook", plan.Overwrite)
	}

	// --force-frozen replaces its content. No server ever unfreezes a version
	// (erchef keeps "frozen?" once set), so that is the one difference left,
	// and the run says why.
	code, out = env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--force-frozen")...)
	diffs = regexp.MustCompile(`(?m)^(missing|changed)\s+(\S+)`).FindAllStringSubmatch(out, -1)
	if code != 1 || !strings.Contains(out, "merge.stays_frozen") || len(diffs) != 1 || diffs[0][2] != "orgs/acme/cookbooks/nginx/1.0.0.json" {
		t.Fatalf("overwrite --force-frozen = %d, want 1 with stays_frozen and only the cookbook differing:\n%s", code, out)
	}
	if got := countsOf(t, out, "overwritten to match the backup"); !reflect.DeepEqual(got, plan.Overwrite) {
		t.Errorf("overwritten %v, planned %v", got, plan.Overwrite)
	}
	cbk = map[string]any{}
	_ = json.Unmarshal(do("GET", "/organizations/acme/cookbooks/nginx/1.0.0", ""), &cbk)
	md, _ = cbk["metadata"].(map[string]any)
	if md["description"] == "changed on the target" || cbk["frozen?"] != true {
		t.Errorf("after --force-frozen: description %v, frozen? %v; want the backup's content, still frozen", md["description"], cbk["frozen?"])
	}
}

// --prune deletes what the restored org on the target holds that the backup
// doesn't, exactly as the plan said, keeps every org's defaults, and keeps
// users when the target has orgs the backup doesn't (they may belong there).
func TestCLIPruneDeletesWhatTheBackupLacks(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	seedPolicies(t, a)
	seedExtras(t, a)
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if code, out := env.run("backup", "--server", a.url, "--user", a.admin, "--key", env.key("a", a), "-o", file); code != 0 {
		t.Fatalf("backup = %d:\n%s", code, out)
	}
	b := startNG(t, "unused")
	target := []string{file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b)}
	if code, out := env.run(append([]string{"restore"}, target...)...); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}

	// Everything the backup doesn't hold.
	ctx := context.Background()
	cb := b.client(t, "")
	post := func(p, body string) {
		t.Helper()
		if _, _, err := cb.Do(ctx, "POST", p, []byte(body)); err != nil {
			t.Fatalf("POST %s: %v", p, err)
		}
	}
	org := "/organizations/acme"
	post(org+"/nodes", `{"name":"extra01","json_class":"Chef::Node","chef_type":"node","chef_environment":"_default","run_list":[],"normal":{},"automatic":{},"default":{},"override":{}}`)
	post(org+"/roles", `{"name":"extra","json_class":"Chef::Role","chef_type":"role","run_list":[]}`)
	post(org+"/environments", `{"name":"staging","json_class":"Chef::Environment","chef_type":"environment"}`)
	post(org+"/data", `{"name":"extra"}`)
	post(org+"/data/secrets", `{"id":"extraitem"}`)
	post(org+"/clients", `{"name":"extra-client"}`)
	post(org+"/groups", `{"groupname":"extra-group"}`)
	post(org+"/containers", `{"containername":"extra-container","containerpath":"extra-container"}`)
	for _, u := range []string{"bob", "carol"} {
		post("/users", `{"username":"`+u+`","email":"`+u+`@example.com","first_name":"X","last_name":"Y","display_name":"`+u+`","password":"sekrit123"}`)
	}
	post(org+"/users", `{"username":"bob"}`)
	post(org+"/association_requests", `{"user":"carol"}`)
	rev := strings.Repeat("a", 40)
	lock, _, err := cb.Do(ctx, "GET", org+"/policies/web/revisions/"+rev, nil)
	if err != nil {
		t.Fatal(err)
	}
	extraRev := strings.Replace(string(lock), rev, strings.Repeat("c", 40), 1)
	post(org+"/policies/web/revisions", extraRev)
	if _, _, err := cb.Do(ctx, "PUT", org+"/policy_groups/staging/policies/web", lock); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "recipes"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"name":"nginx","version":"2.0.0"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "recipes", "default.rb"), []byte("log 'extra'\n"), 0o644)
	cbk, err := cinc.LocalCookbookFromDir(dir, "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.client(t, "acme").Cookbooks.Upload(ctx, cbk); err != nil {
		t.Fatal(err)
	}

	// The plan lists the deletions and changes nothing.
	path := filepath.Join(env.dir, "prune-plan.json")
	if code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--prune", "--plan", "--plan-out", path)...); code != 0 {
		t.Fatalf("plan = %d:\n%s", code, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Delete map[string]int `json:"delete"`
	}
	_ = json.Unmarshal(data, &plan)
	want := map[string]int{
		"nodes": 1, "roles": 1, "environments": 1, "data_bags": 1, "data_bag_items": 1, "clients": 1,
		"groups": 1, "containers": 1, "members": 1, "invitations": 1, "cookbook_versions": 1,
		"policy_revisions": 1, "policy_groups": 1,
	}
	if !reflect.DeepEqual(plan.Delete, want) {
		t.Fatalf("planned deletions = %v, want %v", plan.Delete, want)
	}

	// Without --yes it asks; with no one to answer, it changes nothing.
	if code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--prune")...); code != 1 ||
		!strings.Contains(out, "would delete 13 objects") || !strings.Contains(out, "nothing was changed") {
		t.Fatalf("prune without --yes = %d, want 1 listing 13 deletions and changing nothing:\n%s", code, out)
	}
	if _, _, err := cb.Do(ctx, "GET", org+"/nodes/extra01", nil); err != nil {
		t.Fatalf("a refused prune deleted extra01: %v", err)
	}

	code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--prune", "--yes")...)
	if code != 0 {
		t.Fatalf("prune = %d, want 0:\n%s", code, out)
	}
	if got := countsOf(t, out, "deleted (not in the backup)"); !reflect.DeepEqual(got, plan.Delete) {
		t.Errorf("deleted %v, planned %v", got, plan.Delete)
	}
	for _, p := range []string{org + "/nodes/extra01", org + "/roles/extra", org + "/environments/staging", org + "/data/extra",
		org + "/data/secrets/extraitem", org + "/clients/extra-client", org + "/groups/extra-group", org + "/containers/extra-container",
		org + "/users/bob", org + "/cookbooks/nginx/2.0.0", org + "/policy_groups/staging", org + "/policies/web/revisions/" + strings.Repeat("c", 40)} {
		if _, _, err := cb.Do(ctx, "GET", p, nil); !errors.Is(err, cinc.ErrNotFound) {
			t.Errorf("%s still there after the prune: %v", p, err)
		}
	}
	for _, p := range []string{org + "/environments/_default", org + "/clients/acme-validator", org + "/groups/admins",
		org + "/containers/nodes", "/users/bob", "/users/carol", "/organizations/unused"} {
		if _, _, err := cb.Do(ctx, "GET", p, nil); err != nil {
			t.Errorf("%s was pruned: %v", p, err)
		}
	}
}

// On a full restore into a target with no other orgs, --prune also deletes the
// users the backup doesn't hold, never the superuser it signs as, and the check
// then holds the target to the backup's users.
func TestCLIPruneDeletesUsersOnlyWhenItManagesEveryOrg(t *testing.T) {
	a := startNG(t, "acme")
	seed(t, a)
	// A backup without the source's superuser, so the target's superuser is a
	// user the backup doesn't hold: only the guard keeps it.
	data := editBytes(t, backup(t, a), func(p string, data []byte) []byte {
		if p == "server/users/"+a.admin+".json" || strings.HasPrefix(p, "server/users/"+a.admin+"/") {
			return nil
		}
		return data
	})
	env := cliEnv{t, t.TempDir()}
	file := filepath.Join(env.dir, "chef.csb")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b := startNG(t, "scratch")
	ctx := context.Background()
	cb := b.client(t, "")
	if _, _, err := cb.Do(ctx, "DELETE", "/organizations/scratch", nil); err != nil {
		t.Fatal(err) // a target with no orgs but the backup's
	}
	target := []string{file, "--server", b.url, "--user", b.admin, "--key", env.key("b", b)}
	if code, out := env.run(append([]string{"restore"}, target...)...); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}
	if _, _, err := cb.Do(ctx, "POST", "/users", []byte(`{"username":"zed","email":"zed@example.com","first_name":"Z","last_name":"Z","display_name":"Zed","password":"sekrit123"}`)); err != nil {
		t.Fatal(err)
	}
	code, out := env.run(append(append([]string{"restore"}, target...), "--merge", "overwrite", "--prune", "--yes")...)
	if code != 0 || !regexp.MustCompile(`deleted \(not in the backup\)\n\s+users\s+1`).MatchString(out) {
		t.Fatalf("prune = %d, want 0 deleting one user:\n%s", code, out)
	}
	if _, _, err := cb.Do(ctx, "GET", "/users/zed", nil); !errors.Is(err, cinc.ErrNotFound) {
		t.Errorf("zed survived: %v", err)
	}
	for _, u := range []string{"alice", b.admin} {
		if _, _, err := cb.Do(ctx, "GET", "/users/"+u, nil); err != nil {
			t.Errorf("%s was pruned: %v", u, err)
		}
	}
}
