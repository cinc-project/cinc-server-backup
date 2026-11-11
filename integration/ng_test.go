package integration

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	cinc "github.com/cinc-project/cinc-api"
	"github.com/cinc-project/cinc-server-ng/server"
)

// ngServer is an in-memory cinc-server-ng with auth and ACL enforcement on,
// as the standalone binary runs.
type ngServer struct {
	url   string
	admin string
	key   []byte
	hc    *http.Client
}

func startNG(t *testing.T, orgs ...string) *ngServer {
	t.Helper()
	srv, err := server.New(server.Options{Orgs: orgs, EnforceACL: true})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(func() {
		hc.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return &ngServer{url: srv.URL(), admin: srv.AdminName(), key: srv.AdminKey(), hc: hc}
}

// client returns a client signing as the bootstrap superuser. org may be
// empty for a client used only with absolute paths (Do).
func (s *ngServer) client(t *testing.T, org string) *cinc.Client {
	t.Helper()
	key, err := cinc.ParseKey(s.key)
	if err != nil {
		t.Fatal(err)
	}
	if org == "" {
		org = "_" // Config requires one; Do ignores it.
	}
	c, err := cinc.NewClient(cinc.Config{ServerURL: s.url, Org: org, ClientName: s.admin, Key: key}, cinc.WithHTTPClient(s.hc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
