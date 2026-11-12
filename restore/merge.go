package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	cinc "github.com/cinc-project/cinc-api"
)

// Merge says what restore does with what the target already holds.
type Merge string

const (
	// MergeNone requires a target that holds none of the organizations or
	// users being restored.
	MergeNone Merge = ""
	// MergeSkip keeps what the target has and adds what is missing. An object
	// the target already holds (a user, a node, a cookbook version, ...) is
	// left as it is. Groups and ACLs are the exception: the backup's members
	// and entries the target's copy lacks are added to it, and none are
	// removed, so the result grants what either one granted.
	MergeSkip Merge = "skip"
)

// ParseMerge checks a --merge value.
func ParseMerge(s string) (Merge, error) {
	switch m := Merge(s); m {
	case MergeNone, MergeSkip:
		return m, nil
	}
	return "", fmt.Errorf("unknown merge mode %q (want skip)", s)
}

// tracker records what this run created, so restore can tell an object it
// wrote (whose ACL and membership it sets outright) from one the target
// already had (to which it only adds).
type tracker struct {
	mu    sync.Mutex
	orgs  map[string]bool // orgs this run created
	users map[string]bool // users this run created
	made  map[string]bool // "org\x00type\x00name" of objects this run created
	// strangers are actors the target already had under a backup actor's
	// name but with a different key: "users/<name>" or "<org>/clients/<name>".
	// They are someone else, so no grant the backup gives that name reaches
	// them.
	strangers map[string]bool
}

func newTracker() *tracker {
	return &tracker{orgs: map[string]bool{}, users: map[string]bool{}, made: map[string]bool{}, strangers: map[string]bool{}}
}

func (t *tracker) madeOrg(o string)  { t.mu.Lock(); t.orgs[o] = true; t.mu.Unlock() }
func (t *tracker) madeUser(u string) { t.mu.Lock(); t.users[u] = true; t.mu.Unlock() }

// madeObject records that this run created the object an ACL of type typ
// ("nodes", "data", "cookbooks", ...) and name governs.
func (t *tracker) madeObject(o, typ, name string) {
	t.mu.Lock()
	t.made[o+"\x00"+typ+"\x00"+name] = true
	t.mu.Unlock()
}

func (t *tracker) newOrg(o string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.orgs[o]
}

func (t *tracker) newUser(u string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.users[u]
}

// created reports whether this run created the object: everything in an org
// it created (the defaults the org came with included), or an object it
// recorded with madeObject.
func (t *tracker) created(o, typ, name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.orgs[o] || t.made[o+"\x00"+typ+"\x00"+name]
}

func (t *tracker) stranger(id string) { t.mu.Lock(); t.strangers[id] = true; t.mu.Unlock() }

func (t *tracker) isStranger(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.strangers[id]
}

// sameActor reports whether the target's actor at base (an existing user or
// client restore is skipping) holds the default public key the backup's actor
// at dir has. Without a key on either side to compare, it is not the same.
func (rs *restorer) sameActor(ctx context.Context, base, dir string) (bool, error) {
	want, err := rs.defaultKey(dir)
	if err != nil {
		return false, err
	}
	data, err := rs.do(ctx, "GET", base+"/keys/default", nil, "1")
	if errors.Is(err, cinc.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var k struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(data, &k); err != nil {
		return false, fmt.Errorf("decode %s/keys/default: %w", base, err)
	}
	return want != "" && squash(want) == squash(k.PublicKey), nil
}

// squash drops the whitespace a PEM may be wrapped or indented with.
func squash(s string) string { return strings.Join(strings.Fields(s), "") }

// withoutStrangers drops from a member list (kind "users" or "clients") the
// names that belong to someone else on the target.
func (rs *restorer) withoutStrangers(o, kind string, l []string) []string {
	out := make([]string, 0, len(l))
	for _, n := range l {
		id := "users/" + n
		if kind == "clients" {
			id = o + "/clients/" + n
		}
		if !rs.track.isStranger(id) {
			out = append(out, n)
		}
	}
	return out
}

// strangersOut applies withoutStrangers to an ACE as refs.ACE resolved it.
// An untyped actor is dropped if it names a stranger of either kind.
// Only lists the entry has are touched, so an untyped entry stays untyped.
func (rs *restorer) strangersOut(o string, ace map[string]any) map[string]any {
	for _, kind := range []string{"users", "clients"} {
		if v, ok := ace[kind]; ok {
			ace[kind] = rs.withoutStrangers(o, kind, names(v))
		}
	}
	actors := rs.withoutStrangers(o, "users", names(ace["actors"]))
	if o != "" {
		actors = rs.withoutStrangers(o, "clients", actors)
	}
	ace["actors"] = actors
	return ace
}

func (r *Report) skip(kind string) { r.mu.Lock(); r.Skipped[kind]++; r.mu.Unlock() }

// write sends a create. With MergeSkip, a 409 (the target already has it) is
// counted as skipped instead of failing; created says which happened.
func (rs *restorer) write(ctx context.Context, kind, method, p string, body []byte) (created bool, err error) {
	_, err = rs.do(ctx, method, p, body, "1")
	if err != nil && rs.merge == MergeSkip && errors.Is(err, cinc.ErrConflict) {
		rs.rep.skip(kind)
		return false, nil
	}
	return err == nil, err
}

// union returns a followed by the names of b it lacks, and whether any were
// added.
func union(a, b []string) ([]string, bool) {
	have := map[string]bool{}
	out := append([]string{}, a...)
	for _, n := range a {
		have[n] = true
	}
	added := false
	for _, n := range b {
		if !have[n] {
			have[n] = true
			out = append(out, n)
			added = true
		}
	}
	return out, added
}

// mergeACE adds the backup's entries (out, as refs.ACE resolved them) to the
// target's current entry for the same permission, in the granular form. An
// untyped backup actor is typed by what the backup holds: a user if it is one
// (a server checks users first), otherwise a client. changed is false when the
// target already grants everything the backup does.
func (rs *restorer) mergeACE(o string, target, out map[string]any) (merged map[string]any, changed bool) {
	users, clients := stringsOf(target["users"]), stringsOf(target["clients"])
	addUsers, addClients := names(out["users"]), names(out["clients"])
	for _, a := range names(out["actors"]) {
		if rs.refs.IsUser(a) || o == "" {
			addUsers = append(addUsers, a)
		} else {
			addClients = append(addClients, a)
		}
	}
	users, cu := union(users, addUsers)
	clients, cc := union(clients, addClients)
	groups, cg := union(stringsOf(target["groups"]), names(out["groups"]))
	merged = map[string]any{"actors": []string{}, "users": users, "groups": groups}
	if o != "" {
		merged["clients"] = clients // a user's own ACL takes no clients
	}
	return merged, cu || cc || cg
}

// targetACL reads the target's granular ACL for base.
func (rs *restorer) targetACL(ctx context.Context, base string) (map[string]map[string]any, error) {
	data, err := rs.do(ctx, "GET", base+"/_acl?detail=granular", nil, "1")
	if err != nil {
		return nil, err
	}
	var acl map[string]map[string]any
	if err := json.Unmarshal(data, &acl); err != nil {
		return nil, fmt.Errorf("decode %s/_acl: %w", base, err)
	}
	return acl, nil
}

// listing returns the names of a target collection with what each maps to,
// such as a cookbook's versions.
func (rs *restorer) listing(ctx context.Context, p string) (map[string]json.RawMessage, error) {
	data, err := rs.do(ctx, "GET", p, nil, "1")
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return m, nil
}

// targetVersions lists what an org that existed before this run already holds
// in a versioned collection: name -> version (cookbooks) or identifier
// (artifacts). For an org this run created it is empty.
func (rs *restorer) targetVersions(ctx context.Context, o, coll string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	if rs.track.newOrg(o) {
		return out, nil
	}
	p := "/organizations/" + esc(o) + "/" + coll
	if coll == "cookbooks" {
		p += "?num_versions=all"
	}
	m, err := rs.listing(ctx, p)
	if err != nil {
		return nil, err
	}
	for name, raw := range m {
		var entry struct {
			Versions []struct {
				Version    string `json:"version"`
				Identifier string `json:"identifier"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("decode %s: %w", p, err)
		}
		vs := map[string]bool{}
		for _, v := range entry.Versions {
			vs[v.Version+v.Identifier] = true
		}
		out[name] = vs
	}
	return out, nil
}

// names reads a member list as refs builds it ([]string) or as decoded JSON
// holds it ([]any).
func names(v any) []string {
	if l, ok := v.([]string); ok {
		return l
	}
	return stringsOf(v)
}
