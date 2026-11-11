// Package capture takes a backup of a Chef Infra Server compatible server.
package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"sync"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/format"
)

// API versions the crawler signs with. Version 1 is the lowest that exposes
// every key through the keys API; cookbook and artifact manifests are taken
// at version 2, the all_files form that restore uploads.
const (
	apiVersion      = "1"
	manifestVersion = "2"
)

// Options tune a backup.
type Options struct {
	// Concurrency bounds the requests in flight. Zero means 8.
	Concurrency int
}

// FromAPI crawls the whole server through c and writes every object it finds
// to w, exactly as the server returned it. c must sign as a superuser; its
// Config.Org is not used. An object that disappears between being listed and
// being read is skipped with a warning; any other failure stops the backup.
func FromAPI(ctx context.Context, c *cinc.Client, w *format.Writer, opts Options) error {
	n := opts.Concurrency
	if n <= 0 {
		n = 8
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cr := &crawler{c: c, w: w, sem: make(chan struct{}, n), cancel: cancel}
	cr.spawn(ctx, cr.users)
	cr.spawn(ctx, cr.orgs)
	cr.wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

type crawler struct {
	c      *cinc.Client
	w      *format.Writer
	sem    chan struct{}
	wg     sync.WaitGroup
	cancel context.CancelCauseFunc
	blobs  sync.Map // md5 -> struct{}: claimed by one fetcher
}

// spawn runs fn in its own goroutine once a slot is free. The first error
// cancels the crawl.
func (cr *crawler) spawn(ctx context.Context, fn func(context.Context) error) {
	cr.wg.Add(1)
	go func() {
		defer cr.wg.Done()
		select {
		case cr.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		err := fn(ctx)
		<-cr.sem
		if err != nil {
			cr.cancel(err)
		}
	}()
}

var errVanished = errors.New("vanished")

// get fetches path at API version v. A 404 becomes errVanished.
func (cr *crawler) get(ctx context.Context, path, v string) ([]byte, error) {
	data, _, err := cr.c.Do(cinc.WithServerAPIVersion(ctx, v), "GET", path, nil)
	if errors.Is(err, cinc.ErrNotFound) {
		return nil, errVanished
	}
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return data, nil
}

// save fetches path and stores it at dst. An object that has gone is recorded
// as a warning and reported as (nil, nil).
func (cr *crawler) save(ctx context.Context, path, dst, v string) ([]byte, error) {
	data, err := cr.get(ctx, path, v)
	if errors.Is(err, errVanished) {
		cr.w.Warn(format.Warning{Code: "object.vanished", Path: dst, Detail: "GET " + path + " returned 404 after the object was listed"})
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := cr.w.PutObject(dst, data, format.Meta{Source: format.SourceAPI, APIVersion: v}); err != nil {
		return nil, err
	}
	return data, nil
}

// saveACL stores the granular ACL of the object at path.
func (cr *crawler) saveACL(ctx context.Context, path, dst string) error {
	_, err := cr.save(ctx, path+"/_acl?detail=granular", dst, apiVersion)
	return err
}

func esc(s string) string { return url.PathEscape(s) }

// names returns the sorted keys of a {"name": ...} listing.
func names(data []byte, what string) ([]string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode %s listing: %w", what, err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (cr *crawler) users(ctx context.Context) error {
	data, err := cr.get(ctx, "/users", apiVersion)
	if err != nil {
		return err
	}
	list, err := names(data, "users")
	if err != nil {
		return err
	}
	for _, u := range list {
		cr.spawn(ctx, func(ctx context.Context) error {
			base := "/users/" + esc(u)
			dst := "server/users/" + u
			if obj, err := cr.save(ctx, base, dst+".json", apiVersion); err != nil || obj == nil {
				return err
			}
			if err := cr.keys(ctx, base, dst); err != nil {
				return err
			}
			return cr.saveACL(ctx, base, dst+"/acl.json")
		})
	}
	return nil
}

// keys stores every key of the actor at base under dst/keys/.
func (cr *crawler) keys(ctx context.Context, base, dst string) error {
	data, err := cr.get(ctx, base+"/keys", apiVersion)
	if errors.Is(err, errVanished) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("decode keys of %s: %w", base, err)
	}
	for _, k := range list {
		if _, err := cr.save(ctx, base+"/keys/"+esc(k.Name), dst+"/keys/"+k.Name+".json", apiVersion); err != nil {
			return err
		}
	}
	return nil
}

func (cr *crawler) orgs(ctx context.Context) error {
	data, err := cr.get(ctx, "/organizations", apiVersion)
	if err != nil {
		return err
	}
	list, err := names(data, "organizations")
	if err != nil {
		return err
	}
	for _, o := range list {
		cr.spawn(ctx, func(ctx context.Context) error { return cr.org(ctx, o) })
	}
	return nil
}

// simple collections: list, then GET each object and its ACL.
var simpleCollections = []string{"clients", "groups", "containers", "nodes", "roles", "environments", "policy_groups"}

func (cr *crawler) org(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o)
	dst := "orgs/" + o + "/"
	if obj, err := cr.save(ctx, base, dst+"org.json", apiVersion); err != nil || obj == nil {
		return err
	}
	for _, f := range []struct{ path, dst string }{
		{base + "/users", dst + "members.json"},
		{base + "/association_requests", dst + "invitations.json"},
		{base + "/organizations/_acl?detail=granular", dst + "acl.json"},
	} {
		if _, err := cr.save(ctx, f.path, f.dst, apiVersion); err != nil {
			return err
		}
	}
	for _, coll := range simpleCollections {
		cr.spawn(ctx, func(ctx context.Context) error { return cr.collection(ctx, o, coll) })
	}
	cr.spawn(ctx, func(ctx context.Context) error { return cr.dataBags(ctx, o) })
	cr.spawn(ctx, func(ctx context.Context) error { return cr.cookbooks(ctx, o) })
	cr.spawn(ctx, func(ctx context.Context) error { return cr.artifacts(ctx, o) })
	cr.spawn(ctx, func(ctx context.Context) error { return cr.policies(ctx, o) })
	return nil
}

func (cr *crawler) collection(ctx context.Context, o, coll string) error {
	base := "/organizations/" + esc(o) + "/" + coll
	data, err := cr.get(ctx, base, apiVersion)
	if err != nil {
		return err
	}
	list, err := names(data, coll)
	if err != nil {
		return err
	}
	for _, n := range list {
		cr.spawn(ctx, func(ctx context.Context) error {
			path := base + "/" + esc(n)
			dst := "orgs/" + o + "/" + coll + "/" + n
			if obj, err := cr.save(ctx, path, dst+".json", apiVersion); err != nil || obj == nil {
				return err
			}
			if coll == "clients" {
				if err := cr.keys(ctx, path, dst); err != nil {
					return err
				}
			}
			return cr.saveACL(ctx, path, "orgs/"+o+"/acls/"+coll+"/"+n+".json")
		})
	}
	return nil
}

func (cr *crawler) dataBags(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/data"
	data, err := cr.get(ctx, base, apiVersion)
	if err != nil {
		return err
	}
	bags, err := names(data, "data bags")
	if err != nil {
		return err
	}
	for _, b := range bags {
		cr.spawn(ctx, func(ctx context.Context) error {
			path := base + "/" + esc(b)
			dst := "orgs/" + o + "/data_bags/" + b
			listing, err := cr.save(ctx, path, dst+".json", apiVersion)
			if err != nil || listing == nil {
				return err
			}
			if err := cr.saveACL(ctx, path, "orgs/"+o+"/acls/data/"+b+".json"); err != nil {
				return err
			}
			items, err := names(listing, "data bag "+b)
			if err != nil {
				return err
			}
			for _, it := range items {
				if _, err := cr.save(ctx, path+"/"+esc(it), dst+"/"+it+".json", apiVersion); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return nil
}

// versioned lists a cookbooks-shaped collection: {"name":{"versions":[{key:..}]}}.
func versioned(data []byte, key, what string) (map[string][]string, error) {
	var m map[string]struct {
		Versions []map[string]string `json:"versions"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode %s listing: %w", what, err)
	}
	out := map[string][]string{}
	for name, e := range m {
		for _, v := range e.Versions {
			out[name] = append(out[name], v[key])
		}
	}
	return out, nil
}

func (cr *crawler) cookbooks(ctx context.Context, o string) error {
	return cr.manifests(ctx, o, "cookbooks", "version", "?num_versions=all")
}

func (cr *crawler) artifacts(ctx context.Context, o string) error {
	return cr.manifests(ctx, o, "cookbook_artifacts", "identifier", "")
}

// manifests stores every version of every cookbook (or artifact) in coll,
// the per-name ACL, and every file the manifests reference.
func (cr *crawler) manifests(ctx context.Context, o, coll, key, query string) error {
	base := "/organizations/" + esc(o) + "/" + coll
	data, err := cr.get(ctx, base+query, apiVersion)
	if err != nil {
		return err
	}
	all, err := versioned(data, key, coll)
	if err != nil {
		return err
	}
	for name, versions := range all {
		cr.spawn(ctx, func(ctx context.Context) error {
			return cr.saveACL(ctx, base+"/"+esc(name), "orgs/"+o+"/acls/"+coll+"/"+name+".json")
		})
		for _, v := range versions {
			cr.spawn(ctx, func(ctx context.Context) error {
				m, err := cr.save(ctx, base+"/"+esc(name)+"/"+esc(v), "orgs/"+o+"/"+coll+"/"+name+"/"+v+".json", manifestVersion)
				if err != nil || m == nil {
					return err
				}
				return cr.files(ctx, m)
			})
		}
	}
	return nil
}

// files fetches every file a manifest references that no one has claimed yet.
func (cr *crawler) files(ctx context.Context, manifest []byte) error {
	refs, err := fileRefs(manifest)
	if err != nil {
		return err
	}
	for _, f := range refs {
		if _, claimed := cr.blobs.LoadOrStore(f.checksum, struct{}{}); claimed {
			continue
		}
		cr.spawn(ctx, func(ctx context.Context) error {
			return cr.c.Fetch(ctx, f.url, func(r io.Reader) error { return cr.w.PutBlob(f.checksum, r) })
		})
	}
	return nil
}

type fileRef struct{ checksum, url string }

// fileRefs finds every {"checksum": ..., "url": ...} object in a manifest,
// whichever API version's shape it has (all_files, or per-segment lists).
func fileRefs(manifest []byte) ([]fileRef, error) {
	var doc any
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var out []fileRef
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			sum, ok1 := t["checksum"].(string)
			u, ok2 := t["url"].(string)
			if ok1 && ok2 {
				out = append(out, fileRef{sum, u})
				return
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(doc)
	return out, nil
}

func (cr *crawler) policies(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/policies"
	data, err := cr.get(ctx, base, apiVersion)
	if err != nil {
		return err
	}
	var m map[string]struct {
		Revisions map[string]json.RawMessage `json:"revisions"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("decode policies listing: %w", err)
	}
	for name, p := range m {
		cr.spawn(ctx, func(ctx context.Context) error {
			return cr.saveACL(ctx, base+"/"+esc(name), "orgs/"+o+"/acls/policies/"+name+".json")
		})
		for rev := range p.Revisions {
			cr.spawn(ctx, func(ctx context.Context) error {
				_, err := cr.save(ctx, base+"/"+esc(name)+"/revisions/"+esc(rev), "orgs/"+o+"/policies/"+name+"/"+rev+".json", apiVersion)
				return err
			})
		}
	}
	return nil
}
