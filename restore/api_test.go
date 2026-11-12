package restore

import "testing"

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
