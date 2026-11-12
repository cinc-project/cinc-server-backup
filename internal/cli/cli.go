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
	"github.com/spf13/pflag"

	"github.com/cinc-project/cinc-server-backup/capture"
	"github.com/cinc-project/cinc-server-backup/compare"
	"github.com/cinc-project/cinc-server-backup/format"
	"github.com/cinc-project/cinc-server-backup/internal/refs"
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
// the process exit code: 0 success, 1 failure, 2 usage error, 3 when verify
// or restore finds a server holding everything but the dangling names it
// reports.
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
	if errors.Is(err, errDifferent) {
		return 1 // the command printed what differs
	}
	if errors.Is(err, errDropped) {
		return 3 // verify printed what was dropped
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
	root.AddCommand(newBackupCmd(), newRestoreCmd(), newInspectCmd(), newVerifyCmd(), newDiffCmd(), newVersionCmd())
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
	dangling          string
	noVerify          bool
	orgs, excludeOrgs []string
	merge             string
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
over the API: restored users get a random one until they reset it.

An ACL entry or group member can name a user, client or group the backup
doesn't hold, such as a user deleted after being granted access to a node.
A server refuses a whole ACL that names one, so by default (--dangling=drop)
restore leaves those names out and lists each one. --dangling=fail lists
them and stops before writing anything.

--merge skip restores into a server that already holds some of the backup,
such as a second organization sharing users with one restored earlier, or a
rerun after a restore stopped partway. Users, orgs, clients, nodes, roles,
environments, data bag items, cookbook versions and policy revisions the
target already has are left exactly as they are, and listed as skipped. Groups
and permissions the target already has gain the backup's members and entries
they lack; none are removed. The check at the end then reports anything the
target holds differently from the backup.

--org restores only the organizations you name, and --exclude-org leaves
some out. The users those organizations name come along: members, invitees,
group members, ACL entries, and the users named in those users' own ACLs.

Once everything is written, restore checks the result as verify --server
does: it backs the target up into a temporary file, compares it with the
backup and prints the fidelity report. --no-verify skips that.

Restore exits 0 when the target holds everything in the backup, 3 when it
holds everything but the dangling names it left out, and 1 on any failure
or difference.`,
		Example: `Restore a backup into a new, empty server.
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem

Restore one organization, and the users it names, out of a whole-server backup.
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --org acme

Restore a second organization into a server that already has users it shares.
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --org beta --merge skip

Stop instead if any ACL or group names a user, client or group the backup lacks.
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --dangling fail`,
		Args: exactlyOneBackup,
		RunE: func(cmd *cobra.Command, args []string) error { return runRestore(cmd, args[0], f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "URL of the server to restore into (required)")
	fl.StringVar(&f.user, "user", "pivotal", "superuser to sign requests as")
	fl.StringVar(&f.key, "key", "", "path to that superuser's private key (required)")
	fl.IntVar(&f.concurrency, "concurrency", 8, "most requests to have in flight at once")
	fl.StringArrayVar(&f.blobHosts, "blob-host", nil, "extra host[:port] file uploads may be redirected to, such as an S3 bookshelf (repeatable)")
	fl.StringVar(&f.dangling, "dangling", string(restore.DanglingDrop), "what to do with ACL entries and group members the backup can't resolve: drop or fail")
	fl.BoolVar(&f.noVerify, "no-verify", false, "don't back the target up and compare it with the backup afterwards")
	fl.StringVar(&f.merge, "merge", "", "restore into a server that already holds some of the backup: skip keeps what it has and adds what's missing")
	orgFlags(fl, &f.orgs, &f.excludeOrgs)
	return cmd
}

func runRestore(cmd *cobra.Command, file string, f restoreFlags) error {
	dangling, err := restore.ParseDangling(f.dangling)
	if err != nil {
		return usagef("%v", err)
	}
	merge, err := restore.ParseMerge(f.merge)
	if err != nil {
		return usagef("%v", err)
	}
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
	out := cmd.OutOrStdout()
	sel, err := selection(out, r, f.orgs, f.excludeOrgs)
	if err != nil {
		return err
	}
	c, err := newClient(f.server, f.user, f.key, f.blobHosts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	rep, err := restore.ToAPI(ctx, r, c, restore.Options{
		Concurrency: f.concurrency, Dangling: dangling, Orgs: sel.Orgs, Merge: merge,
	})
	if rep != nil {
		printReport(out, rep)
	}
	if err != nil {
		return err
	}
	if f.noVerify {
		return nil
	}
	fmt.Fprintln(out)
	return checkServer(ctx, out, r, sel, file, f.server, f.user, c, capture.Options{Concurrency: f.concurrency, BlobHosts: f.blobHosts})
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
	if len(rep.Skipped) > 0 {
		fmt.Fprintln(tw, "\nalready on the target, left as it was")
		skipped := make([]string, 0, len(rep.Skipped))
		for k := range rep.Skipped {
			skipped = append(skipped, k)
		}
		sort.Strings(skipped)
		for _, k := range skipped {
			fmt.Fprintf(tw, "  %s\t%d\n", k, rep.Skipped[k])
		}
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprintf(tw, "\nleft out (%d)\n", len(rep.Warnings))
		for _, w := range rep.Warnings {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", w.Code, w.Path, w.Detail)
		}
	}
	_ = tw.Flush()
}

// errDifferent is diff's "the backups differ" result: exit 1 with no error
// message, as diff(1) does.
var errDifferent = errors.New("backups differ")

func newDiffCmd() *cobra.Command {
	var skipUsers []string
	cmd := &cobra.Command{
		Use:   "diff <before.csb> <after.csb>",
		Short: "Show what one backup holds that another lacks or holds differently",
		Long: `Compare two backups object by object and list everything in the first that
is missing from the second or differs there. Values a server regenerates on
its own are set aside: organization GUIDs, cookbook file URLs, the URLs in
listings, per-user group names, and the order of members and ACL entries.
Objects only in the second backup are not reported. A name in the first
backup's ACLs or groups that the backup doesn't hold (such as a deleted user)
and the second lacks is listed as dropped, not as a difference: restore
leaves such names out.

Use it to check a migration: back up the old server, restore into the new
one, back up the new one, and diff the two. Each server's own superuser has
its own key, so pivotal is skipped unless you say otherwise with --skip-user.

Diff exits 0 when nothing differs and 1 when something does.`,
		Example: `Check that a restore carried everything over.
cinc-server-backup diff old-server.csb new-server.csb`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usagef("diff needs two backup files, and we got %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, fa, err := format.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = fa.Close() }() // read-only
			b, fb, err := format.Open(args[1])
			if err != nil {
				return err
			}
			defer func() { _ = fb.Close() }() // read-only
			var skip []string
			for _, u := range skipUsers {
				if u != "" {
					skip = append(skip, u)
				}
			}
			res, err := compare.Compare(a, b, compare.Options{SkipUsers: skip})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, d := range res.Dropped {
				fmt.Fprintf(out, "%-8s %s %s\n", "dropped", d.Path, d.Detail)
			}
			diffs := res.Differences
			if len(diffs) == 0 {
				fmt.Fprintln(out, "No differences.")
				return nil
			}
			printDifferences(out, diffs)
			fmt.Fprintf(out, "%d differences\n", len(diffs))
			return errDifferent
		},
	}
	cmd.Flags().StringArrayVar(&skipUsers, "skip-user", []string{"pivotal"}, "user to leave out of the comparison, such as each server's superuser (repeatable; pass an empty value to skip nobody)")
	return cmd
}

type verifyFlags struct {
	server, user, key string
	concurrency       int
	blobHosts         []string
	orgs, excludeOrgs []string
}

func newVerifyCmd() *cobra.Command {
	var f verifyFlags
	cmd := &cobra.Command{
		Use:   "verify <file.csb> [--server <url> --key <pem>]",
		Short: "Check that a backup is intact, or that a server holds what it does",
		Long: `Check every file in a backup against the checksums in its manifest. Verify
fails if anything was changed, removed or added since the backup was written.

With --server, verify then backs that server up into a temporary file and
compares it with the backup, object by object, as diff does, and prints a
fidelity report:

  preserved    what the server holds as the backup does, by kind
  regenerated  how many of those matched only once the values a server
               regenerates were set aside (org GUIDs, file URLs, per-user
               group names, member order)
  skipped      the superuser verify signs as, and per-user groups
  dropped      names in the backup's ACLs and groups that the backup itself
               doesn't hold (such as a deleted user) and the server lacks,
               as restore leaves them out
  differences  everything else the server lacks or holds differently

Use it after a restore to check the target, right after a backup to check
the source, or any time later to see whether a server has drifted. The
temporary file is readable by you alone and removed when verify finishes.

Verify exits 0 when the server holds everything, 3 when it holds everything
but the dropped names, and 1 when anything differs.`,
		Example: `Check a backup before relying on it.
cinc-server-backup verify chef.csb

Check that a restore carried everything over.
cinc-server-backup verify chef.csb --server https://new-chef.example.com --key pivotal.pem`,
		Args: exactlyOneBackup,
		RunE: func(cmd *cobra.Command, args []string) error { return runVerify(cmd, args[0], f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.server, "server", "", "URL of a server to compare with the backup")
	fl.StringVar(&f.user, "user", "pivotal", "superuser to sign requests as; left out of the comparison")
	fl.StringVar(&f.key, "key", "", "path to that superuser's private key (required with --server)")
	fl.IntVar(&f.concurrency, "concurrency", 8, "most requests to have in flight at once")
	fl.StringArrayVar(&f.blobHosts, "blob-host", nil, "extra host[:port] cookbook files may come from, such as an S3 bookshelf (repeatable)")
	orgFlags(fl, &f.orgs, &f.excludeOrgs)
	return cmd
}

// orgFlags adds --org and --exclude-org, which pick part of a backup.
func orgFlags(fl *pflag.FlagSet, orgs, exclude *[]string) {
	fl.StringSliceVar(orgs, "org", nil, "only these organizations, and the users they name (comma-separated or repeated)")
	fl.StringSliceVar(exclude, "exclude-org", nil, "leave these organizations out (comma-separated or repeated)")
}

// selection picks the part of r --org and --exclude-org name, and says
// which orgs and users that is when it is not everything.
func selection(out io.Writer, r *format.Reader, orgs, exclude []string) (*refs.Selection, error) {
	sel, err := refs.Select(r, orgs, exclude)
	if err != nil {
		return nil, usageError{err}
	}
	if !sel.All {
		fmt.Fprintf(out, "selected orgs: %s\nwith the users they name: %s\n", strings.Join(sel.Orgs, ", "), strings.Join(sel.Users, ", "))
	}
	return sel, nil
}

// errDropped is verify's "everything but the dangling names" result: exit 3
// with no error message.
var errDropped = errors.New("dangling names dropped")

func runVerify(cmd *cobra.Command, file string, f verifyFlags) error {
	if f.server == "" && f.key != "" {
		return usagef("--key needs --server")
	}
	if f.server != "" && f.key == "" {
		return usagef("verify --server needs --key")
	}
	r, fh, err := format.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }() // read-only
	if err := r.Verify(); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "ok: %d entries verified\n", len(r.Manifest().Entries))
	if f.server == "" {
		if len(f.orgs) > 0 || len(f.excludeOrgs) > 0 {
			return usagef("--org and --exclude-org need --server")
		}
		return nil
	}
	sel, err := selection(out, r, f.orgs, f.excludeOrgs)
	if err != nil {
		return err
	}

	opts := capture.Options{Concurrency: f.concurrency, BlobHosts: f.blobHosts}
	c, err := newClient(f.server, f.user, f.key, opts.BlobHosts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	return checkServer(ctx, out, r, sel, file, f.server, f.user, c, opts)
}

// checkServer backs up the server c talks to into a temporary file (0600,
// removed afterwards), compares it with the backup r read from file, leaving
// out the superuser c signs as and anything outside sel, and prints the
// fidelity report. It returns
// errDifferent if anything differs, errDropped if only dangling names are
// missing, and nil if the server holds everything.
func checkServer(ctx context.Context, out io.Writer, r *format.Reader, sel *refs.Selection, file, server, user string, c *cinc.Client, opts capture.Options) error {
	tmp, err := os.CreateTemp("", "csb-verify-*.csb") // created 0600
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	fmt.Fprintf(out, "backing up %s to compare\n", server)
	w := format.NewWriter(tmp, format.Source{URL: server})
	w.SetTool(progName + " " + Version)
	if err := capture.FromAPI(ctx, c, w, opts); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	st, err := tmp.Stat()
	if err != nil {
		return err
	}
	live, err := format.NewReader(tmp, st.Size())
	if err != nil {
		return err
	}
	res, err := compare.Compare(r, live, compare.Options{SkipUsers: []string{user}, Include: sel.Includes})
	if err != nil {
		return err
	}
	printFidelity(out, file, server, res)
	switch {
	case len(res.Differences) > 0:
		return errDifferent
	case len(res.Dropped) > 0:
		return errDropped
	}
	return nil
}

// printFidelity prints verify's report of how a server matches a backup.
func printFidelity(out io.Writer, file, server string, res *compare.Result) {
	fmt.Fprintf(out, "\nfidelity: %s -> %s\n", file, server)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	kinds := make([]string, 0, len(res.Preserved))
	total := 0
	for k, n := range res.Preserved {
		kinds = append(kinds, k)
		total += n
	}
	sort.Strings(kinds)
	fmt.Fprintf(tw, "  preserved\t%d\n", total)
	for _, k := range kinds {
		fmt.Fprintf(tw, "    %s\t%d\n", k, res.Preserved[k])
	}
	fmt.Fprintf(tw, "  regenerated\t%d\t(expected: matched once server-generated values were set aside)\n", res.Regenerated)
	fmt.Fprintf(tw, "  skipped\t%d\t(the signing superuser and per-user groups)\n", res.Skipped)
	fmt.Fprintf(tw, "  dropped\t%d\t(names the backup doesn't hold)\n", len(res.Dropped))
	for _, d := range res.Dropped {
		fmt.Fprintf(tw, "    %s\t%s\n", d.Path, d.Detail)
	}
	fmt.Fprintf(tw, "  differences\t%d\n", len(res.Differences))
	_ = tw.Flush()
	printDifferences(out, res.Differences)
}

// printDifferences lists differences as diff prints them.
func printDifferences(out io.Writer, diffs []compare.Difference) {
	for _, d := range diffs {
		fmt.Fprintf(out, "%-8s %s\n", d.Kind, d.Path)
		if d.Kind == compare.Changed {
			fmt.Fprintf(out, "  before %s\n  after  %s\n", strings.TrimSpace(d.Before), strings.TrimSpace(d.After))
		}
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
