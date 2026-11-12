package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/format"
)

// Write is one request a restore sends to change the target.
type Write struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// What every new organization comes with, as Chef Infra Server creates it (and
// cinc-server-ng mirrors). A dry run can't ask a target about an org it hasn't
// created, so it plans against these; a real run lists the target instead.
var (
	defaultContainers = []string{
		"clients", "containers", "cookbooks", "cookbook_artifacts", "data",
		"environments", "groups", "nodes", "policies", "policy_groups",
		"roles", "sandboxes",
	}
	defaultGroups = []string{"admins", "billing-admins", "clients", "users", "public_key_read_access"}
)

// orgDefaults returns what a new org o holds in coll.
func orgDefaults(o, coll string) map[string]bool {
	var names []string
	switch coll {
	case "containers":
		names = defaultContainers
	case "groups":
		names = defaultGroups
	case "clients":
		names = []string{o + "-validator"}
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func (r *Report) record(method, p string) {
	r.mu.Lock()
	r.Writes = append(r.Writes, Write{Method: method, Path: p})
	r.mu.Unlock()
}

// conflict records a reason the restore would refuse to start.
func (r *Report) conflict(s string) { r.mu.Lock(); r.Conflicts = append(r.Conflicts, s); r.mu.Unlock() }

// sortWrites orders the recorded writes by path, then method.
func (r *Report) sortWrites() {
	sort.SliceStable(r.Writes, func(i, j int) bool {
		if r.Writes[i].Path != r.Writes[j].Path {
			return r.Writes[i].Path < r.Writes[j].Path
		}
		return r.Writes[i].Method < r.Writes[j].Method
	})
}

// exists reports whether the target holds the object at p. A dry run asks only
// about an org that is already on the target: one the run would create holds
// nothing yet.
func (rs *restorer) exists(ctx context.Context, o, p string) (bool, error) {
	if rs.track.newOrg(o) {
		return false, nil
	}
	_, err := rs.do(ctx, "GET", p, nil, "1")
	if errors.Is(err, cinc.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// invited reports whether org o on the target already has an invitation for
// user, for a dry run (a real one learns it from a 409).
func (rs *restorer) invited(ctx context.Context, o, user string) (bool, error) {
	if rs.track.newOrg(o) {
		return false, nil
	}
	data, err := rs.do(ctx, "GET", "/organizations/"+esc(o)+"/association_requests", nil, "1")
	if err != nil {
		return false, err
	}
	var invites []struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &invites); err != nil {
		return false, fmt.Errorf("decode association_requests: %w", err)
	}
	for _, inv := range invites {
		if inv.Username == user {
			return true, nil
		}
	}
	return false, nil
}

// countDangling counts the dangling names among warnings.
func countDangling(ws []format.Warning) int {
	n := 0
	for _, w := range ws {
		if strings.HasSuffix(w.Code, ".dangling_dropped") {
			n++
		}
	}
	return n
}

// joins records, for a dry run, a member the server would add to one of its
// own groups as a side effect of a write: Chef puts a new client in the org's
// clients group and a new member in its users group. A real run finds them
// there when it merges group membership; a dry run has to add them itself, or
// it would plan to add them again.
func (rs *restorer) joins(o, group, name string) {
	if !rs.dry {
		return
	}
	v, _ := rs.planned.LoadOrStore("join\x00"+o+"\x00"+group, &joinList{})
	l := v.(*joinList)
	l.mu.Lock()
	l.names = append(l.names, name)
	l.mu.Unlock()
}

// joined returns the members of kind ("users" or "clients") a dry run expects
// the server to have added to group g of org o. A real run returns none: it
// reads the group.
func (rs *restorer) joined(o, g, kind string) []string {
	if !rs.dry || g != kind {
		return nil
	}
	v, ok := rs.planned.Load("join\x00" + o + "\x00" + g)
	if !ok {
		return nil
	}
	l := v.(*joinList)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

type joinList struct {
	mu    sync.Mutex
	names []string
}

// at is the existence check for write: whether org o's object at p is on the
// target.
func (rs *restorer) at(o, p string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) { return rs.exists(ctx, o, p) }
}
