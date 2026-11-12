package integration

import (
	"bytes"
	"context"
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
