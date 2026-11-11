package format

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// build writes a backup with w and returns its bytes.
func build(t *testing.T, fn func(w *Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, Source{URL: "https://chef.example.com", Flavor: "chef-server", Version: "15.10.127"})
	w.now = func() time.Time { return t0 }
	fn(w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func open(t *testing.T, b []byte) *Reader {
	t.Helper()
	r, err := NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

const exactDoc = `{"z":1,"a":1000.0,"big":9007199254740993}`

func TestObjectRoundTripsExactBytes(t *testing.T) {
	b := build(t, func(w *Writer) {
		if err := w.PutObject("orgs/acme/nodes/web01.json", []byte(exactDoc), Meta{Source: SourceAPI, APIVersion: "1"}); err != nil {
			t.Fatal(err)
		}
	})
	r := open(t, b)
	got, err := r.Object("orgs/acme/nodes/web01.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != exactDoc {
		t.Fatalf("got %s, want %s", got, exactDoc)
	}
	e, ok := r.Entry("orgs/acme/nodes/web01.json")
	if !ok || e.Source != SourceAPI || e.APIVersion != "1" || e.Size != int64(len(exactDoc)) || !e.FetchedAt.Equal(t0) {
		t.Fatalf("entry = %+v", e)
	}
}

func TestManifestRecordsSourceAndCounts(t *testing.T) {
	b := build(t, func(w *Writer) {
		_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{}`), Meta{Source: SourceAPI})
		_ = w.PutObject("orgs/acme/nodes/b.json", []byte(`{}`), Meta{Source: SourceAPI})
		_ = w.PutObject("server/users/alice.json", []byte(`{}`), Meta{Source: SourceAPI})
		w.Warn(Warning{Code: "acl.dangling_actor", Path: "orgs/acme/acls/nodes/a.json", Detail: "bob"})
	})
	m := open(t, b).Manifest()
	if m.Format != FormatName || m.Version != Version {
		t.Fatalf("format/version = %q/%d", m.Format, m.Version)
	}
	if m.Source.URL != "https://chef.example.com" || m.Source.Version != "15.10.127" {
		t.Fatalf("source = %+v", m.Source)
	}
	if m.Counts["nodes"] != 2 || m.Counts["users"] != 1 {
		t.Fatalf("counts = %v", m.Counts)
	}
	if len(m.Warnings) != 1 || m.Warnings[0].Code != "acl.dangling_actor" {
		t.Fatalf("warnings = %+v", m.Warnings)
	}
	if !m.Window.StartedAt.Equal(t0) || !m.Window.FinishedAt.Equal(t0) {
		t.Fatalf("window = %+v", m.Window)
	}
}

func TestBlobStoredOnceByChecksum(t *testing.T) {
	content := "package 'nginx'\n"
	sum := md5hex(content)
	b := build(t, func(w *Writer) {
		for range 2 {
			if err := w.PutBlob(sum, strings.NewReader(content)); err != nil {
				t.Fatal(err)
			}
		}
		if !w.HasBlob(sum) {
			t.Fatal("HasBlob = false after PutBlob")
		}
	})
	r := open(t, b)
	if n := r.Manifest().Counts["blobs"]; n != 1 {
		t.Fatalf("blob count = %d, want 1", n)
	}
	rc, err := r.Blob(sum)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != content {
		t.Fatalf("blob = %q", got)
	}
	if !r.HasBlob(sum) || r.HasBlob(md5hex("other")) {
		t.Fatal("Reader.HasBlob wrong")
	}
}

func TestPutBlobRejectsChecksumMismatch(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Source{})
	err := w.PutBlob(md5hex("expected"), strings.NewReader("actual"))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if w.HasBlob(md5hex("expected")) {
		t.Fatal("mismatched blob was recorded")
	}
}

func TestPutObjectRejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{
		"", "/abs.json", "../escape.json", "orgs/../../x.json", "orgs/a\\b.json",
		"manifest.json", "unknown/top.json", "orgs/acme/./nodes/x.json", "orgs//x.json",
	} {
		var buf bytes.Buffer
		w := NewWriter(&buf, Source{})
		if err := w.PutObject(p, []byte(`{}`), Meta{}); !errors.Is(err, ErrPath) {
			t.Errorf("PutObject(%q) err = %v, want ErrPath", p, err)
		}
	}
}

func TestPutObjectRejectsDuplicatePath(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Source{})
	if err := w.PutObject("orgs/acme/org.json", []byte(`{}`), Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := w.PutObject("orgs/acme/org.json", []byte(`{}`), Meta{}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestPathsListsObjectsUnderPrefixSorted(t *testing.T) {
	b := build(t, func(w *Writer) {
		_ = w.PutObject("orgs/acme/nodes/b.json", []byte(`{}`), Meta{})
		_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{}`), Meta{})
		_ = w.PutObject("orgs/acme/roles/r.json", []byte(`{}`), Meta{})
	})
	got := open(t, b).Paths("orgs/acme/nodes/")
	if strings.Join(got, ",") != "orgs/acme/nodes/a.json,orgs/acme/nodes/b.json" {
		t.Fatalf("Paths = %v", got)
	}
}

func TestVerifyPassesForIntactBackup(t *testing.T) {
	b := build(t, func(w *Writer) {
		_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{}`), Meta{})
		_ = w.PutBlob(md5hex("x"), strings.NewReader("x"))
	})
	if err := open(t, b).Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// rewrite copies a backup, letting edit replace any member's contents.
func rewrite(t *testing.T, b []byte, edit func(name string, data []byte) []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		data = edit(f.Name, data)
		if data == nil {
			continue
		}
		fw, _ := zw.Create(f.Name)
		_, _ = fw.Write(data)
	}
	_ = zw.Close()
	return out.Bytes()
}

func TestTamperedObjectFailsReadAndVerify(t *testing.T) {
	b := build(t, func(w *Writer) {
		_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{"a":1}`), Meta{})
	})
	bad := rewrite(t, b, func(name string, data []byte) []byte {
		if name == "orgs/acme/nodes/a.json" {
			return []byte(`{"a":2}`)
		}
		return data
	})
	r := open(t, bad)
	if _, err := r.Object("orgs/acme/nodes/a.json"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Object err = %v, want ErrChecksum", err)
	}
	if err := r.Verify(); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Verify err = %v, want ErrChecksum", err)
	}
}

func TestVerifyFindsMissingAndUnlistedMembers(t *testing.T) {
	b := build(t, func(w *Writer) {
		_ = w.PutObject("orgs/acme/nodes/a.json", []byte(`{}`), Meta{})
	})
	missing := rewrite(t, b, func(name string, data []byte) []byte {
		if name == "orgs/acme/nodes/a.json" {
			return nil
		}
		return data
	})
	if err := open(t, missing).Verify(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Verify(missing) = %v", err)
	}
	extra := appendMember(t, b, "orgs/acme/nodes/sneaky.json", []byte(`{}`))
	if err := open(t, extra).Verify(); err == nil || !strings.Contains(err.Error(), "not in manifest") {
		t.Fatalf("Verify(extra) = %v", err)
	}
}

func appendMember(t *testing.T, b []byte, name string, data []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if err := zw.Copy(f); err != nil {
			t.Fatal(err)
		}
	}
	fw, _ := zw.Create(name)
	_, _ = fw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func TestNewReaderRejectsUnknownFormatAndFutureVersion(t *testing.T) {
	b := build(t, func(w *Writer) {})
	for _, tc := range []struct{ old, new string }{
		{`"format": "cinc-server-backup"`, `"format": "something-else"`},
		{`"version": 1`, `"version": 99`},
	} {
		bad := rewrite(t, b, func(name string, data []byte) []byte {
			if name == manifestName {
				return bytes.Replace(data, []byte(tc.old), []byte(tc.new), 1)
			}
			return data
		})
		if _, err := NewReader(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", tc.new, err)
		}
	}
}

func TestNewReaderRejectsUnsafeMemberNames(t *testing.T) {
	b := build(t, func(w *Writer) {})
	bad := appendMember(t, b, "../evil.json", []byte(`{}`))
	if _, err := NewReader(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrPath) {
		t.Fatalf("err = %v, want ErrPath", err)
	}
}

func TestNewReaderRequiresManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_ = zw.Close()
	if _, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len())); !errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
}

func TestCountsEveryCollection(t *testing.T) {
	paths := map[string]string{
		"server/users/alice.json":                        "users",
		"server/users/alice/keys/default.json":           "user_keys",
		"orgs/acme/org.json":                             "orgs",
		"orgs/acme/members.json":                         "",
		"orgs/acme/acls/nodes/web01.json":                "acls",
		"orgs/acme/data_bags/secrets.json":               "data_bags",
		"orgs/acme/data_bags/secrets/db.json":            "data_bag_items",
		"orgs/acme/clients/web01.json":                   "clients",
		"orgs/acme/clients/web01/keys/default.json":      "client_keys",
		"orgs/acme/cookbooks/nginx/1.0.0.json":           "cookbook_versions",
		"orgs/acme/cookbook_artifacts/nginx/abc123.json": "cookbook_artifacts",
		"orgs/acme/policies/web/0123abcd.json":           "policy_revisions",
		"orgs/acme/policy_groups/prod.json":              "policy_groups",
		"orgs/acme/nodes/web01.json":                     "nodes",
		"host/users.json":                                "",
	}
	b := build(t, func(w *Writer) {
		for p := range paths {
			if err := w.PutObject(p, []byte(`{}`), Meta{}); err != nil {
				t.Fatal(err)
			}
		}
	})
	want := map[string]int{}
	for _, k := range paths {
		if k != "" {
			want[k]++
		}
	}
	got := open(t, b).Manifest().Counts
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("counts[%s] = %d, want %d", k, got[k], n)
		}
	}
}

func TestManifestRecordsToolAndUpdatedSource(t *testing.T) {
	b := build(t, func(w *Writer) {
		w.SetTool("cinc-server-backup test")
		w.SetSource(Source{URL: "https://other", Flavor: "cinc-server-ng", APIVersions: []int{0, 2}})
	})
	m := open(t, b).Manifest()
	if m.Tool != "cinc-server-backup test" || m.Source.Flavor != "cinc-server-ng" || len(m.Source.APIVersions) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestOpenReadsBackupFile(t *testing.T) {
	b := build(t, func(w *Writer) { _ = w.PutObject("orgs/acme/org.json", []byte(`{}`), Meta{}) })
	path := t.TempDir() + "/x.csb"
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r, f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, ok := r.Entry("orgs/acme/org.json"); !ok {
		t.Fatal("entry missing")
	}
	if _, _, err := Open(t.TempDir() + "/nope.csb"); err == nil {
		t.Fatal("Open of a missing file succeeded")
	}
	junk := t.TempDir() + "/junk.csb"
	_ = os.WriteFile(junk, []byte("not a zip"), 0o600)
	if _, _, err := Open(junk); !errors.Is(err, ErrFormat) {
		t.Fatalf("Open(junk) err = %v, want ErrFormat", err)
	}
}

func TestWriterRejectsWritesAfterClose(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Source{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if err := w.PutObject("orgs/acme/org.json", []byte(`{}`), Meta{}); err == nil {
		t.Fatal("PutObject after Close succeeded")
	}
	if err := w.PutBlob(md5hex("x"), strings.NewReader("x")); err == nil {
		t.Fatal("PutBlob after Close succeeded")
	}
}

func TestPutBlobRejectsMalformedChecksum(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, Source{})
	for _, s := range []string{"", "abc", strings.Repeat("z", 32)} {
		if err := w.PutBlob(s, strings.NewReader("x")); !errors.Is(err, ErrPath) {
			t.Errorf("PutBlob(%q) err = %v, want ErrPath", s, err)
		}
	}
}

func TestPutBlobAcceptsUppercaseChecksum(t *testing.T) {
	b := build(t, func(w *Writer) {
		if err := w.PutBlob(strings.ToUpper(md5hex("x")), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	})
	if !open(t, b).HasBlob(md5hex("x")) {
		t.Fatal("blob missing")
	}
}

func TestObjectAndBlobReportNotFound(t *testing.T) {
	r := open(t, build(t, func(w *Writer) {}))
	if _, err := r.Object("orgs/acme/org.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Object err = %v", err)
	}
	if _, err := r.Blob(md5hex("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Blob err = %v", err)
	}
	if r.HasBlob("a") {
		t.Fatal("HasBlob(short) = true")
	}
}

func TestNewReaderRejectsUnsafeManifestEntry(t *testing.T) {
	b := build(t, func(w *Writer) { _ = w.PutObject("orgs/acme/org.json", []byte(`{}`), Meta{}) })
	bad := rewrite(t, b, func(name string, data []byte) []byte {
		if name == manifestName {
			return bytes.Replace(data, []byte(`"path": "orgs/acme/org.json"`), []byte(`"path": "../x"`), 1)
		}
		return data
	})
	if _, err := NewReader(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrPath) {
		t.Fatalf("err = %v, want ErrPath", err)
	}
}

func TestNewReaderRejectsCorruptManifest(t *testing.T) {
	b := build(t, func(w *Writer) {})
	bad := rewrite(t, b, func(name string, data []byte) []byte {
		if name == manifestName {
			return []byte("{")
		}
		return data
	})
	if _, err := NewReader(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
}
