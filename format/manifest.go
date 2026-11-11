package format

import (
	"encoding/json"
	"fmt"
	"strings"
)

// File is one file a cookbook or cookbook artifact manifest lists.
type File struct {
	Checksum string // lower-case hex MD5
	URL      string // where the server said it could be fetched; empty on upload
}

// fileLists are the manifest members that list a cookbook's files: all_files
// (API v2) and the per-segment lists of v0/v1. Nothing else in a manifest is
// read for files, because metadata holds whatever the uploader wrote.
var fileLists = []string{"all_files", "recipes", "definitions", "libraries", "attributes", "files", "templates", "resources", "providers", "root_files"}

// ManifestFiles returns the files a cookbook or artifact manifest lists. A
// checksum that is not 32 hex digits is an error.
func ManifestFiles(manifest []byte) ([]File, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var out []File
	for _, list := range fileLists {
		raw, ok := doc[list]
		if !ok {
			continue
		}
		var files []struct {
			Checksum string `json:"checksum"`
			URL      string `json:"url"`
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, fmt.Errorf("decode manifest %s: %w", list, err)
		}
		for _, f := range files {
			sum := strings.ToLower(f.Checksum)
			if !validMD5(sum) {
				return nil, fmt.Errorf("manifest %s lists a file with checksum %q, not an MD5", list, f.Checksum)
			}
			out = append(out, File{Checksum: sum, URL: f.URL})
		}
	}
	return out, nil
}
