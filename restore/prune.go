package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// prune deletes what org o on the target holds that the backup doesn't, so
// that, with MergeOverwrite having made the rest match, the org is an exact
// copy. It runs after everything else in the org has been written.
//
// It never deletes what every org comes with (default groups and containers,
// _default, the validator), per-user groups, or the superuser restore signs as.
// An org this run created holds only what the backup does, so there is nothing
// to prune in it.
func (rs *restorer) prune(ctx context.Context, o string) error {
	if !rs.pruning || rs.track.newOrg(o) {
		return nil
	}
	org := "/organizations/" + esc(o)
	inBackup := func(dir string) map[string]bool {
		m := map[string]bool{}
		for _, n := range rs.names(dir) {
			m[n] = true
		}
		return m
	}
	// del deletes each name the target lists under coll that keep doesn't
	// accept.
	del := func(kind, coll string, keep func(string) bool) error {
		have, err := rs.listing(ctx, org+"/"+coll)
		if err != nil {
			return err
		}
		for _, n := range sortedNames(have) {
			if keep(n) {
				continue
			}
			if err := rs.remove(ctx, kind, org+"/"+coll+"/"+esc(n)); err != nil {
				return err
			}
		}
		return nil
	}

	// Policy groups first, so no group still points at a revision below.
	if err := del("policy_groups", "policy_groups", has(inBackup("orgs/"+o+"/policy_groups/"))); err != nil {
		return err
	}
	if err := rs.prunePolicies(ctx, o); err != nil {
		return err
	}
	for _, coll := range []string{"cookbooks", "cookbook_artifacts"} {
		if err := rs.pruneVersions(ctx, o, coll); err != nil {
			return err
		}
	}
	for _, coll := range []string{"nodes", "roles"} {
		if err := del(coll, coll, has(inBackup("orgs/"+o+"/"+coll+"/"))); err != nil {
			return err
		}
	}
	envs := inBackup("orgs/" + o + "/environments/")
	if err := del("environments", "environments", func(n string) bool { return n == "_default" || envs[n] }); err != nil {
		return err
	}
	if err := rs.pruneDataBags(ctx, o); err != nil {
		return err
	}
	clients := inBackup("orgs/" + o + "/clients/")
	if err := del("clients", "clients", func(n string) bool { return n == o+"-validator" || clients[n] }); err != nil {
		return err
	}
	if err := rs.pruneMembers(ctx, o); err != nil {
		return err
	}
	groups := inBackup("orgs/" + o + "/groups/")
	if err := del("groups", "groups", func(n string) bool {
		return groups[n] || usag.MatchString(n) || orgDefaults(o, "groups")[n]
	}); err != nil {
		return err
	}
	containers := inBackup("orgs/" + o + "/containers/")
	return del("containers", "containers", func(n string) bool {
		return containers[n] || orgDefaults(o, "containers")[n]
	})
}

func has(m map[string]bool) func(string) bool { return func(n string) bool { return m[n] } }

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// remove deletes the object at p, counting it as pruned.
func (rs *restorer) remove(ctx context.Context, kind, p string) error {
	if _, err := rs.do(ctx, "DELETE", p, nil, "1"); err != nil {
		return err
	}
	rs.rep.pruned(kind)
	return nil
}

// prunePolicies deletes policy revisions the backup doesn't hold, and policies
// it holds none of.
func (rs *restorer) prunePolicies(ctx context.Context, o string) error {
	org := "/organizations/" + esc(o)
	want := map[string]map[string]bool{}
	for _, pr := range rs.versions(o, "policies") {
		name, rev := path.Split(pr)
		name = strings.TrimSuffix(name, "/")
		if want[name] == nil {
			want[name] = map[string]bool{}
		}
		want[name][rev] = true
	}
	have, err := rs.listing(ctx, org+"/policies")
	if err != nil {
		return err
	}
	for _, name := range sortedNames(have) {
		if want[name] == nil {
			if err := rs.remove(ctx, "policies", org+"/policies/"+esc(name)); err != nil {
				return err
			}
			continue
		}
		var entry struct {
			Revisions map[string]json.RawMessage `json:"revisions"`
		}
		if err := json.Unmarshal(have[name], &entry); err != nil {
			return fmt.Errorf("decode %s/policies: %w", org, err)
		}
		for _, rev := range sortedNames(entry.Revisions) {
			if !want[name][rev] {
				if err := rs.remove(ctx, "policy_revisions", org+"/policies/"+esc(name)+"/revisions/"+esc(rev)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pruneVersions deletes cookbook versions (or artifact identifiers) the backup
// doesn't hold.
func (rs *restorer) pruneVersions(ctx context.Context, o, coll string) error {
	org := "/organizations/" + esc(o)
	want := map[string]bool{}
	for _, nv := range rs.versions(o, coll) {
		want[nv] = true
	}
	have, err := rs.targetVersions(ctx, o, coll)
	if err != nil {
		return err
	}
	kind := "cookbook_versions"
	if coll == "cookbook_artifacts" {
		kind = "cookbook_artifacts"
	}
	for _, name := range sortedNames(have) {
		for _, v := range sortedNames(have[name]) {
			if want[name+"/"+v] {
				continue
			}
			if err := rs.remove(ctx, kind, org+"/"+coll+"/"+esc(name)+"/"+esc(v)); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneDataBags deletes data bags the backup doesn't hold, and items it
// doesn't hold from the bags it does.
func (rs *restorer) pruneDataBags(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o) + "/data"
	bags, err := rs.listing(ctx, base)
	if err != nil {
		return err
	}
	for _, bag := range sortedNames(bags) {
		items := map[string]bool{}
		for _, n := range rs.names("orgs/" + o + "/data_bags/" + bag + "/") {
			items[n] = true
		}
		if _, ok := rs.r.Entry("orgs/" + o + "/data_bags/" + bag + ".json"); !ok {
			if err := rs.remove(ctx, "data_bags", base+"/"+esc(bag)); err != nil {
				return err
			}
			continue
		}
		have, err := rs.listing(ctx, base+"/"+esc(bag))
		if err != nil {
			return err
		}
		for _, item := range sortedNames(have) {
			if !items[item] {
				if err := rs.remove(ctx, "data_bag_items", base+"/"+esc(bag)+"/"+esc(item)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pruneMembers removes from org o the users the backup doesn't list as
// members, and withdraws invitations it doesn't hold. The superuser restore
// signs as is never removed.
func (rs *restorer) pruneMembers(ctx context.Context, o string) error {
	base := "/organizations/" + esc(o)
	members, invites := map[string]bool{}, map[string]bool{}
	if data, err := rs.r.Object("orgs/" + o + "/members.json"); err == nil {
		var l []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &l); err != nil {
			return fmt.Errorf("orgs/%s/members.json: %w", o, err)
		}
		for _, m := range l {
			members[m.User.Username] = true
		}
	}
	if data, err := rs.r.Object("orgs/" + o + "/invitations.json"); err == nil {
		var l []struct {
			Username string `json:"username"`
		}
		if err := json.Unmarshal(data, &l); err != nil {
			return fmt.Errorf("orgs/%s/invitations.json: %w", o, err)
		}
		for _, inv := range l {
			invites[inv.Username] = true
		}
	}
	data, err := rs.do(ctx, "GET", base+"/users", nil, "1")
	if err != nil {
		return err
	}
	var have []struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &have); err != nil {
		return fmt.Errorf("decode %s/users: %w", base, err)
	}
	for _, m := range have {
		u := m.User.Username
		if members[u] || u == rs.super {
			continue
		}
		if err := rs.remove(ctx, "members", base+"/users/"+esc(u)); err != nil {
			return err
		}
	}
	data, err = rs.do(ctx, "GET", base+"/association_requests", nil, "1")
	if err != nil {
		return err
	}
	var pending []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return fmt.Errorf("decode %s/association_requests: %w", base, err)
	}
	for _, inv := range pending {
		if !invites[inv.Username] {
			if err := rs.remove(ctx, "invitations", base+"/association_requests/"+esc(inv.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneUsers deletes the users the backup doesn't hold, but only on a full
// restore into a target with no orgs the backup doesn't hold: anywhere else, a
// user may belong to an org this restore doesn't manage. The superuser restore
// signs as is never deleted. It reports whether users were pruned.
func (rs *restorer) pruneUsers(ctx context.Context) (bool, error) {
	if !rs.pruning || !rs.sel.All {
		return false, nil
	}
	orgs, err := rs.listing(ctx, "/organizations")
	if err != nil {
		return false, err
	}
	ours := map[string]bool{}
	for _, o := range rs.orgs() {
		ours[o] = true
	}
	for o := range orgs {
		if !ours[o] {
			return false, nil
		}
	}
	keep := map[string]bool{rs.super: true}
	for _, u := range rs.names("server/users/") {
		keep[u] = true
	}
	users, err := rs.listing(ctx, "/users")
	if err != nil {
		return false, err
	}
	for _, u := range sortedNames(users) {
		if !keep[u] {
			if err := rs.remove(ctx, "users", "/users/"+esc(u)); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}
