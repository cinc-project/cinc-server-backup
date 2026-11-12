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

// verify --server after a restore: exit 0 when the target holds everything,
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
	if code, out := run("restore", file, "--server", b.url, "--user", b.admin, "--key", keyB); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}
	code, out := run("verify", file, "--server", b.url, "--user", b.admin, "--key", keyB)
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
	if code, out := run("restore", clean, "--server", c.url, "--user", c.admin, "--key", keyC); code != 0 {
		t.Fatalf("restore = %d:\n%s", code, out)
	}
	if code, out := run("verify", clean, "--server", c.url, "--user", c.admin, "--key", keyC); code != 0 || !regexp.MustCompile(`dropped\s+0\s`).MatchString(out) {
		t.Fatalf("verify of a clean restore = %d, want 0:\n%s", code, out)
	}
}
