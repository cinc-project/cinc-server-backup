package cli

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinc-project/cinc-server-backup/format"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// writeBackup writes a small valid backup and returns its path.
func writeBackup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.csb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := format.NewWriter(f, format.Source{URL: "https://chef.example.com", Version: "15.10.127"})
	_ = w.PutObject("orgs/acme/org.json", []byte(`{"name":"acme"}`), format.Meta{Source: format.SourceAPI})
	_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{}`), format.Meta{Source: format.SourceAPI})
	_ = w.PutObject("orgs/acme/nodes/b.json", []byte(`{}`), format.Meta{Source: format.SourceAPI})
	w.Warn(format.Warning{Code: "object.vanished", Path: "orgs/acme/nodes/c.json"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return path
}

func TestNoArgsPrintsHelpAsUsageError(t *testing.T) {
	code, _, errOut := run()
	if code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Errorf("Run() = %d, stderr %q; want 2 and the help", code, errOut)
	}
}

func TestVerifyIntactBackup(t *testing.T) {
	code, out, errOut := run("verify", writeBackup(t))
	if code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("verify = %d, out %q, err %q", code, out, errOut)
	}
}

func TestVerifyTamperedBackupFails(t *testing.T) {
	path := writeBackup(t)
	// Replace one member's content, keeping the manifest's digest for it.
	data, _ := os.ReadFile(path)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if f.Name == "orgs/acme/nodes/a.json" {
			fw, _ := zw.Create(f.Name)
			_, _ = fw.Write([]byte(`{"tampered":true}`))
			continue
		}
		_ = zw.Copy(f)
	}
	_ = zw.Close()
	_ = os.WriteFile(path, buf.Bytes(), 0o600)
	code, _, errOut := run("verify", path)
	if code != 1 || errOut == "" {
		t.Fatalf("verify = %d, err %q; want 1 with a reason", code, errOut)
	}
}

func TestVerifyMissingFileFails(t *testing.T) {
	code, _, errOut := run("verify", filepath.Join(t.TempDir(), "nope.csb"))
	if code != 1 || errOut == "" {
		t.Fatalf("verify = %d, err %q", code, errOut)
	}
}

func TestInspectPrintsSourceCountsAndWarnings(t *testing.T) {
	code, out, _ := run("inspect", writeBackup(t))
	if code != 0 {
		t.Fatalf("inspect = %d", code)
	}
	for _, want := range []string{"https://chef.example.com", "15.10.127", "nodes", "2", "object.vanished", "orgs/acme/nodes/c.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output lacks %q:\n%s", want, out)
		}
	}
}

func writeKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pivotal.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// emptyServer answers every GET with an empty listing: a server with nothing
// on it, enough to drive a backup end to end.
func emptyServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBackupWritesOwnerOnlyFileAtomically(t *testing.T) {
	srv := emptyServer(t)
	out := filepath.Join(t.TempDir(), "chef.csb")
	code, _, errOut := run("backup", "--server", srv.URL, "--user", "pivotal", "--key", writeKey(t), "-o", out)
	if code != 0 {
		t.Fatalf("backup = %d: %s", code, errOut)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(out + ".partial"); !os.IsNotExist(err) {
		t.Fatalf(".partial left behind: %v", err)
	}
	if code, _, errOut := run("verify", out); code != 0 {
		t.Fatalf("verify of fresh backup = %d: %s", code, errOut)
	}
}

func TestFailedBackupLeavesNoOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	out := filepath.Join(t.TempDir(), "chef.csb")
	code, _, errOut := run("backup", "--server", srv.URL, "--user", "pivotal", "--key", writeKey(t), "-o", out)
	if code != 1 || !strings.Contains(errOut, "403") {
		t.Fatalf("backup = %d, err %q; want 1 naming the 403", code, errOut)
	}
	for _, p := range []string{out, out + ".partial"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed backup", p)
		}
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	srv := emptyServer(t)
	out := writeBackup(t)
	code, _, errOut := run("backup", "--server", srv.URL, "--user", "pivotal", "--key", writeKey(t), "-o", out)
	if code != 1 || !strings.Contains(errOut, "exists") {
		t.Fatalf("backup = %d, err %q", code, errOut)
	}
}

func TestBackupRequiresServerKeyAndOutput(t *testing.T) {
	code, _, errOut := run("backup", "--server", "https://x")
	if code != 2 || !strings.Contains(errOut, "--key") {
		t.Fatalf("backup = %d, err %q", code, errOut)
	}
}

// A server whose one cookbook file lives on a second host, as with an
// external (S3) bookshelf: backup refuses it unless --blob-host allows it.
func TestBackupBlobHostAllowsExternalBookshelf(t *testing.T) {
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(bucket.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/organizations":
			_, _ = w.Write([]byte(`{"acme":"u"}`))
		case "/organizations/acme/cookbooks":
			_, _ = w.Write([]byte(`{"nginx":{"versions":[{"version":"1.0.0"}]}}`))
		case "/organizations/acme/cookbooks/nginx/1.0.0":
			_, _ = w.Write([]byte(`{"all_files":[{"name":"recipes/default.rb","checksum":"9dd4e461268c8034f5c8564e155c67a6","url":"` + bucket.URL + `/x"}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(api.Close)
	key := writeKey(t)
	dir := t.TempDir()
	bucketHost := strings.TrimPrefix(bucket.URL, "http://")

	code, _, errOut := run("backup", "--server", api.URL, "--key", key, "-o", filepath.Join(dir, "a.csb"))
	if code != 1 || !strings.Contains(errOut, bucketHost) {
		t.Fatalf("without --blob-host: %d %q; want a refusal naming %s", code, errOut, bucketHost)
	}
	code, _, errOut = run("backup", "--server", api.URL, "--key", key, "--blob-host", bucketHost, "-o", filepath.Join(dir, "b.csb"))
	if code != 0 {
		t.Fatalf("with --blob-host: %d %q", code, errOut)
	}
}

// A file URL on the server itself that redirects to another host is held to
// the same --blob-host rule as a URL that names that host directly.
func TestBackupFollowsRedirectsOnlyToAllowedHosts(t *testing.T) {
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(bucket.Close)
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/organizations":
			_, _ = w.Write([]byte(`{"acme":"u"}`))
		case "/organizations/acme/cookbooks":
			_, _ = w.Write([]byte(`{"nginx":{"versions":[{"version":"1.0.0"}]}}`))
		case "/organizations/acme/cookbooks/nginx/1.0.0":
			_, _ = w.Write([]byte(`{"all_files":[{"name":"recipes/default.rb","checksum":"9dd4e461268c8034f5c8564e155c67a6","url":"` + api.URL + `/bookshelf/x"}]}`))
		case "/bookshelf/x":
			http.Redirect(w, r, bucket.URL+"/x", http.StatusFound)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(api.Close)
	key := writeKey(t)
	dir := t.TempDir()
	bucketHost := strings.TrimPrefix(bucket.URL, "http://")

	code, _, errOut := run("backup", "--server", api.URL, "--key", key, "-o", filepath.Join(dir, "a.csb"))
	if code != 1 || !strings.Contains(errOut, bucketHost) {
		t.Fatalf("without --blob-host: %d %q; want a refused redirect naming %s", code, errOut, bucketHost)
	}
	code, _, errOut = run("backup", "--server", api.URL, "--key", key, "--blob-host", bucketHost, "-o", filepath.Join(dir, "b.csb"))
	if code != 0 {
		t.Fatalf("with --blob-host: %d %q", code, errOut)
	}
}
