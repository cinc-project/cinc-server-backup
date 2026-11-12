package restore

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/cinc-project/cinc-server-backup/format"
	"github.com/cinc-project/cinc-server-backup/internal/refs"
)

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

func TestParseMerge(t *testing.T) {
	for _, s := range []string{"", "skip", "overwrite"} {
		if _, err := ParseMerge(s); err != nil {
			t.Errorf("ParseMerge(%q): %v", s, err)
		}
	}
	if _, err := ParseMerge("replace"); err == nil {
		t.Error("ParseMerge(replace) accepted")
	}
}

func TestUnionKeepsOrderAndReportsAdditions(t *testing.T) {
	got, added := union([]string{"b", "a"}, []string{"a", "c", "c"})
	if strings.Join(got, ",") != "b,a,c" || !added {
		t.Errorf("union = %v, %v", got, added)
	}
	if _, added := union([]string{"a"}, []string{"a"}); added {
		t.Error("union reported an addition when there was none")
	}
}

// mergeACE adds the backup's entries to the target's, types untyped backup
// actors, and reports no change when the target already grants everything.
func TestMergeACE(t *testing.T) {
	rs := &restorer{}
	rs.refs = refs.FromBackup(emptyBackup(t), "alice")
	target := map[string]any{"users": []any{"pivotal"}, "clients": []any{}, "groups": []any{"admins"}}

	out, changed := rs.mergeACE("acme", target, map[string]any{
		"actors": []string{"alice", "web01"}, "groups": []string{"admins", "devs"},
	})
	if !changed {
		t.Fatal("no change reported")
	}
	want := map[string]any{"actors": []string{}, "users": []string{"pivotal", "alice"}, "clients": []string{"web01"}, "groups": []string{"admins", "devs"}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("merged = %v, want %v", out, want)
	}

	_, changed = rs.mergeACE("acme", target, map[string]any{
		"actors": []string{}, "users": []string{"pivotal"}, "clients": []string{}, "groups": []string{"admins"},
	})
	if changed {
		t.Error("a change reported when the target already grants everything")
	}

	// A user's own ACL takes users only.
	out, _ = rs.mergeACE("", map[string]any{"users": []any{}, "groups": []any{}}, map[string]any{"actors": []string{"bob"}, "groups": []string{}})
	if _, ok := out["clients"]; ok || !reflect.DeepEqual(out["users"], []string{"bob"}) {
		t.Errorf("user ACL merged = %v", out)
	}
}

func emptyBackup(t *testing.T) *format.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := format.NewWriter(&buf, format.Source{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := format.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// strangersOut drops a name only in lists the entry has, by type, so an
// untyped entry stays untyped and a client sharing a stranger user's name keeps
// its grant.
func TestStrangersOut(t *testing.T) {
	rs := &restorer{track: newTracker()}
	rs.track.stranger("users/alice")

	granular := rs.strangersOut("acme", map[string]any{
		"actors": []string{}, "users": []string{"alice", "bob"}, "clients": []string{"alice"}, "groups": []string{"admins"},
	})
	if !reflect.DeepEqual(granular["users"], []string{"bob"}) || !reflect.DeepEqual(granular["clients"], []string{"alice"}) {
		t.Errorf("granular = %v", granular)
	}

	untyped := rs.strangersOut("acme", map[string]any{"actors": []string{"alice", "bob"}, "groups": []string{}})
	if _, ok := untyped["users"]; ok {
		t.Errorf("an untyped entry gained a users list: %v", untyped)
	}
	if !reflect.DeepEqual(untyped["actors"], []string{"bob"}) {
		t.Errorf("untyped actors = %v", untyped["actors"])
	}
}
