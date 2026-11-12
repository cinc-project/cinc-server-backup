// Package restore writes a backup into a Chef Infra Server compatible server
// through its API.
package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/format"
)

// Options tune a restore.
type Options struct {
	// Concurrency bounds the requests in flight. Zero means 8.
	Concurrency int
	// Dangling says what to do with ACL entries and group members naming
	// what the backup does not hold. Empty means DanglingDrop.
	Dangling Dangling
}

// Report says what a restore did.
type Report struct {
	mu sync.Mutex
	// Restored counts what was written, by kind.
	Restored map[string]int
	// Warnings lists what was left out, and why.
	Warnings []format.Warning
}

func (r *Report) add(kind string) { r.mu.Lock(); r.Restored[kind]++; r.mu.Unlock() }

// usag matches the per-user groups a server creates when a user joins an
// org: they are named after the user's internal id, which a target assigns
// afresh, so they are recreated by association rather than restored.
var usag = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ToAPI restores everything in r into the server c talks to. c must sign as
// a superuser; that superuser itself is left as the target has it. The
// target must not already hold any of the backup's organizations or users.
//
// Objects are written in dependency order: users and their keys, orgs and
// membership, then per org containers, clients and keys, groups, objects,
// cookbooks and artifacts (files uploaded from the backup), policies, and
// ACLs last, once everything they name exists.
func ToAPI(ctx context.Context, r *format.Reader, c *cinc.Client, opts Options) (*Report, error) {
	n := opts.Concurrency
	if n <= 0 {
		n = 8
	}
	mode := opts.Dangling
	if mode == "" {
		mode = DanglingDrop
	}
	rs := &restorer{r: r, c: c, n: n, super: c.ClientName(), rep: &Report{Restored: map[string]int{}}}
	rs.refs = rs.known()
	orgs := rs.orgs()
	users := rs.users()
	if err := rs.preflight(ctx, orgs, users); err != nil {
		return nil, err
	}
	warns, err := rs.resolve(ctx, mode)
	if err != nil {
		return nil, err
	}
	rs.rep.Warnings = warns
	if err := rs.each(ctx, users, rs.user); err != nil {
		return rs.rep, err
	}
	if err := rs.each(ctx, orgs, rs.org); err != nil {
		return rs.rep, err
	}
	if err := rs.each(ctx, orgs, rs.orgContents); err != nil {
		return rs.rep, err
	}
	// User ACLs last: they may name other users.
	if err := rs.each(ctx, users, rs.userACL); err != nil {
		return rs.rep, err
	}
	sort.Slice(rs.rep.Warnings, func(i, j int) bool { return rs.rep.Warnings[i].Path < rs.rep.Warnings[j].Path })
	return rs.rep, nil
}

type restorer struct {
	r     *format.Reader
	c     *cinc.Client
	n     int
	super string
	rep   *Report
	refs  *known
}

// each runs fn for every item, at most n at a time, and returns the first
// error; the others are cancelled.
func (rs *restorer) each(ctx context.Context, items []string, fn func(context.Context, string) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, rs.n)
	var wg sync.WaitGroup
	for _, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, it); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

// do sends body to path at API version v and returns the response body.
func (rs *restorer) do(ctx context.Context, method, p string, body []byte, v string) ([]byte, error) {
	if v == "" {
		v = "1"
	}
	// cinc-api's errors already name the method and path.
	data, _, err := rs.c.Do(cinc.WithServerAPIVersion(ctx, v), method, p, body)
	return data, err
}

// object reads and decodes a backup object, keeping numbers exact.
func (rs *restorer) object(p string) (map[string]any, error) {
	data, err := rs.r.Object(p)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return m, nil
}

func esc(s string) string { return url.PathEscape(s) }

// names returns the last path element, without .json, of each backup path
// under dir that sits directly in it.
func (rs *restorer) names(dir string) []string {
	var out []string
	for _, p := range rs.r.Paths(dir) {
		rest := strings.TrimPrefix(p, dir)
		if !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".json") {
			out = append(out, strings.TrimSuffix(rest, ".json"))
		}
	}
	return out
}

func (rs *restorer) orgs() []string {
	var out []string
	for _, p := range rs.r.Paths("orgs/") {
		segs := strings.Split(p, "/")
		if len(segs) == 3 && segs[2] == "org.json" {
			out = append(out, segs[1])
		}
	}
	return out
}

func (rs *restorer) users() []string {
	var out []string
	for _, u := range rs.names("server/users/") {
		if u != rs.super {
			out = append(out, u)
		}
	}
	return out
}

// preflight refuses a target that already has any of the backup's orgs or
// users: restore creates, and does not merge.
func (rs *restorer) preflight(ctx context.Context, orgs, users []string) error {
	var clash []string
	check := func(p, what string) error {
		_, _, err := rs.c.Do(cinc.WithServerAPIVersion(ctx, "1"), "GET", p, nil)
		switch {
		case err == nil:
			clash = append(clash, what)
		case !errors.Is(err, cinc.ErrNotFound):
			return fmt.Errorf("GET %s: %w", p, err)
		}
		return nil
	}
	for _, o := range orgs {
		if err := check("/organizations/"+esc(o), "organization "+o); err != nil {
			return err
		}
	}
	for _, u := range users {
		if err := check("/users/"+esc(u), "user "+u); err != nil {
			return err
		}
	}
	if len(clash) > 0 {
		return fmt.Errorf("the target already has %s; restore needs a server without them", strings.Join(clash, ", "))
	}
	return nil
}

func randomPassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// defaultKey returns the public key of the actor's "default" key in the
// backup, if it has one.
func (rs *restorer) defaultKey(dir string) (string, error) {
	if _, ok := rs.r.Entry(dir + "/keys/default.json"); !ok {
		return "", nil
	}
	k, err := rs.object(dir + "/keys/default.json")
	if err != nil {
		return "", err
	}
	s, _ := k["public_key"].(string)
	return s, nil
}

// extraKeys adds every key but "default" of the actor at dir to base.
func (rs *restorer) extraKeys(ctx context.Context, dir, base, kind string) error {
	for _, name := range rs.names(dir + "/keys/") {
		if name == "default" {
			continue
		}
		k, err := rs.object(dir + "/keys/" + name + ".json")
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"name": k["name"], "public_key": k["public_key"], "expiration_date": k["expiration_date"]})
		if _, err := rs.do(ctx, "POST", base+"/keys", body, "1"); err != nil {
			return err
		}
		rs.rep.add(kind + "_keys")
	}
	return nil
}

func (rs *restorer) user(ctx context.Context, u string) error {
	dir := "server/users/" + u
	doc, err := rs.object(dir + ".json")
	if err != nil {
		return err
	}
	for _, k := range []string{"public_key", "chef_key", "create_key", "uri", "private_key"} {
		delete(doc, k)
	}
	pub, err := rs.defaultKey(dir)
	if err != nil {
		return err
	}
	if pub != "" {
		doc["public_key"] = pub
	}
	// The API stores only a hash of a plaintext password, so the real hash
	// cannot be restored here; a random one keeps the account locked until
	// the host tier or a reset sets it.
	doc["password"] = randomPassword()
	body, _ := json.Marshal(doc)
	if _, err := rs.do(ctx, "POST", "/users", body, "1"); err != nil {
		return err
	}
	rs.rep.add("users")
	return rs.extraKeys(ctx, dir, "/users/"+esc(u), "user")
}

func (rs *restorer) org(ctx context.Context, o string) error {
	doc, err := rs.object("orgs/" + o + "/org.json")
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": o, "full_name": doc["full_name"]})
	if _, err := rs.do(ctx, "POST", "/organizations", body, "1"); err != nil {
		return err
	}
	rs.rep.add("orgs")
	base := "/organizations/" + esc(o)
	if data, err := rs.r.Object("orgs/" + o + "/members.json"); err == nil {
		var members []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &members); err != nil {
			return fmt.Errorf("orgs/%s/members.json: %w", o, err)
		}
		for _, m := range members {
			b, _ := json.Marshal(map[string]string{"username": m.User.Username})
			if _, err := rs.do(ctx, "POST", base+"/users", b, "1"); err != nil {
				return err
			}
			rs.rep.add("members")
		}
	}
	if data, err := rs.r.Object("orgs/" + o + "/invitations.json"); err == nil {
		var invites []struct {
			Username string `json:"username"`
		}
		if err := json.Unmarshal(data, &invites); err != nil {
			return fmt.Errorf("orgs/%s/invitations.json: %w", o, err)
		}
		for _, inv := range invites {
			b, _ := json.Marshal(map[string]string{"user": inv.Username})
			if _, err := rs.do(ctx, "POST", base+"/association_requests", b, "1"); err != nil {
				return err
			}
			rs.rep.add("invitations")
		}
	}
	return nil
}

// existing lists the names in a target collection.
func (rs *restorer) existing(ctx context.Context, p string) (map[string]bool, error) {
	data, err := rs.do(ctx, "GET", p, nil, "1")
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out, nil
}

func (rs *restorer) orgContents(ctx context.Context, o string) error {
	for _, step := range []func(context.Context, string) error{
		rs.containers, rs.clients, rs.groups, rs.plainObjects, rs.dataBags,
		rs.cookbooks, rs.artifacts, rs.policies, rs.acls,
	} {
		if err := step(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

func (rs *restorer) containers(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/containers"
	have, err := rs.existing(ctx, base)
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range rs.names("orgs/" + o + "/containers/") {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return rs.each(ctx, missing, func(ctx context.Context, n string) error {
		b, _ := json.Marshal(map[string]string{"containername": n, "containerpath": n})
		if _, err := rs.do(ctx, "POST", base, b, "1"); err != nil {
			return err
		}
		rs.rep.add("containers")
		return nil
	})
}

func (rs *restorer) clients(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/clients"
	have, err := rs.existing(ctx, base)
	if err != nil {
		return err
	}
	return rs.each(ctx, rs.names("orgs/"+o+"/clients/"), func(ctx context.Context, name string) error {
		dir := "orgs/" + o + "/clients/" + name
		doc, err := rs.object(dir + ".json")
		if err != nil {
			return err
		}
		pub, err := rs.defaultKey(dir)
		if err != nil {
			return err
		}
		cbase := base + "/" + esc(name)
		if have[name] {
			// Created with the org (the validator): put back its key, so
			// existing validation.pem files keep working.
			if pub != "" {
				b, _ := json.Marshal(map[string]string{"name": "default", "public_key": pub, "expiration_date": "infinity"})
				if _, err := rs.do(ctx, "PUT", cbase+"/keys/default", b, "1"); err != nil {
					return err
				}
			}
		} else {
			body := map[string]any{"name": name}
			if v, ok := doc["validator"]; ok {
				body["validator"] = v
			}
			if pub != "" {
				body["public_key"] = pub
			}
			b, _ := json.Marshal(body)
			if _, err := rs.do(ctx, "POST", base, b, "1"); err != nil {
				return err
			}
		}
		rs.rep.add("clients")
		return rs.extraKeys(ctx, dir, cbase, "client")
	})
}

func stringsOf(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (rs *restorer) groups(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/groups"
	have, err := rs.existing(ctx, base)
	if err != nil {
		return err
	}
	var all []string
	for _, g := range rs.names("orgs/" + o + "/groups/") {
		if !usag.MatchString(g) {
			all = append(all, g)
		}
	}
	// Create every group first, so membership can name any of them.
	var missing []string
	for _, g := range all {
		if !have[g] {
			missing = append(missing, g)
		}
	}
	if err := rs.each(ctx, missing, func(ctx context.Context, g string) error {
		b, _ := json.Marshal(map[string]string{"groupname": g})
		_, err := rs.do(ctx, "POST", base, b, "1")
		return err
	}); err != nil {
		return err
	}
	return rs.each(ctx, all, func(ctx context.Context, g string) error {
		doc, err := rs.object("orgs/" + o + "/groups/" + g + ".json")
		if err != nil {
			return err
		}
		// Keep the per-user groups the target made when members joined; the
		// backup's are named after ids the target does not have.
		cur, err := rs.do(ctx, "GET", base+"/"+esc(g), nil, "1")
		if err != nil {
			return err
		}
		var target map[string]any
		_ = json.Unmarshal(cur, &target)
		// Dangling names were reported by resolve.
		users, clients, groups, _ := rs.refs.members(o, doc)
		for _, sub := range stringsOf(target["groups"]) {
			if usag.MatchString(sub) {
				groups = append(groups, sub)
			}
		}
		b, _ := json.Marshal(map[string]any{"groupname": g, "actors": map[string]any{"users": users, "clients": clients, "groups": groups}})
		if _, err := rs.do(ctx, "PUT", base+"/"+esc(g), b, "1"); err != nil {
			return err
		}
		rs.rep.add("groups")
		return nil
	})
}

// plainObjects posts environments, roles and nodes exactly as backed up.
func (rs *restorer) plainObjects(ctx context.Context, o string) error {
	for _, coll := range []string{"environments", "roles", "nodes"} {
		base := "/organizations/" + esc(o) + "/" + coll
		var items []string
		for _, n := range rs.names("orgs/" + o + "/" + coll + "/") {
			if coll == "environments" && n == "_default" {
				continue // every org has it, and it cannot be changed
			}
			items = append(items, n)
		}
		if err := rs.each(ctx, items, func(ctx context.Context, n string) error {
			p := "orgs/" + o + "/" + coll + "/" + n + ".json"
			data, err := rs.r.Object(p)
			if err != nil {
				return err
			}
			if _, err := rs.do(ctx, "POST", base, data, "1"); err != nil {
				return err
			}
			rs.rep.add(coll)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (rs *restorer) dataBags(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/data"
	return rs.each(ctx, rs.names("orgs/"+o+"/data_bags/"), func(ctx context.Context, bag string) error {
		b, _ := json.Marshal(map[string]string{"name": bag})
		if _, err := rs.do(ctx, "POST", base, b, "1"); err != nil {
			return err
		}
		rs.rep.add("data_bags")
		for _, item := range rs.names("orgs/" + o + "/data_bags/" + bag + "/") {
			data, err := rs.r.Object("orgs/" + o + "/data_bags/" + bag + "/" + item + ".json")
			if err != nil {
				return err
			}
			if _, err := rs.do(ctx, "POST", base+"/"+esc(bag), data, "1"); err != nil {
				return err
			}
			rs.rep.add("data_bag_items")
		}
		return nil
	})
}

// versions lists name/version pairs under orgs/<o>/<coll>/.
func (rs *restorer) versions(o, coll string) []string {
	prefix := "orgs/" + o + "/" + coll + "/"
	var out []string
	for _, p := range rs.r.Paths(prefix) {
		rest := strings.TrimSuffix(strings.TrimPrefix(p, prefix), ".json")
		if strings.Count(rest, "/") == 1 {
			out = append(out, rest)
		}
	}
	return out
}

func (rs *restorer) cookbooks(ctx context.Context, o string) error {
	return rs.manifests(ctx, o, "cookbooks", "cookbook_versions")
}

func (rs *restorer) artifacts(ctx context.Context, o string) error {
	return rs.manifests(ctx, o, "cookbook_artifacts", "cookbook_artifacts")
}

// manifests uploads each version's files through a sandbox, then PUTs its
// manifest as backed up, frozen state included.
func (rs *restorer) manifests(ctx context.Context, o, coll, kind string) error {
	org := "/organizations/" + esc(o)
	return rs.each(ctx, rs.versions(o, coll), func(ctx context.Context, nv string) error {
		p := "orgs/" + o + "/" + coll + "/" + nv + ".json"
		data, err := rs.r.Object(p)
		if err != nil {
			return err
		}
		files, err := format.ManifestFiles(data)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := rs.upload(ctx, org, files); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		body, err := withoutFileURLs(data)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		name, version := path.Split(nv)
		e, _ := rs.r.Entry(p)
		target := org + "/" + coll + "/" + esc(strings.TrimSuffix(name, "/")) + "/" + esc(version)
		if _, err := rs.do(ctx, "PUT", target, body, e.APIVersion); err != nil {
			return err
		}
		rs.rep.add(kind)
		return nil
	})
}

// upload puts files the org does not have yet into a sandbox and commits it.
func (rs *restorer) upload(ctx context.Context, org string, files []format.File) error {
	if len(files) == 0 {
		return nil
	}
	sums := map[string]any{}
	for _, f := range files {
		sums[f.Checksum] = nil
	}
	b, _ := json.Marshal(map[string]any{"checksums": sums})
	data, err := rs.do(ctx, "POST", org+"/sandboxes", b, "1")
	if err != nil {
		return err
	}
	var sb struct {
		ID        string `json:"sandbox_id"`
		Checksums map[string]struct {
			URL         string `json:"url"`
			NeedsUpload bool   `json:"needs_upload"`
		} `json:"checksums"`
	}
	if err := json.Unmarshal(data, &sb); err != nil {
		return fmt.Errorf("decode sandbox: %w", err)
	}
	for sum, cs := range sb.Checksums {
		if !cs.NeedsUpload {
			continue
		}
		e, ok := rs.r.Entry(format.BlobPath(sum))
		if !ok {
			return fmt.Errorf("the backup does not hold file %s", sum)
		}
		open := func() (io.ReadCloser, error) { return rs.r.Blob(sum) }
		if err := rs.c.Upload(ctx, cs.URL, sum, e.Size, open); err != nil {
			return err
		}
		rs.rep.add("files")
	}
	_, err = rs.do(ctx, "PUT", org+"/sandboxes/"+esc(sb.ID), []byte(`{"is_completed":true}`), "1")
	return err
}

// withoutFileURLs drops the server-generated url of each listed file, which a
// manifest PUT does not take.
func withoutFileURLs(manifest []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, err
	}
	changed := false
	for _, list := range []string{"all_files", "recipes", "definitions", "libraries", "attributes", "files", "templates", "resources", "providers", "root_files"} {
		raw, ok := m[list]
		if !ok {
			continue
		}
		var files []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			delete(f, "url")
		}
		m[list], _ = json.Marshal(files)
		changed = true
	}
	if !changed {
		return manifest, nil
	}
	return json.Marshal(m)
}

func (rs *restorer) policies(ctx context.Context, o string) error {
	org := "/organizations/" + esc(o)
	revs := rs.versions(o, "policies")
	if err := rs.each(ctx, revs, func(ctx context.Context, pr string) error {
		data, err := rs.r.Object("orgs/" + o + "/policies/" + pr + ".json")
		if err != nil {
			return err
		}
		name, _ := path.Split(pr)
		if _, err := rs.do(ctx, "POST", org+"/policies/"+esc(strings.TrimSuffix(name, "/"))+"/revisions", data, "1"); err != nil {
			return err
		}
		rs.rep.add("policy_revisions")
		return nil
	}); err != nil {
		return err
	}
	return rs.each(ctx, rs.names("orgs/"+o+"/policy_groups/"), func(ctx context.Context, g string) error {
		doc, err := rs.object("orgs/" + o + "/policy_groups/" + g + ".json")
		if err != nil {
			return err
		}
		pols, _ := doc["policies"].(map[string]any)
		for name, v := range pols {
			ref, _ := v.(map[string]any)
			rev, _ := ref["revision_id"].(string)
			data, err := rs.r.Object("orgs/" + o + "/policies/" + name + "/" + rev + ".json")
			if err != nil {
				return fmt.Errorf("policy group %s names %s revision %s: %w", g, name, rev, err)
			}
			if _, err := rs.do(ctx, "PUT", org+"/policy_groups/"+esc(g)+"/policies/"+esc(name), data, "1"); err != nil {
				return err
			}
		}
		rs.rep.add("policy_groups")
		return nil
	})
}

var perms = []string{"create", "read", "update", "delete", "grant"}

// putACL writes each permission of the ACL stored at p, for an object of org
// o ("" for a user's own ACL), to base/_acl/<perm>. Per-user groups and
// dangling names are left out; resolve has already reported them.
func (rs *restorer) putACL(ctx context.Context, p, base, o string) error {
	acl, err := rs.object(p)
	if err != nil {
		return err
	}
	for _, perm := range perms {
		ace, ok := acl[perm].(map[string]any)
		if !ok {
			continue
		}
		out, _, _ := rs.refs.ace(o, ace)
		b, _ := json.Marshal(map[string]any{perm: out})
		if _, err := rs.do(ctx, "PUT", base+"/_acl/"+perm, b, "1"); err != nil {
			return err
		}
	}
	rs.rep.add("acls")
	return nil
}

func (rs *restorer) acls(ctx context.Context, o string) error {
	org := "/organizations/" + esc(o)
	prefix := "orgs/" + o + "/acls/"
	if err := rs.each(ctx, rs.aclPaths(o), func(ctx context.Context, p string) error {
		typ, file := path.Split(strings.TrimPrefix(p, prefix))
		name := strings.TrimSuffix(file, ".json")
		return rs.putACL(ctx, p, org+"/"+strings.TrimSuffix(typ, "/")+"/"+esc(name), o)
	}); err != nil {
		return err
	}
	if _, ok := rs.r.Entry("orgs/" + o + "/acl.json"); ok {
		return rs.putACL(ctx, "orgs/"+o+"/acl.json", org+"/organizations", o)
	}
	return nil
}

func (rs *restorer) userACL(ctx context.Context, u string) error {
	p := "server/users/" + u + "/acl.json"
	if _, ok := rs.r.Entry(p); !ok {
		return nil
	}
	return rs.putACL(ctx, p, "/users/"+esc(u), "")
}
