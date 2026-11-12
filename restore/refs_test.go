package restore

import (
	"reflect"
	"strings"
	"testing"
)

func testKnown() *known {
	return &known{
		users:   setOf([]string{"alice", "pivotal"}),
		clients: map[string]nameSet{"acme": setOf([]string{"web01", "alice"})},
		groups:  map[string]nameSet{"acme": setOf([]string{"admins", "devs"})},
	}
}

func list(s ...string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

const usagName = "0123456789abcdef0123456789abcdef"

func TestACEDropsDanglingGranularNames(t *testing.T) {
	out, usags, dangling := testKnown().ace("acme", map[string]any{
		"actors":  list(),
		"users":   list("alice", "ghost"),
		"clients": list("web01", "gone01", "pivotal"),
		"groups":  list("admins", usagName, "ops"),
	})
	want := map[string]any{
		"actors":  []string{},
		"users":   []string{"alice"},
		"clients": []string{"web01"},
		"groups":  []string{"admins"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v, want %v", out, want)
	}
	if !reflect.DeepEqual(usags, []string{usagName}) {
		t.Errorf("usags = %v", usags)
	}
	// pivotal is a user, not a client of acme.
	if got, w := strings.Join(dangling, ","), "group ops,user ghost,client gone01,client pivotal"; got != w {
		t.Errorf("dangling = %s, want %s", got, w)
	}
}

func TestACEUntypedActorsMatchUsersOrClients(t *testing.T) {
	out, _, dangling := testKnown().ace("acme", map[string]any{
		"actors": list("alice", "web01", "ghost"),
		"groups": list(),
	})
	if want := []string{"alice", "web01"}; !reflect.DeepEqual(out["actors"], want) {
		t.Errorf("actors = %v, want %v", out["actors"], want)
	}
	if _, ok := out["users"]; ok {
		t.Error("an untyped ACE came back in the granular form")
	}
	if !reflect.DeepEqual(dangling, []string{"actor ghost"}) {
		t.Errorf("dangling = %v", dangling)
	}
}

// A user's own ACL lives outside any org: its actors are users only, and
// the server does not check its groups.
func TestACEOfUserACLChecksUsersOnly(t *testing.T) {
	out, _, dangling := testKnown().ace("", map[string]any{
		"actors": list("alice", "web01"),
		"groups": list("whatever"),
	})
	if want := []string{"alice"}; !reflect.DeepEqual(out["actors"], want) {
		t.Errorf("actors = %v, want %v", out["actors"], want)
	}
	if want := []string{"whatever"}; !reflect.DeepEqual(out["groups"], want) {
		t.Errorf("groups = %v, want %v", out["groups"], want)
	}
	if !reflect.DeepEqual(dangling, []string{"actor web01"}) {
		t.Errorf("dangling = %v", dangling)
	}
}

func TestMembersDropDanglingNamesAndUSAGs(t *testing.T) {
	users, clients, groups, dangling := testKnown().members("acme", map[string]any{
		"users":   list("alice", "ghost"),
		"clients": list("gone01"),
		"groups":  list("devs", usagName, "ops"),
	})
	if !reflect.DeepEqual(users, []string{"alice"}) || !reflect.DeepEqual(clients, []string{}) || !reflect.DeepEqual(groups, []string{"devs"}) {
		t.Errorf("members = %v %v %v", users, clients, groups)
	}
	if got, w := strings.Join(dangling, ","), "user ghost,client gone01,group ops"; got != w {
		t.Errorf("dangling = %s, want %s", got, w)
	}
}

func TestParseDangling(t *testing.T) {
	for _, s := range []string{"drop", "fail"} {
		if _, err := ParseDangling(s); err != nil {
			t.Errorf("ParseDangling(%q): %v", s, err)
		}
	}
	if _, err := ParseDangling("keep"); err == nil {
		t.Error("ParseDangling(keep) accepted")
	}
}
