// Package cli implements the cinc-server-backup command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"

	cinc "github.com/cinc-project/cinc-api"

	"github.com/cinc-project/cinc-server-backup/capture"
	"github.com/cinc-project/cinc-server-backup/format"
)

// Version is the tool version, set at build time.
var Version = "dev"

const usage = `usage: cinc-server-backup <command> [flags]

commands:
  backup   take a backup of a server over its API
  inspect  print what a backup contains
  verify   check a backup's integrity
`

// errUsage marks a command-line mistake (exit 2) rather than a failure (exit 1).
var errUsage = errors.New("usage")

// Run executes the command line args (without the program name) and returns
// the process exit code: 0 success, 1 failure, 2 usage error.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "backup":
		err = backup(args[1:], stdout, stderr)
	case "inspect":
		err = inspect(args[1:], stdout)
	case "verify":
		err = verify(args[1:], stdout)
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintln(stderr, err)
		return 2
	default:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}

func backup(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "server URL, e.g. https://chef.example.com")
	user := fs.String("user", "pivotal", "superuser to sign requests as")
	keyPath := fs.String("key", "", "path to the superuser's private key")
	out := fs.String("o", "", "backup file to write")
	conc := fs.Int("concurrency", 8, "maximum requests in flight")
	var blobHosts stringList
	fs.Var(&blobHosts, "blob-host", "extra host[:port] cookbook files may be fetched from, e.g. an S3 bookshelf (repeatable)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	for _, req := range []struct{ name, val string }{{"--server", *server}, {"--key", *keyPath}, {"-o", *out}} {
		if req.val == "" {
			return fmt.Errorf("%w: backup needs %s", errUsage, req.name)
		}
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", *out)
	}
	pem, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	key, err := cinc.ParseKey(pem)
	if err != nil {
		return fmt.Errorf("%s: %w", *keyPath, err)
	}
	// Config.Org is required by the client but unused: the crawler sends
	// absolute paths.
	c, err := cinc.NewClient(cinc.Config{ServerURL: *server, Org: "_", ClientName: *user, Key: key},
		cinc.WithUserAgent("cinc-server-backup/"+Version))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	partial := *out + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(partial)
		}
	}()
	w := format.NewWriter(f, format.Source{URL: *server})
	w.SetTool("cinc-server-backup " + Version)
	if err := capture.FromAPI(ctx, c, w, capture.Options{Concurrency: *conc, BlobHosts: blobHosts}); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, *out); err != nil {
		return err
	}
	ok = true
	fmt.Fprintf(stdout, "wrote %s\n", *out)
	return nil
}

// openArg opens the single backup file named in args.
func openArg(cmd string, args []string) (*format.Reader, io.Closer, error) {
	if len(args) != 1 {
		return nil, nil, fmt.Errorf("%w: %s <backup.csb>", errUsage, cmd)
	}
	return format.Open(args[0])
}

func verify(args []string, stdout io.Writer) error {
	r, f, err := openArg("verify", args)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := r.Verify(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: %d entries verified\n", len(r.Manifest().Entries))
	return nil
}

func inspect(args []string, stdout io.Writer) error {
	r, f, err := openArg("inspect", args)
	if err != nil {
		return err
	}
	defer f.Close()
	m := r.Manifest()
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "format\t%s v%d\n", m.Format, m.Version)
	fmt.Fprintf(tw, "tool\t%s\n", m.Tool)
	fmt.Fprintf(tw, "source\t%s\n", m.Source.URL)
	if m.Source.Version != "" {
		fmt.Fprintf(tw, "server version\t%s\n", m.Source.Version)
	}
	fmt.Fprintf(tw, "taken\t%s to %s (%s)\n", m.Window.StartedAt.Format("2006-01-02 15:04:05Z"), m.Window.FinishedAt.Format("2006-01-02 15:04:05Z"), m.Consistency)
	keys := make([]string, 0, len(m.Counts))
	for k := range m.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(tw, "\ncounts")
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%d\n", k, m.Counts[k])
	}
	if len(m.Warnings) > 0 {
		fmt.Fprintf(tw, "\nwarnings (%d)\n", len(m.Warnings))
		for _, w := range m.Warnings {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", w.Code, w.Path, w.Detail)
		}
	}
	return tw.Flush()
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
