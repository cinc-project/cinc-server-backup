// Package cli implements the cinc-server-backup command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	cinc "github.com/cinc-project/cinc-api"
	"github.com/spf13/cobra"

	"github.com/cinc-project/cinc-server-backup/capture"
	"github.com/cinc-project/cinc-server-backup/format"
	"github.com/cinc-project/cinc-server-backup/restore"
)

// Version is the tool version, set at build time.
var Version = "dev"

const progName = "cinc-server-backup"

// usageError marks a command-line mistake (exit 2) rather than a failure
// (exit 1).
type usageError struct{ error }

func usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }

// Run executes the command line args (without the program name) and returns
// the process exit code: 0 success, 1 failure, 2 usage error.
func Run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if len(args) == 0 {
		root.SetOut(stderr)
		_ = root.Help()
		return 2
	}
	cmd, err := root.ExecuteContextC(context.Background())
	if err == nil {
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		fmt.Fprintf(stderr, "Error: %v\nRun '%s --help' to see how to use it.\n", err, cmd.CommandPath())
		return 2
	}
	fmt.Fprintln(stderr, "Error:", err)
	return 1
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   progName,
		Short: "Back up and restore a whole Cinc Server or Chef Infra Server",
		Long: `cinc-server-backup takes a full backup of a Cinc Server or Chef Infra Server
into one portable file: users and their keys, organizations, members, ACLs,
groups, clients, nodes, roles, environments, data bags, every cookbook
version with its files, cookbook artifacts and every policy revision.

The same file restores into a Cinc Server, a Chef Infra Server, or
cinc-server-ng. Every object is stored exactly as the server sent it, and a
manifest of checksums lets you prove later that nothing changed.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newBackupCmd(), newRestoreCmd(), newInspectCmd(), newVerifyCmd(), newVersionCmd())
	return root
}

// exactlyOneBackup is the Args check for commands that take one backup file.
func exactlyOneBackup(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return usagef("%s needs exactly one backup file, and we got %d", cmd.Name(), len(args))
	}
	return nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version of cinc-server-backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", progName, Version)
			return nil
		},
	}
}

type backupFlags struct {
	server, user, key, output string
	concurrency               int
	blobHosts                 []string
}

func newBackupCmd() *cobra.Command {
	var f backupFlags
	cmd := &cobra.Command{
		Use:   "backup --server <url> --key <pem> -o <file.csb>",
		Short: "Take a backup of a whole server over its API",
		Long: `Take a backup of a whole server over its API, signing every request as a
superuser (pivotal, unless you say otherwise with --user).

The backup is written to <file>.partial and renamed to the name you gave only
once it is complete, so a file with the final name is always a whole backup.
It is created readable by you alone, and we never overwrite an existing file.

Cookbook files are fetched only from the server's own host. If your server
keeps them in an external bookshelf such as S3, allow that host with
--blob-host.`,
		Example: `Back up a server with the pivotal key, on the server itself.
cinc-server-backup backup --server https://chef.example.com --key /etc/opscode/pivotal.pem -o chef.csb

Back up a server whose cookbook files live in an S3 bucket.
cinc-server-backup backup --server https://chef.example.com --key pivotal.pem \
  --blob-host chef-bookshelf.s3.us-east-1.amazonaws.com -o chef.csb`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runBackup(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "server URL, such as https://chef.example.com (required)")
	fl.StringVar(&f.user, "user", "pivotal", "superuser to sign requests as")
	fl.StringVar(&f.key, "key", "", "path to that superuser's private key (required)")
	fl.StringVarP(&f.output, "output", "o", "", "backup file to write, such as chef.csb (required)")
	fl.IntVar(&f.concurrency, "concurrency", 8, "most requests to have in flight at once")
	fl.StringArrayVar(&f.blobHosts, "blob-host", nil, "extra host[:port] cookbook files may come from, such as an S3 bookshelf (repeatable)")
	return cmd
}

func runBackup(cmd *cobra.Command, f backupFlags) error {
	var missing []string
	for _, req := range []struct{ name, val string }{{"--server", f.server}, {"--key", f.key}, {"-o", f.output}} {
		if req.val == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return usagef("backup needs %s", strings.Join(missing, ", "))
	}
	if _, err := os.Stat(f.output); err == nil {
		return fmt.Errorf("%s already exists, and we won't overwrite a backup; choose another -o path or move the old file", f.output)
	}
	opts := capture.Options{Concurrency: f.concurrency, BlobHosts: f.blobHosts}
	c, err := newClient(f.server, f.user, f.key, opts.BlobHosts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	partial := f.output + ".partial"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
			_ = os.Remove(partial)
		}
	}()
	w := format.NewWriter(file, format.Source{URL: f.server})
	w.SetTool(progName + " " + Version)
	if err := capture.FromAPI(ctx, c, w, opts); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, f.output); err != nil {
		return err
	}
	ok = true
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", f.output)
	return nil
}

// newClient builds a client signing as user with the key at keyPath. File
// transfers may follow redirects only to the server's host or blobHosts.
func newClient(server, user, keyPath string, blobHosts []string) (*cinc.Client, error) {
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("we couldn't read the key: %w", err)
	}
	key, err := cinc.ParseKey(pem)
	if err != nil {
		return nil, fmt.Errorf("%s isn't a private key we can use: %w", keyPath, err)
	}
	// cinc-api follows redirects for file transfers only; hold every hop to
	// the same host rules as the URL the server gave.
	policy := capture.RedirectPolicy(server, capture.Options{BlobHosts: blobHosts})
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: policy}
	// Config.Org is required by the client but unused: every request is
	// sent with an absolute path.
	return cinc.NewClient(cinc.Config{ServerURL: server, Org: "_", ClientName: user, Key: key},
		cinc.WithUserAgent(progName+"/"+Version), cinc.WithHTTPClient(hc))
}

type restoreFlags struct {
	server, user, key string
	concurrency       int
	blobHosts         []string
}

func newRestoreCmd() *cobra.Command {
	var f restoreFlags
	cmd := &cobra.Command{
		Use:   "restore <file.csb> --server <url> --key <pem>",
		Short: "Restore a backup into a server over its API",
		Long: `Restore a backup into a Cinc Server, Chef Infra Server or cinc-server-ng,
signing every request as a superuser (pivotal, unless you say otherwise with
--user). That superuser is left as the target has it.

The target must not already have any of the backup's organizations or users:
restore creates, it does not merge. We check the whole backup against its
checksums before sending anything, then write users and their keys, orgs and
members, clients and their keys, groups, every object, cookbook and policy,
and finally the ACLs.

Clients and users keep their public keys, so existing client.pem and
validation.pem files work against the new server. Passwords can't be carried
over the API: restored users get a random one until they reset it.`,
		Example: `Restore a backup into a new, empty server.
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem`,
		Args: exactlyOneBackup,
		RunE: func(cmd *cobra.Command, args []string) error { return runRestore(cmd, args[0], f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "URL of the server to restore into (required)")
	fl.StringVar(&f.user, "user", "pivotal", "superuser to sign requests as")
	fl.StringVar(&f.key, "key", "", "path to that superuser's private key (required)")
	fl.IntVar(&f.concurrency, "concurrency", 8, "most requests to have in flight at once")
	fl.StringArrayVar(&f.blobHosts, "blob-host", nil, "extra host[:port] file uploads may be redirected to, such as an S3 bookshelf (repeatable)")
	return cmd
}

func runRestore(cmd *cobra.Command, file string, f restoreFlags) error {
	var missing []string
	for _, req := range []struct{ name, val string }{{"--server", f.server}, {"--key", f.key}} {
		if req.val == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return usagef("restore needs %s", strings.Join(missing, ", "))
	}
	r, fh, err := format.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }() // read-only
	if err := r.Verify(); err != nil {
		return fmt.Errorf("the backup failed its checksum check, so we won't restore it: %w", err)
	}
	c, err := newClient(f.server, f.user, f.key, f.blobHosts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	rep, err := restore.ToAPI(ctx, r, c, restore.Options{Concurrency: f.concurrency})
	if rep != nil {
		printReport(cmd.OutOrStdout(), rep)
	}
	return err
}

func printReport(out io.Writer, rep *restore.Report) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	keys := make([]string, 0, len(rep.Restored))
	for k := range rep.Restored {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(tw, "restored")
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%d\n", k, rep.Restored[k])
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprintf(tw, "\nleft out (%d)\n", len(rep.Warnings))
		for _, w := range rep.Warnings {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", w.Code, w.Path, w.Detail)
		}
	}
	_ = tw.Flush()
}

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <file.csb>",
		Short: "Check that a backup is complete and unchanged",
		Long: `Check every file in a backup against the checksums in its manifest. Verify
fails if anything was changed, removed or added since the backup was written.`,
		Example: `Check a backup before relying on it.
cinc-server-backup verify chef.csb`,
		Args: exactlyOneBackup,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, f, err := format.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }() // read-only
			if err := r.Verify(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d entries verified\n", len(r.Manifest().Entries))
			return nil
		},
	}
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <file.csb>",
		Short: "Show what a backup contains",
		Long: `Show where and when a backup was taken, how many of each kind of object it
holds, and any warnings, such as objects deleted while the backup ran. Inspect
reads only the manifest; use verify to check the contents.`,
		Example: `Show what a backup holds.
cinc-server-backup inspect chef.csb`,
		Args: exactlyOneBackup,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, f, err := format.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }() // read-only
			return printManifest(cmd.OutOrStdout(), r.Manifest())
		},
	}
}

func printManifest(out io.Writer, m format.Manifest) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
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
