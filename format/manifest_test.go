package format

import (
	"strings"
	"testing"
)

const sumA, sumB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

func TestManifestFilesReadsOnlyManifestFileLists(t *testing.T) {
	refs, err := ManifestFiles([]byte(`{
		"all_files":[{"name":"recipes/default.rb","checksum":"` + sumA + `","url":"u1"}],
		"recipes":[{"name":"default.rb","checksum":"` + sumB + `","url":"u2"}],
		"metadata":{"attributes":{"planted":{"checksum":"` + sumA + `","url":"http://169.254.169.254/"}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Checksum] = r.URL
	}
	// A {"checksum","url"} object an uploader wrote into metadata is not a file.
	if len(refs) != 2 || got[sumA] != "u1" || got[sumB] != "u2" {
		t.Fatalf("refs = %+v", refs)
	}
	if _, err := ManifestFiles([]byte(`{`)); err == nil {
		t.Fatal("bad manifest accepted")
	}
}

func TestManifestFilesRejectsMalformedChecksum(t *testing.T) {
	for _, sum := range []string{"", "abc", "../../etc/passwd", strings.Repeat("g", 32)} {
		if _, err := ManifestFiles([]byte(`{"all_files":[{"checksum":"` + sum + `","url":"u"}]}`)); err == nil {
			t.Errorf("checksum %q accepted", sum)
		}
	}
}
