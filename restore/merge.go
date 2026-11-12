package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/compare"
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
	// MergeOverwrite makes every object in the backup match the backup on the
	// target. An object the target holds differently is replaced, compared as
	// verify compares (values a server regenerates set aside), so one that
	// already matches isn't written. Groups, ACLs and policy group assignments
	// are set to exactly the backup's. Artifacts and policy revisions, which
	// can't change under their identifier, are left as they are, and so is a
	// frozen cookbook version unless ForceFrozen is set. Objects the backup
	// doesn't hold are not touched, and neither is a same-named actor that is
	// someone else (see sameActor).
	MergeOverwrite Merge = "overwrite"
)

// merging reports whether the target may already hold what is restored.
func (rs *restorer) merging() bool { return rs.merge == MergeSkip || rs.merge == MergeOverwrite }

// overwriting reports whether objects the target holds are made to match.
func (rs *restorer) overwriting() bool { return rs.merge == MergeOverwrite }

// ParseMerge checks a --merge value.
func ParseMerge(s string) (Merge, error) {
	switch m := Merge(s); m {
	case MergeNone, MergeSkip, MergeOverwrite:
		return m, nil
	}
	return "", fmt.Errorf("unknown merge mode %q (want skip or overwrite)", s)
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
// client restore is skipping) is the backup's actor at dir: whether it holds
// exactly the backup's keys, by name and public key, and no others. Public keys
// are public, so one matching key proves nothing on its own; requiring the
// whole set means every key the target's actor can sign with is one the
// backup's actor held. Without any key on either side, it is not the same.
func (rs *restorer) sameActor(ctx context.Context, base, dir string) (bool, error) {
	want := map[string]string{}
	for _, name := range rs.names(dir + "/keys/") {
		k, err := rs.object(dir + "/keys/" + name + ".json")
		if err != nil {
			return false, err
		}
		pub, _ := k["public_key"].(string)
		want[name] = squash(pub)
	}
	data, err := rs.do(ctx, "GET", base+"/keys", nil, "1")
	if errors.Is(err, cinc.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return false, fmt.Errorf("decode %s/keys: %w", base, err)
	}
	if len(want) == 0 || len(list) != len(want) {
		return false, nil
	}
	for _, e := range list {
		pub, ok := want[e.Name]
		if !ok {
			return false, nil
		}
		data, err := rs.do(ctx, "GET", base+"/keys/"+esc(e.Name), nil, "1")
		if err != nil {
			return false, err
		}
		var k struct {
			PublicKey string `json:"public_key"`
		}
		if err := json.Unmarshal(data, &k); err != nil {
			return false, fmt.Errorf("decode %s/keys/%s: %w", base, e.Name, err)
		}
		if pub == "" || squash(k.PublicKey) != pub {
			return false, nil
		}
	}
	return true, nil
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

func (r *Report) update(kind string) { r.mu.Lock(); r.Updated[kind]++; r.mu.Unlock() }

func (r *Report) skip(kind string) { r.mu.Lock(); r.Skipped[kind]++; r.mu.Unlock() }

// write sends a create. With MergeSkip, a 409 (the target already has it) is
// counted as skipped instead of failing; created says which happened.
//
// A dry run can't learn from a 409, so it asks has (whether the target already
// holds the object) and counts the skip from that instead.
//
// With MergeOverwrite, ow (when not nil) says how to make an existing object
// match the backup instead of skipping it.
func (rs *restorer) write(ctx context.Context, kind, method, p string, body []byte, has func(context.Context) (bool, error), ow *replacement) (created bool, err error) {
	if rs.dry && has != nil {
		ok, err := has(ctx)
		if err != nil {
			return false, err
		}
		if ok {
			return false, rs.existed(ctx, kind, ow)
		}
	}
	_, err = rs.do(ctx, method, p, body, "1")
	if err != nil && rs.merging() && errors.Is(err, cinc.ErrConflict) {
		return false, rs.existed(ctx, kind, ow)
	}
	return err == nil, err
}

// existed handles an object write found already on the target: overwrite it
// to match the backup, or skip it.
func (rs *restorer) existed(ctx context.Context, kind string, ow *replacement) error {
	if rs.overwriting() && ow != nil {
		return rs.replace(ctx, kind, *ow)
	}
	rs.rep.skip(kind)
	return nil
}

// replacement says how to make the target's copy of an object match the
// backup: read url, compare it with the backup's object at path, and PUT body
// to url if they differ.
type replacement struct {
	url, path string
	body      []byte
}

// replace makes the target's object match the backup's when it doesn't,
// counting it as updated, or as skipped when it already matches.
func (rs *restorer) replace(ctx context.Context, kind string, ow replacement) error {
	cur, err := rs.do(ctx, "GET", ow.url, nil, "1")
	if err != nil {
		return err
	}
	want, err := rs.r.Object(ow.path)
	if err != nil {
		return err
	}
	same, err := compare.Equal(ow.path, want, cur)
	if err != nil {
		return fmt.Errorf("%s: %w", ow.path, err)
	}
	if same {
		rs.rep.skip(kind)
		return nil
	}
	if _, err := rs.do(ctx, "PUT", ow.url, ow.body, "1"); err != nil {
		return err
	}
	rs.rep.update(kind)
	return nil
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

// sameLogin reports whether the target's user u signs in through the same
// external identity (external_authentication_uid, the SSO/LDAP login) as the
// backup's user at dir. Keys can't show who a login belongs to, but a different
// external identity means a different person. A password can't be compared:
// the API doesn't expose its hash.
func (rs *restorer) sameLogin(ctx context.Context, u, dir string) (bool, error) {
	backup, err := rs.object(dir + ".json")
	if err != nil {
		return false, err
	}
	data, err := rs.do(ctx, "GET", "/users/"+esc(u), nil, "1")
	if err != nil {
		return false, err
	}
	var target map[string]any
	if err := json.Unmarshal(data, &target); err != nil {
		return false, fmt.Errorf("decode /users/%s: %w", u, err)
	}
	uid := func(m map[string]any) string { s, _ := m["external_authentication_uid"].(string); return s }
	return uid(backup) == uid(target), nil
}

// sameSet reports whether a and b hold the same names, ignoring order and
// repeats.
func sameSet(a, b []string) bool {
	set := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, n := range l {
			m[n] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for n := range sa {
		if !sb[n] {
			return false
		}
	}
	return true
}

// sameACE reports whether the target's entry for one permission already grants
// exactly what the backup's (out, as refs.ACE resolved it) does. Untyped backup
// actors are typed as mergeACE types them.
func (rs *restorer) sameACE(o string, target, out map[string]any) bool {
	users, clients := names(out["users"]), names(out["clients"])
	for _, a := range names(out["actors"]) {
		if rs.refs.IsUser(a) || o == "" {
			users = append(users, a)
		} else {
			clients = append(clients, a)
		}
	}
	return sameSet(stringsOf(target["users"]), users) &&
		sameSet(stringsOf(target["clients"]), clients) &&
		sameSet(stringsOf(target["groups"]), names(out["groups"]))
}

// frozen reports whether a cookbook manifest has "frozen?" set.
func frozen(manifest []byte) bool {
	var m struct {
		Frozen bool `json:"frozen?"`
	}
	return json.Unmarshal(manifest, &m) == nil && m.Frozen
}
