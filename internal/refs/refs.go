// Package refs resolves the names a backup's ACLs and groups refer to
// against the users, clients and groups the backup itself holds. A name the
// backup does not hold is dangling: a server refuses an ACL that names it,
// so restore leaves it out, and verify expects the target to lack it.
package refs

import (
	"regexp"
	"strings"

	"github.com/cinc-project/cinc-server-backup/format"
)

// USAG matches the per-user groups a server creates when a user joins an
// org. They are named after the user's internal id, which a target assigns
// afresh, so they are recreated by association rather than restored.
var USAG = regexp.MustCompile(`^[0-9a-f]{32}$`)

type nameSet map[string]bool

// Known is every user, client and group a restore of a backup writes.
type Known struct {
	users   nameSet
	clients map[string]nameSet // by org
	groups  map[string]nameSet // by org, per-user groups aside
}

// FromBackup collects the names r holds, plus extraUsers (such as the
// superuser a restore signs as, which the target already has).
func FromBackup(r *format.Reader, extraUsers ...string) *Known {
	k := &Known{users: nameSet{}, clients: map[string]nameSet{}, groups: map[string]nameSet{}}
	for _, u := range append(names(r, "server/users/"), extraUsers...) {
		k.users[u] = true
	}
	for _, o := range Orgs(r) {
		c := nameSet{}
		for _, n := range names(r, "orgs/"+o+"/clients/") {
			c[n] = true
		}
		g := nameSet{}
		for _, n := range names(r, "orgs/"+o+"/groups/") {
			if !USAG.MatchString(n) {
				g[n] = true
			}
		}
		k.clients[o], k.groups[o] = c, g
	}
	return k
}

// Orgs lists the organizations in r.
func Orgs(r *format.Reader) []string {
	var out []string
	for _, p := range r.Paths("orgs/") {
		segs := strings.Split(p, "/")
		if len(segs) == 3 && segs[2] == "org.json" {
			out = append(out, segs[1])
		}
	}
	return out
}

// names returns the last path element, without .json, of each path in r
// directly under dir.
func names(r *format.Reader, dir string) []string {
	var out []string
	for _, p := range r.Paths(dir) {
		rest := strings.TrimPrefix(p, dir)
		if !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".json") {
			out = append(out, strings.TrimSuffix(rest, ".json"))
		}
	}
	return out
}

// Strings returns the strings in a decoded JSON list.
func Strings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// keep splits l into the names ok accepts and the rest, each of the rest
// labelled with kind ("user ghost").
func keep(l []string, kind string, ok func(string) bool) (kept, dropped []string) {
	kept = []string{}
	for _, n := range l {
		if ok(n) {
			kept = append(kept, n)
		} else {
			dropped = append(dropped, kind+" "+n)
		}
	}
	return kept, dropped
}

// ACE resolves one permission of a backed-up ACL in org o ("" for a user's
// own ACL, whose actors are users only and whose groups a server does not
// check). It returns the entry without per-user groups (usags) or names the
// backup does not hold (dangling, labelled "user ghost"). An entry in the
// granular form, with users and clients apart, keeps that form.
func (k *Known) ACE(o string, ace map[string]any) (out map[string]any, usags, dangling []string) {
	isUser := func(n string) bool { return k.users[n] }
	isClient := func(n string) bool { return o != "" && k.clients[o][n] }
	isActor := func(n string) bool { return isUser(n) || isClient(n) }
	isGroup := func(n string) bool { return o == "" || k.groups[o][n] }

	groups := []string{}
	for _, g := range Strings(ace["groups"]) {
		if USAG.MatchString(g) {
			usags = append(usags, g)
		} else {
			groups = append(groups, g)
		}
	}
	groups, d := keep(groups, "group", isGroup)
	dangling = append(dangling, d...)

	actorsOK := isActor
	if o == "" {
		actorsOK = isUser
	}
	actors, d := keep(Strings(ace["actors"]), "actor", actorsOK)
	dangling = append(dangling, d...)
	out = map[string]any{"actors": actors, "groups": groups}

	users, uok := ace["users"]
	clients, cok := ace["clients"]
	if uok || cok {
		u, du := keep(Strings(users), "user", isUser)
		c, dc := keep(Strings(clients), "client", isClient)
		dangling = append(append(dangling, du...), dc...)
		out["actors"] = []string{}
		out["users"], out["clients"] = u, c
	}
	return out, usags, dangling
}

// Members resolves a backed-up group of org o: its users, clients and
// groups without per-user groups or names the backup does not hold.
func (k *Known) Members(o string, doc map[string]any) (users, clients, groups, dangling []string) {
	users, du := keep(Strings(doc["users"]), "user", func(n string) bool { return k.users[n] })
	clients, dc := keep(Strings(doc["clients"]), "client", func(n string) bool { return k.clients[o][n] })
	var sub []string
	for _, g := range Strings(doc["groups"]) {
		if !USAG.MatchString(g) {
			sub = append(sub, g)
		}
	}
	groups, dg := keep(sub, "group", func(n string) bool { return k.groups[o][n] })
	return users, clients, groups, append(append(du, dc...), dg...)
}

// Scope says which org the ACL or group stored at p belongs to: ok is false
// for any other path, and o is "" for a user's own ACL.
func Scope(p string) (o string, acl, group, ok bool) {
	segs := strings.Split(p, "/")
	switch {
	case len(segs) == 4 && segs[0] == "server" && segs[1] == "users" && segs[3] == "acl.json":
		return "", true, false, true
	case len(segs) == 3 && segs[0] == "orgs" && segs[2] == "acl.json":
		return segs[1], true, false, true
	case len(segs) == 5 && segs[0] == "orgs" && segs[2] == "acls":
		return segs[1], true, false, true
	case len(segs) == 4 && segs[0] == "orgs" && segs[2] == "groups":
		return segs[1], false, true, true
	}
	return "", false, false, false
}

// IsUser reports whether the backup (or the extra users FromBackup was given)
// holds a user of this name.
func (k *Known) IsUser(n string) bool { return k.users[n] }
