package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
