# cinc-server-backup

[![CI](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml?query=branch%3Amain)

Back up a Chef Infra Server or Cinc Server into a single file, and restore that file into a new server: another Chef or Cinc Server, or [cinc-server-ng](https://github.com/cinc-project/cinc-server-ng). Your nodes, cookbooks, users and permissions arrive intact, and the tool checks that they did.

It replaces knife-ec-backup. You'll want it when you need to:

- **keep restorable backups** of a Chef server;
- **move to a new server**, whether that's new hardware, a newer Chef Server, or cinc-server-ng;
- **pull one organization out** of a server, into a server of its own;
- **check a server against a backup** to see whether anything is missing or has changed.

> **Status:** everything below works today over the server's API. Still to come: carrying user passwords and server-admins across (they need access to the server's database), and deleting what a server holds that the backup doesn't. See [What isn't carried over yet](#what-isnt-carried-over-yet).

## Contents

- [Install](#install)
- [What you need first](#what-you-need-first)
- [Back up a server](#back-up-a-server)
- [Move to a new server](#move-to-a-new-server)
- [Restore one organization](#restore-one-organization)
- [Restore into a server that already has data](#restore-into-a-server-that-already-has-data)
- [Make a server match a backup again](#make-a-server-match-a-backup-again)
- [Preview a restore](#preview-a-restore)
- [Check a server against a backup](#check-a-server-against-a-backup)
- [See what changed between two backups](#see-what-changed-between-two-backups)
- [When a backup names someone who no longer exists](#when-a-backup-names-someone-who-no-longer-exists)
- [Reading the reports](#reading-the-reports)
- [What isn't carried over yet](#what-isnt-carried-over-yet)
- [Keeping backups safe](#keeping-backups-safe)

## Install

With Go 1.27 or newer:

```sh
go install github.com/cinc-project/cinc-server-backup/cmd/cinc-server-backup@latest
```

This puts `cinc-server-backup` in `$(go env GOPATH)/bin`. Every command explains itself with `--help`, and `cinc-server-backup completion <shell>` prints shell completion.

## What you need first

**A superuser's key for each server you talk to.** The tool reads every org, user and ACL, which only a superuser can do. On a Chef Infra Server that's `pivotal`, and its key is `/etc/opscode/pivotal.pem` on the server host. For cinc-server-ng, it's the key the server wrote with `--key-out`. The tool signs as `pivotal` unless you pass `--user`.

**Network access to the server's API** from wherever you run the tool. It doesn't have to run on the server itself.

**If your cookbook files live in S3** (an external bookshelf), the server hands out S3 links for them. For safety the tool only follows links to the server's own host, so allow the bucket's host with `--blob-host chef-bookshelf.s3.us-east-1.amazonaws.com` on `backup`, `restore` and `verify`.

## Back up a server

```sh
cinc-server-backup backup --server https://chef.example.com --key pivotal.pem -o chef-2026-11-12.csb
```

This captures everything a restore needs:
- users and their keys;
- organizations, their members and pending invitations;
- clients and their keys;
- groups and containers;
- nodes, roles, environments, and data bags with their items;
- every cookbook version and cookbook artifact, with their files;
- every policy revision and policy group;
- the permissions (ACLs) on all of it.

Objects are stored exactly as the server returned them, and each cookbook file is stored once, however many versions share it.

A few things worth knowing:

- **The file is only ever complete.** While the backup runs it writes `chef-2026-11-12.csb.partial`, and renames it when done. If the run is interrupted, you won't find a half-written backup under the real name.
- **It never overwrites.** Pick a new name each time, such as a date-stamped one.
- **Only you can read it** (mode 0600). It holds sensitive data; see [Keeping backups safe](#keeping-backups-safe).
- **It reads; it never writes** to the server. It keeps up to 8 requests in flight. Lower that with `--concurrency` if a busy server struggles, or raise it for a faster backup of a large one.

### Is the backup good?

Check it any time, for example before you rely on it, or after copying it somewhere:

```sh
cinc-server-backup verify chef-2026-11-12.csb
```
```
ok: 344 entries verified
```

`verify` checks every file inside the backup against the checksums recorded when it was written, so it catches corruption, truncation and tampering. `inspect` shows what the backup holds:

```sh
cinc-server-backup inspect chef-2026-11-12.csb
```
```
format  cinc-server-backup v1
source  https://chef.example.com
taken   2026-11-12 02:07:34Z to 2026-11-12 02:07:41Z (fuzzy)
counts
  acls               153
  cookbook_versions  8
  nodes              107
  users              3
  ...
```

"Fuzzy" means the server stayed in use while the backup ran. Each object is captured whole, but objects changed mid-run may be captured before or after the change. For a backup that must be exactly consistent, such as one for a migration, stop changes first (see below).

### Nightly backups

A cron job that keeps dated backups and stops if one fails its check:

```sh
#!/bin/sh
set -e
f=/backups/chef-$(date +%F).csb
cinc-server-backup backup --server https://chef.example.com --key /etc/opscode/pivotal.pem -o "$f"
cinc-server-backup verify "$f"
```

Delete old files on whatever schedule you keep. Each backup is self-contained, so you can delete any one without affecting the others.

## Move to a new server

The tool restores into an **empty** server: a fresh Chef or Cinc Server install, or a new cinc-server-ng. A walk-through:

**1. Stand up the new server** and get its superuser key. Don't create any organizations or users on it.

**2. Stop changes on the old server.** Tell people to hold off uploading cookbooks and editing roles, and if you can, pause chef-client runs, which update node data. Anything changed after the backup won't be on the new server.

**3. Back up the old server and check the file:**

```sh
cinc-server-backup backup --server https://old-chef.example.com --key old-pivotal.pem -o migration.csb
cinc-server-backup verify migration.csb
```

**4. Restore into the new server:**

```sh
cinc-server-backup restore migration.csb --server https://new-chef.example.com --key new-pivotal.pem
```

The restore:
1. checks the backup file;
2. makes sure the new server doesn't already have any of the organizations or users it's about to create;
3. writes everything in dependency order: users, orgs, clients, groups, then content, and permissions last;
4. backs up the new server and compares it with your backup, object by object.

It finishes with a fidelity report:

```
restored
  nodes              107
  cookbook_versions  8
  users              2
  ...

fidelity: migration.csb -> https://new-chef.example.com
  preserved            341
  regenerated          15  (expected: matched once server-generated values were set aside)
  skipped              3   (the signing superuser and per-user groups)
  dropped              0   (names the backup doesn't hold)
  differences          0
```

`differences 0` and exit status 0 mean the new server holds everything in the backup. [Reading the reports](#reading-the-reports) explains each line. The check at the end reads the whole new server again. On a very large server you can skip it with `--no-verify` and run it later with `verify --server`.

**5. Point your nodes and people at the new server.**

- **Nodes keep working.** Each client's public key was restored, so a node's existing `client.pem` authenticates against the new server. Change `chef_server_url` in the node's `client.rb` (or your DNS) and it carries on.
- **Bootstrapping keeps working.** Each organization's validator key was restored too, so your existing `validation.pem` files are still good.
- **knife keeps working.** Users keep their keys, so everyone's existing `.pem` works against the new server.
- **Web UI passwords don't carry over yet.** Restored users get a random password and need a new one set. On Chef Infra Server, use `chef-server-ctl password <user>`.

**6. Keep the old server until you're satisfied.** You can re-check the new server against the backup any time; see [Check a server against a backup](#check-a-server-against-a-backup).

## Restore one organization

To split a server up, or to recover one team's organization without touching the rest, restore just that org:

```sh
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --org acme
```
```
selected orgs: acme
with the users they name: alice, carol, dave
```

You don't have to work out which users to bring along. Everyone the org depends on comes with it:
- its members and invitees;
- anyone in one of its groups;
- anyone named in one of its permissions;
- anyone named in those users' own permissions.

Users who belong only to other organizations are left behind.

`--org a,b` takes several orgs. `--exclude-org` works the other way round: restore everything except the orgs you name. The check at the end compares only what you restored, so the orgs you left out aren't reported as missing.

The new server may already have other organizations. If it already has the org you're restoring or any of its users, for example a user shared with an org you restored earlier, add `--merge skip` (next section).

## Restore into a server that already has data

By default restore refuses a server that already has any of the organizations or users it's about to create, so it never changes something that's already there. `--merge skip` lifts that: it **keeps what the server has and adds what's missing**.

```sh
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --org beta --merge skip
```

Use it to:

- **restore a second organization** that shares users with one you restored earlier;
- **finish a restore that stopped partway**: run the same command again with `--merge skip`, and it adds only what didn't make it the first time.

What happens to each thing the server already has:

- **Users, organizations, clients, nodes, roles, environments, data bag items, cookbook versions and policy revisions** are left exactly as they are, keys and contents included, even if the backup's copy differs.
- **Group memberships and permissions** gain the backup's members and entries they don't have yet. Nothing is removed from them, so afterwards they grant what either the server or the backup granted.
- **Users the server already has** still join the organizations being restored.
- **A user or client that only shares a name** with one in the backup gets nothing. Chef knows actors by name, so restore checks that the server's actor holds exactly the backup's keys, no more and no fewer, and for a user, that it signs in through the same external (SSO/LDAP) identity. If it doesn't, it's someone else: restore leaves that name out of every membership and permission it writes, warns (`merge.different_user` or `merge.different_client`), and the check at the end lists what's missing. A password can't be compared, because the API doesn't expose it. So a same-named user created on the server with exactly the backup user's keys is trusted, and whatever password it was given still works. Only use `--merge skip` on a server whose existing users you trust.

The report lists what was skipped:

```
already on the target, left as it was
  roles   1
  users   1
```

The check at the end still compares everything against the backup. So if the server holds something differently from the backup, such as a role someone edited, it shows up under differences and the run exits 1. That's how you find out what `--merge skip` didn't change.

## Make a server match a backup again

`--merge skip` never changes what's already there. `--merge overwrite` does: **everything in the backup that the server holds differently is made to match the backup.** Use it to bring a server restored earlier back in line, for example a final sync from the old server just before you switch over:

```sh
cinc-server-backup restore final.csb --server https://new-chef.example.com --key pivotal.pem --merge overwrite --plan
cinc-server-backup restore final.csb --server https://new-chef.example.com --key pivotal.pem --merge overwrite
```

- **Users' and clients' details, organizations, nodes, roles, environments, data bag items and cookbook versions** that differ are replaced with the backup's. Each is compared the way `verify` compares, so anything that already matches isn't written.
- **Group memberships, permissions and policy group assignments** are set to exactly the backup's. Members, entries and assignments the backup doesn't have are removed.
- **Frozen cookbook versions** are kept, with a `merge.frozen_kept` warning, unless you add `--force-frozen`. Even then, a server never unfreezes a version, so it stays frozen with the backup's content (`merge.stays_frozen`).
- **What's left alone:**
  - objects the backup doesn't have (deleting those is still to come);
  - cookbook artifacts and policy revisions, which can't change under their identifier;
  - users' keys and passwords;
  - a same-named user or client that isn't the backup's (see above).

The report lists what was overwritten. Run `--plan` first: it shows exactly what would be overwritten, as the run itself would decide it.

## Preview a restore

Add `--plan` to any `restore` to see what it would do, without changing anything on the server:

```sh
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --plan
```
```
plan: would restore
  acls               156
  cookbook_versions  8
  files (at most)    16
  nodes              107
  users              2
  ...

nothing was changed; 936 writes planned
```

With `--merge skip`, a "would leave as it is (already on the target)" section lists what the server already has.

The plan reads the server to make the same decisions the restore would make, so it's accurate for the server as it is now:
- what it would restore, and what it would leave as it is;
- names it would leave out (see [above](#when-a-backup-names-someone-who-no-longer-exists));
- same-named users and clients it wouldn't trust.

`files (at most)` is an upper bound: only the server can say which cookbook files it already has, and finding out means starting an upload.

If the restore would refuse to start (the server already has an org without `--merge skip`, or `--dangling fail` finds names to drop), the plan still shows what it would do, lists every reason after it, and exits 1.

`--plan-out plan.json` also writes the plan as JSON, including every request the restore would send, for review or a change ticket.

## Check a server against a backup

`verify --server` backs a server up into a temporary file, compares it with a backup, and prints the same fidelity report that `restore` ends with. The temporary file is readable only by you and deleted when the check finishes. Use it to:

- **re-check a restore later**, for example after a few days in service:
  ```sh
  cinc-server-backup verify migration.csb --server https://new-chef.example.com --key pivotal.pem
  ```
- **confirm a backup captured everything** right after taking it, by checking it against the server it came from. If something changed during the backup, you'll see it here.
- **check part of a server** with `--org` or `--exclude-org`, as for `restore`.

It only reports what the backup holds that the server lacks or holds differently. Anything created on the server since the backup isn't listed.

## See what changed between two backups

`diff` compares two backup files, with no server involved:

```sh
cinc-server-backup diff monday.csb tuesday.csb
```
```
changed  orgs/acme/roles/web.json
  before {"name":"web","run_list":["recipe[nginx]"],...}
  after  {"name":"web","run_list":["recipe[nginx]","recipe[monitoring]"],...}
missing  orgs/acme/nodes/old-db-01.json
2 differences
```

It lists what the first backup holds that the second lacks (`missing`) or holds differently (`changed`), and exits 1 if there's anything. Things the second backup has that the first doesn't aren't listed, so to see additions too, run it the other way round as well.

It ignores what a server generates on its own (org IDs, cookbook file links, per-user group names, the order of lists), so two backups of the same content compare equal even when they come from different servers. That makes `diff old-server.csb new-server.csb` another way to check a migration.

Each server's superuser has its own key, so `pivotal` is left out by default. `--skip-user` changes that.

## When a backup names someone who no longer exists

Chef Infra Server leaves a deleted user or client in the permissions and groups that named them. A backup of such a server names someone it doesn't contain. A server refuses permissions that name an unknown user, so restoring those as they are would fail.

By default, restore **leaves those names out**, restores everything else in those permissions and groups, and lists every name it dropped:

```
left out (2)
  acl.dangling_dropped    orgs/acme/acls/nodes/web01.json  update: user former-employee
  group.dangling_dropped  orgs/acme/groups/admins.json     user former-employee
```

The run then exits with status **3**: everything was restored except those names. Since the names refer to users and clients that no longer exist, dropping them doesn't take anyone's access away.

To review them before anything is written, use `--dangling fail`. Restore lists every such name and stops without writing anything:

```sh
cinc-server-backup restore chef.csb --server https://new-chef.example.com --key pivotal.pem --dangling fail
```

## Reading the reports

### The fidelity report

`restore` ends with it and `verify --server` prints it.

| Line | What it means |
|---|---|
| **preserved** | Objects the server holds just as the backup does, counted by kind |
| **regenerated** | How many of those match only once values the server generates on its own are set aside: organization IDs, cookbook file links, per-user group names, the order of lists. Expected, and nothing to fix |
| **skipped** | Not compared: the superuser you signed as (each server has its own) and per-user groups (named after IDs each server assigns) |
| **dropped** | Names the backup refers to but doesn't contain, which the server lacks too. See [above](#when-a-backup-names-someone-who-no-longer-exists) |
| **differences** | Anything else the server lacks or holds differently, each listed with the backup's version and the server's. This should be 0 |

### Exit status

| Status | Meaning |
|---|---|
| 0 | Success. For `restore` and `verify --server`: the server holds everything in the backup. For `diff`: no differences |
| 1 | Something failed, or something differs |
| 2 | The command line was wrong; the message says how |
| 3 | `restore` / `verify --server`: everything is there except dropped names |

In scripts, treat 3 as success if dropped names are acceptable to you, and review the listed names.

## What isn't carried over yet

These need access to the server's database, not just its API, and are planned next:

- **Web UI passwords.** Restored users get a random password; set new ones after restoring.
- **Server-admins**, the users allowed to manage other users.
- **Creation and update times** of objects. The new server records its own.

Also planned:

- **Deleting what the backup doesn't have** (`--prune`). `--merge overwrite` makes everything in the backup match, but leaves anything else on the server alone.
- **Resuming a restore** from where it stopped, without re-reading the server. `--merge skip` already finishes a stopped restore; a journal would make that faster.

## Keeping backups safe

A backup is a complete copy of your Chef server's data. Treat it like the server itself:

- **It contains data bag items as stored.** Unencrypted data bags hold their secrets in plain text. Encrypted data bags stay encrypted.
- **It contains node attributes**, which often include hostnames, IP addresses and configuration details.
- **It contains every public key**, but no private keys: Chef servers keep only the public half.
- **It contains no passwords.**

Files are created readable only by you. Keep them that way, store them encrypted at rest, and limit who can read your backup location.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build and test, and the [design document](docs/specs/2026-11-11-cinc-server-backup-design.md) for how the tool works and where it's going.

## License

Apache-2.0. See [LICENSE](LICENSE).
