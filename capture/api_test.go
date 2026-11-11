package capture

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/format"
)

// fakeServer answers GETs from a path -> (status, body) table; anything not
// listed is an empty listing, so a test only describes what it cares about.
func fakeServer(t *testing.T, routes map[string]route) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		rt, ok := routes[key]
		if !ok {
			rt, ok = routes[r.URL.Path]
		}
		if !ok {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if rt.status != 0 {
			w.WriteHeader(rt.status)
		}
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type route struct {
	status int
	body   string
}

func newClient(t *testing.T, url string) *cinc.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cinc.NewClient(cinc.Config{ServerURL: url, Org: "_", ClientName: "pivotal", Key: key}, cinc.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func run(t *testing.T, routes map[string]route) (*format.Reader, error) {
	t.Helper()
	srv := fakeServer(t, routes)
	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{URL: srv.URL})
	err := FromAPI(context.Background(), newClient(t, srv.URL), w, Options{})
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	r, rerr := format.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	return r, err
}

func TestObjectThatVanishesAfterListingIsAWarning(t *testing.T) {
	r, err := run(t, map[string]route{
		"/organizations":                      {body: `{"acme":"u"}`},
		"/organizations/acme/nodes":           {body: `{"gone":"u","here":"u"}`},
		"/organizations/acme/nodes/gone":      {status: 404, body: `{"error":["not found"]}`},
		"/organizations/acme/nodes/here":      {body: `{"name":"here"}`},
		"/organizations/acme/nodes/here/_acl": {body: `{}`},
	})
	if err != nil {
		t.Fatalf("FromAPI: %v", err)
	}
	if _, ok := r.Entry("orgs/acme/nodes/here.json"); !ok {
		t.Fatal("surviving node missing")
	}
	if _, ok := r.Entry("orgs/acme/nodes/gone.json"); ok {
		t.Fatal("vanished node stored")
	}
	ws := r.Manifest().Warnings
	if len(ws) != 1 || ws[0].Code != "object.vanished" || ws[0].Path != "orgs/acme/nodes/gone.json" {
		t.Fatalf("warnings = %+v", ws)
	}
}

func TestAnyOtherErrorStopsTheBackup(t *testing.T) {
	_, err := run(t, map[string]route{
		"/organizations":            {body: `{"acme":"u"}`},
		"/organizations/acme/roles": {status: 403, body: `{"error":["forbidden"]}`},
	})
	if err == nil || !strings.Contains(err.Error(), "/organizations/acme/roles") {
		t.Fatalf("err = %v, want failure naming the roles listing", err)
	}
}

func TestBlobWithWrongContentFailsTheBackup(t *testing.T) {
	var srvURL string
	routes := map[string]route{
		"/organizations": {body: `{"acme":"u"}`},
		"/organizations/acme/cookbooks?num_versions=all": {body: `{"nginx":{"versions":[{"version":"1.0.0"}]}}`},
		"/bookshelf/x": {body: "not the recipe"},
	}
	srv := fakeServer(t, routes)
	srvURL = srv.URL
	routes["/organizations/acme/cookbooks/nginx/1.0.0"] = route{body: `{"all_files":[{"name":"recipes/default.rb","checksum":"` +
		strings.Repeat("0", 32) + `","url":"` + srvURL + `/bookshelf/x"}]}`}

	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{})
	err := FromAPI(context.Background(), newClient(t, srv.URL), w, Options{})
	if !errors.Is(err, format.ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
}

const sumA, sumB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

func TestFileRefsReadsOnlyManifestFileLists(t *testing.T) {
	refs, err := fileRefs([]byte(`{
		"all_files":[{"name":"recipes/default.rb","checksum":"` + sumA + `","url":"u1"}],
		"recipes":[{"name":"default.rb","checksum":"` + sumB + `","url":"u2"}],
		"metadata":{"attributes":{"planted":{"checksum":"` + sumA + `","url":"http://169.254.169.254/"}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.checksum] = r.url
	}
	// A {"checksum","url"} object an uploader wrote into metadata is not a file.
	if len(refs) != 2 || got[sumA] != "u1" || got[sumB] != "u2" {
		t.Fatalf("refs = %+v", refs)
	}
	if _, err := fileRefs([]byte(`{`)); err == nil {
		t.Fatal("bad manifest accepted")
	}
}

func TestFileRefsRejectsMalformedChecksum(t *testing.T) {
	for _, sum := range []string{"", "abc", "../../etc/passwd", strings.Repeat("g", 32)} {
		if _, err := fileRefs([]byte(`{"all_files":[{"checksum":"` + sum + `","url":"u"}]}`)); err == nil {
			t.Errorf("checksum %q accepted", sum)
		}
	}
}

// cookbookServer serves one cookbook version whose file URL is fileURL
// (with SERVER replaced by the fake server's own URL).
func cookbookServer(t *testing.T, fileURL string) *httptest.Server {
	t.Helper()
	routes := map[string]route{
		"/organizations": {body: `{"acme":"u"}`},
		"/organizations/acme/cookbooks?num_versions=all": {body: `{"nginx":{"versions":[{"version":"1.0.0"}]}}`},
		"/bookshelf/x": {body: "x"},
	}
	srv := fakeServer(t, routes)
	routes["/organizations/acme/cookbooks/nginx/1.0.0"] = route{body: `{"all_files":[{"name":"recipes/default.rb","checksum":"` +
		md5hex("x") + `","url":"` + strings.ReplaceAll(fileURL, "SERVER", srv.URL) + `"}]}`}
	return srv
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestFileOnServerHostIsFetched(t *testing.T) {
	srv := cookbookServer(t, "SERVER/bookshelf/x")
	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{})
	if err := FromAPI(context.Background(), newClient(t, srv.URL), w, Options{}); err != nil {
		t.Fatal(err)
	}
	if !w.HasBlob(md5hex("x")) {
		t.Fatal("blob not stored")
	}
}

func TestFileOnAnotherHostIsRefusedUnlessAllowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(other.Close)
	otherHost := strings.TrimPrefix(other.URL, "http://")

	srv := cookbookServer(t, other.URL+"/bucket/x")
	var buf bytes.Buffer
	err := FromAPI(context.Background(), newClient(t, srv.URL), format.NewWriter(&buf, format.Source{}), Options{})
	if err == nil || !strings.Contains(err.Error(), otherHost) {
		t.Fatalf("err = %v, want a refusal naming %s", err, otherHost)
	}

	// An operator can allow an external bookshelf (S3) host.
	buf.Reset()
	w := format.NewWriter(&buf, format.Source{})
	if err := FromAPI(context.Background(), newClient(t, srv.URL), w, Options{BlobHosts: []string{otherHost}}); err != nil {
		t.Fatalf("with BlobHosts: %v", err)
	}
	if !w.HasBlob(md5hex("x")) {
		t.Fatal("blob from allowed host not stored")
	}
}

func TestFileURLWithOtherSchemeIsRefused(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "gopher://SERVER/x"} {
		srv := cookbookServer(t, u)
		var buf bytes.Buffer
		err := FromAPI(context.Background(), newClient(t, srv.URL), format.NewWriter(&buf, format.Source{}), Options{})
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: err = %v, want a refusal", u, err)
		}
	}
}
