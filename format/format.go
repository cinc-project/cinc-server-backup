// Package format reads and writes cinc-server-backup files (.csb).
//
// A backup is a zip archive. Every member except manifest.json is listed in
// the manifest with its SHA-256 and size, so a reader can prove that nothing
// was altered, added or lost. Objects are stored as the exact bytes the
// server returned; cookbook file contents are stored once per backup under
// blobs/, keyed by the MD5 checksum Chef uses for them.
//
// This package is the stable contract between the backup tool and anything
// that restores from its output (including cinc-server-ng's native restore),
// so it depends on nothing but the standard library.
package format

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// FormatName and Version identify the format in the manifest. A reader
// rejects any other name, and any major version newer than its own.
const (
	FormatName = "cinc-server-backup"
	Version    = 1
)

const manifestName = "manifest.json"

// Sources an entry can come from.
const (
	SourceAPI    = "api"    // a server API response
	SourceHost   = "host"   // read on the server host (database, files)
	SourceManual = "manual" // added by hand, e.g. by pack from an edited tree
)

var (
	// ErrPath reports a member path that is unsafe or outside the layout.
	ErrPath = errors.New("format: invalid path")
	// ErrChecksum reports content that does not match its recorded digest.
	ErrChecksum = errors.New("format: checksum mismatch")
	// ErrDuplicate reports a second write to the same path.
	ErrDuplicate = errors.New("format: duplicate path")
	// ErrFormat reports a file that is not a backup this package can read.
	ErrFormat = errors.New("format: not a supported backup")
	// ErrNotFound reports a path or blob the backup does not contain.
	ErrNotFound = errors.New("format: not found")
)

// topLevel lists the directories a backup may contain.
var topLevel = map[string]bool{"server": true, "orgs": true, "blobs": true, "host": true, "secrets": true}

// Source describes the server a backup was taken from.
type Source struct {
	URL         string `json:"url"`
	Flavor      string `json:"flavor,omitempty"` // "chef-server", "cinc-server-ng", ...
	Version     string `json:"version,omitempty"`
	APIVersions []int  `json:"api_versions,omitempty"`
}

// Window is when the backup was taken.
type Window struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Warning is a problem found while taking the backup that did not stop it.
type Warning struct {
	Code   string `json:"code"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Entry describes one member of the backup.
type Entry struct {
	Path       string    `json:"path"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Source     string    `json:"source,omitempty"`
	FetchedAt  time.Time `json:"fetched_at,omitzero"`
	APIVersion string    `json:"api_version,omitempty"`
}

// Manifest is the index stored as manifest.json.
type Manifest struct {
	Format      string         `json:"format"`
	Version     int            `json:"version"`
	Tool        string         `json:"tool,omitempty"`
	Source      Source         `json:"source"`
	Consistency string         `json:"consistency"`
	Window      Window         `json:"window"`
	Counts      map[string]int `json:"counts"`
	Warnings    []Warning      `json:"warnings,omitempty"`
	Entries     []Entry        `json:"entries"`
}

// Meta is what the caller knows about an object it stores.
type Meta struct {
	Source     string
	APIVersion string
	// FetchedAt defaults to the time of the PutObject call.
	FetchedAt time.Time
}

// ValidPath reports whether p is a safe member path inside the layout:
// relative, slash-separated, no empty, "." or ".." segment, no backslash,
// valid UTF-8, under a known top-level directory. manifest.json is reserved.
func ValidPath(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 || !topLevel[segs[0]] {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}

// BlobPath returns the member path of the blob with the given MD5 checksum.
func BlobPath(md5hex string) string {
	md5hex = strings.ToLower(md5hex)
	if len(md5hex) < 2 {
		return "blobs/" + md5hex
	}
	return "blobs/" + md5hex[:2] + "/" + md5hex
}

func validMD5(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// countKey names the collection a path is counted under, or "" for paths
// that are not counted (org metadata files, ACLs are counted as "acls").
func countKey(p string) string {
	segs := strings.Split(p, "/")
	switch segs[0] {
	case "blobs":
		return "blobs"
	case "server":
		switch {
		case len(segs) == 3 && segs[1] == "users":
			return "users"
		case len(segs) == 5 && segs[1] == "users" && segs[3] == "keys":
			return "user_keys"
		}
	case "orgs":
		if len(segs) == 3 && segs[2] == "org.json" {
			return "orgs"
		}
		if len(segs) < 4 {
			return ""
		}
		coll := segs[2]
		switch {
		case coll == "acls":
			return "acls"
		case coll == "data_bags" && len(segs) == 4:
			return "data_bags"
		case coll == "data_bags" && len(segs) == 5:
			return "data_bag_items"
		case coll == "clients" && len(segs) == 6 && segs[4] == "keys":
			return "client_keys"
		case coll == "cookbooks" && len(segs) == 5:
			return "cookbook_versions"
		case coll == "cookbook_artifacts" && len(segs) == 5:
			return "cookbook_artifacts"
		case coll == "policies" && len(segs) == 5:
			return "policy_revisions"
		case len(segs) == 4:
			return coll
		}
	}
	return ""
}

// Writer writes a backup. It is safe for concurrent use. Members are written
// as they arrive; Close writes the manifest and finishes the archive.
type Writer struct {
	mu       sync.Mutex
	zw       *zip.Writer
	source   Source
	entries  map[string]Entry
	warnings []Warning
	started  time.Time
	tool     string
	consist  string
	closed   bool
	now      func() time.Time
}

// NewWriter starts a backup written to w.
func NewWriter(w io.Writer, src Source) *Writer {
	return &Writer{
		zw:      zip.NewWriter(w),
		source:  src,
		entries: map[string]Entry{},
		consist: "fuzzy",
		now:     time.Now,
	}
}

// SetTool records the name and version of the program writing the backup.
func (w *Writer) SetTool(s string) { w.mu.Lock(); w.tool = s; w.mu.Unlock() }

// SetSource replaces the source description, e.g. once the server version
// is known.
func (w *Writer) SetSource(s Source) { w.mu.Lock(); w.source = s; w.mu.Unlock() }

// Warn records a warning in the manifest.
func (w *Writer) Warn(warn Warning) {
	w.mu.Lock()
	w.warnings = append(w.warnings, warn)
	w.mu.Unlock()
}

// start records the start of the backup window on first use. Callers hold mu.
func (w *Writer) start() time.Time {
	t := w.now().UTC()
	if w.started.IsZero() {
		w.started = t
	}
	return t
}

// PutObject stores data at path exactly as given.
func (w *Writer) PutObject(path string, data []byte, meta Meta) error {
	if !ValidPath(path) || strings.HasPrefix(path, "blobs/") {
		return fmt.Errorf("%w: %q", ErrPath, path)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.entries[path]; dup {
		return fmt.Errorf("%w: %q", ErrDuplicate, path)
	}
	now := w.start()
	if meta.FetchedAt.IsZero() {
		meta.FetchedAt = now
	}
	sum := sha256.Sum256(data)
	if err := w.writeMember(path, bytes.NewReader(data)); err != nil {
		return err
	}
	w.entries[path] = Entry{
		Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)),
		Source: meta.Source, FetchedAt: meta.FetchedAt.UTC(), APIVersion: meta.APIVersion,
	}
	return nil
}

// HasBlob reports whether the blob with this MD5 checksum is already stored.
func (w *Writer) HasBlob(md5hex string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.entries[BlobPath(md5hex)]
	return ok
}

// PutBlob stores r as the blob with the given MD5 checksum. The content is
// spooled to a temporary file and checked first, so a mismatch (ErrChecksum)
// leaves nothing in the backup. Storing a checksum that is already present
// is a no-op.
func (w *Writer) PutBlob(md5hex string, r io.Reader) error {
	md5hex = strings.ToLower(md5hex)
	if !validMD5(md5hex) {
		return fmt.Errorf("%w: blob checksum %q", ErrPath, md5hex)
	}
	if w.HasBlob(md5hex) {
		return nil
	}
	tmp, err := os.CreateTemp("", "csb-blob-*")
	if err != nil {
		return fmt.Errorf("format: spool blob: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	hm, hs := md5.New(), sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hm, hs), r)
	if err != nil {
		return fmt.Errorf("format: spool blob %s: %w", md5hex, err)
	}
	if got := hex.EncodeToString(hm.Sum(nil)); got != md5hex {
		return fmt.Errorf("%w: blob %s has md5 %s", ErrChecksum, md5hex, got)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("format: rewind blob: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	path := BlobPath(md5hex)
	if _, dup := w.entries[path]; dup {
		return nil // another goroutine stored it while we were hashing
	}
	now := w.start()
	if err := w.writeMember(path, tmp); err != nil {
		return err
	}
	w.entries[path] = Entry{Path: path, SHA256: hex.EncodeToString(hs.Sum(nil)), Size: size, FetchedAt: now}
	return nil
}

// writeMember adds one compressed member. Callers hold mu.
func (w *Writer) writeMember(path string, r io.Reader) error {
	if w.closed {
		return errors.New("format: writer is closed")
	}
	fw, err := w.zw.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: w.now().UTC()})
	if err != nil {
		return fmt.Errorf("format: add %s: %w", path, err)
	}
	if _, err := io.Copy(fw, r); err != nil {
		return fmt.Errorf("format: write %s: %w", path, err)
	}
	return nil
}

// Close writes the manifest and finishes the archive. It does not close the
// underlying writer.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	finished := w.start()
	m := Manifest{
		Format: FormatName, Version: Version, Tool: w.tool, Source: w.source,
		Consistency: w.consist,
		Window:      Window{StartedAt: w.started, FinishedAt: finished},
		Counts:      map[string]int{},
		Warnings:    w.warnings,
		Entries:     make([]Entry, 0, len(w.entries)),
	}
	for _, e := range w.entries {
		m.Entries = append(m.Entries, e)
		if k := countKey(e.Path); k != "" {
			m.Counts[k]++
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("format: encode manifest: %w", err)
	}
	if err := w.writeMember(manifestName, bytes.NewReader(data)); err != nil {
		return err
	}
	w.closed = true
	return w.zw.Close()
}

// Reader reads a backup.
type Reader struct {
	zr       *zip.Reader
	manifest Manifest
	entries  map[string]Entry
	files    map[string]*zip.File
}

// Open opens the backup file at path. The caller closes the returned file.
func Open(path string) (*Reader, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	r, err := NewReader(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return r, f, nil
}

// NewReader reads a backup from ra. It checks the manifest and that every
// member name is safe; it does not hash the contents (see Verify).
func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	r := &Reader{zr: zr, entries: map[string]Entry{}, files: map[string]*zip.File{}}
	var mf *zip.File
	for _, f := range zr.File {
		if f.Name == manifestName {
			mf = f
			continue
		}
		if !ValidPath(f.Name) {
			return nil, fmt.Errorf("%w: member %q", ErrPath, f.Name)
		}
		if _, dup := r.files[f.Name]; dup {
			return nil, fmt.Errorf("%w: member %q", ErrDuplicate, f.Name)
		}
		r.files[f.Name] = f
	}
	if mf == nil {
		return nil, fmt.Errorf("%w: no %s", ErrFormat, manifestName)
	}
	rc, err := mf.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: open manifest: %v", ErrFormat, err)
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(&r.manifest); err != nil {
		return nil, fmt.Errorf("%w: decode manifest: %v", ErrFormat, err)
	}
	if r.manifest.Format != FormatName {
		return nil, fmt.Errorf("%w: format %q", ErrFormat, r.manifest.Format)
	}
	if r.manifest.Version < 1 || r.manifest.Version > Version {
		return nil, fmt.Errorf("%w: version %d (this build reads up to %d)", ErrFormat, r.manifest.Version, Version)
	}
	for _, e := range r.manifest.Entries {
		if !ValidPath(e.Path) {
			return nil, fmt.Errorf("%w: manifest entry %q", ErrPath, e.Path)
		}
		r.entries[e.Path] = e
	}
	return r, nil
}

// Manifest returns the backup's manifest.
func (r *Reader) Manifest() Manifest { return r.manifest }

// Entry returns the manifest entry for path.
func (r *Reader) Entry(path string) (Entry, bool) {
	e, ok := r.entries[path]
	return e, ok
}

// Paths returns the manifest paths that start with prefix, sorted.
func (r *Reader) Paths(prefix string) []string {
	var out []string
	for p := range r.entries {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// open returns a reader for path that fails with ErrChecksum at EOF if the
// content does not match the manifest.
func (r *Reader) open(path string) (io.ReadCloser, error) {
	e, ok := r.entries[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	f, ok := r.files[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s is in the manifest but missing from the archive", ErrNotFound, path)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("format: open %s: %w", path, err)
	}
	return &verifyingReader{rc: rc, h: sha256.New(), want: e.SHA256, size: e.Size, path: path}, nil
}

// Object returns the bytes stored at path, checked against the manifest.
func (r *Reader) Object(path string) ([]byte, error) {
	rc, err := r.open(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// HasBlob reports whether the backup holds the blob with this MD5 checksum.
func (r *Reader) HasBlob(md5hex string) bool {
	_, ok := r.entries[BlobPath(md5hex)]
	return ok
}

// Blob opens the blob with this MD5 checksum. Reading it to EOF checks it
// against the manifest; a mismatch surfaces as ErrChecksum from Read.
func (r *Reader) Blob(md5hex string) (io.ReadCloser, error) {
	return r.open(BlobPath(md5hex))
}

// Verify checks every member against the manifest: each entry is present
// with the right size and digest, and no member is missing from the manifest.
// It returns all problems found, joined.
func (r *Reader) Verify() error {
	var errs []error
	for _, p := range r.Paths("") {
		rc, err := r.open(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: missing: %w", p, err))
			continue
		}
		_, err = io.Copy(io.Discard, rc)
		_ = rc.Close()
		if err != nil {
			errs = append(errs, err)
		}
	}
	names := make([]string, 0, len(r.files))
	for n := range r.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := r.entries[n]; !ok {
			errs = append(errs, fmt.Errorf("%s: member not in manifest", n))
		}
	}
	return errors.Join(errs...)
}

type verifyingReader struct {
	rc   io.ReadCloser
	h    hashWriter
	want string
	size int64
	n    int64
	path string
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	v.n += int64(n)
	_, _ = v.h.Write(p[:n])
	if err == io.EOF {
		if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want || v.n != v.size {
			return n, fmt.Errorf("%w: %s (sha256 %s, size %d; manifest says %s, %d)", ErrChecksum, v.path, got, v.n, v.want, v.size)
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.rc.Close() }
