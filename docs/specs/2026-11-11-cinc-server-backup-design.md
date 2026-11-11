# cinc-server-backup: design

Status: draft · 2026-11-11

## 1. Summary

`cinc-server-backup` takes a backup of an **entire** Chef Infra Server / Cinc Server and stores it in one portable, verifiable, optionally encrypted file. It restores that same file into:

- a legacy Chef Infra Server or Cinc Server (disaster recovery, rebuilds, server-to-server moves), or
- cinc-server-ng (migration).

It replaces knife-ec-backup. It keeps that tool's good ideas (a knife-shaped layout, phase ordering, skipping files already present) and fixes what makes it painful:

- it loses data silently;
- it runs serially and breaks under concurrency;
- it has no format version;
- it has no retries, no resume and no verification;
- it is tied to whatever Postgres schema the backup ran against.

There are four design principles:

1. **Capture exact bytes.** Every object is stored as the bytes the server returned, never decoded into a struct and re-encoded. Whatever we don't understand is still saved.
2. **Get to the lowest layer only when it adds data.** The HTTP API is the default and works remotely against any server. A *host tier*, used only when the tool runs on the Chef Server host, adds what the API cannot express: password hashes, server-admins, timestamps and OAuth apps. Each fact is labelled with the tier it came from, so restore knows how much fidelity it has.
3. **Restore is plan, then apply, then verify.** Restore computes a dependency-ordered plan against the live target, applies it in parallel and idempotently with a journal it can resume from, and then backs up the target again and compares the result with the backup. Every run ends with a **fidelity report** listing what was preserved, what the target regenerated, and what was dropped, with the reason for each.
4. **One format, several engines.** The format is a public, versioned Go package. cinc-server-ng imports it to offer a native offline restore straight into SQLite.

The seamless case is:

```sh
# on the old Chef Server host: zero flags, full fidelity
sudo cinc-server-backup backup -o chef-2026-11-11.csb --recipient age1...

# anywhere: into a fresh legacy server or into cinc-server-ng
cinc-server-backup restore chef-2026-11-11.csb --server https://new.example.com --key pivotal.pem
```

## 2. Goals and non-goals

**Goals**

- Back up everything a Chef Server holds that is not derived:
  - users, orgs, membership, invitations
  - clients and every key
  - groups, containers, every ACL
  - nodes, roles, environments, data bags
  - every cookbook version, frozen state included, with its file contents
  - cookbook artifacts and every policy revision
  - policy group assignments
  - server-admins, password hashes, OAuth apps
- Restore it into a legacy server so that existing nodes, `knife` users and web logins keep working with no reconfiguration other than the server URL.
- Restore the same file into cinc-server-ng with the same guarantee.
- Never lose data silently. Anything not restored is named in the fidelity report and makes the run exit non-zero, unless the operator accepted that category in advance.
- Handle large installs: tens of thousands of nodes and hundreds of GB of cookbook files, finishing in minutes, not hours.
- Resume after a crash or interrupted run, and allow re-runs without causing damage.

**Non-goals**

- Backing up derived state: search indexes, bookshelf's own layout, Solr/OpenSearch, erchef caches. Targets rebuild these.
- Translating `chef-server.rb`. We capture a redacted copy for reference only.
- Recovering client or user **private** keys. The server never had them.
- Preserving internal ids (org GUIDs, authz ids, user ids) on a legacy target. erchef generates these, and nothing a client sees depends on them (§4).
- Being a generic Chef API sync tool. Continuous replication may come later; this design keeps the door open (§13) but does not build it.

## 3. What knife-ec-backup gets wrong

Each point below comes from reading its source (it is mostly a wrapper around ChefFS):

| Problem | Evidence | What we do instead |
|---|---|---|
| No format version or manifest | issue #107 | Versioned manifest with a checksum per entry (§6) |
| Users fetched serially, orgs serially, each top-level directory serially | `ec_backup.rb:49-90, 216-218` | One global work queue with adaptive concurrency (§7.3) |
| Breaks under concurrency; the README tells users to fall back to `--concurrency 1` | #81, #136, #176 | Concurrency that backs off on 5xx/latency (AIMD) |
| No retries; errors are logged to a side file and the exit code is 0 | #19; `ec_error_handler.rb:56-62` | Retries with budgets; any unexpected failure gives a non-zero exit and appears in the fidelity report |
| Every cookbook version is a full copy; no deduplication across versions or orgs | ChefFS `cookbook_file.rb` | Blob store addressed by checksum (§6.3) |
| Signs at API v0, so only the `default` key is visible; extra or rotated keys need `--with-key-sql` | #16, #117 | API v1 keys endpoints capture every key, name and expiry, with no SQL |
| `--with-user-sql` writes a **whole** `users` row back with `UPDATE … SET *` and drops it when the ids differ | `ec_key_import.rb:166-207`, #148 | Write only the columns we mean to (`hashed_password`, `salt`, `hash_type`, `admin`, ...), matched by username |
| server-admins not backed up | #95 | Host tier reads it, and restore writes it (§8.6) |
| ACL restore fails as a whole when one actor is missing | #83 | Preflight resolves every reference; dangling entries are dropped and reported, never fatal by surprise (§8.4) |
| Clients in `admins` break restore; a clients-group mismatch blocks restore | #22, #157 | Clients are restored before any group membership, so the three-step admin-group dance isn't needed |
| Re-freezes cookbooks with a second PUT after upload; frozen-cookbook 409s on re-run | #82, `ec_restore.rb:383-436` | The first manifest PUT carries `frozen?: true` (erchef honours it on create); on a re-run, an identical manifest is skipped |
| `status.json` inside the cookbook dir probably uploads as a root file *(inferred, not run)* | ChefFS `cookbook_dir.rb:135-147` | Frozen state lives in the manifest we store, not in a sidecar file |
| Timestamps, OAuth apps and key `created_at` lost | `ec_key_import.rb` uses `Time.now` | Host tier captures them; restore writes them when it can (§8.6) |

## 4. What can move through the API, and what can't

From reading erchef, bifrost and chef-server-ctl in `~/dev/oss/cinc-server/src`. Paths are relative to `oc_erchef/apps/` unless stated.

| Data | Readable over API | Writable exactly over API | Otherwise |
|---|---|---|---|
| User profile, display fields | yes | yes | |
| User password hash, salt, `hash_type` | **no** (`chef_user.erl:260-273`) | **no**: the API only takes plaintext and always re-hashes (`chef_password.erl:45-51`) | `users` table |
| User `admin` column | no | no (always inserted as `false`) | `users` table |
| `external_authentication_uid`, `recovery_authentication_enabled` | **no** (not stored in `serialized_object`) | yes, superuser PUT (`oc_chef_wm_named_user.erl:134-143`) | read from `users` table |
| Public keys: every key with its name and expiry (users and clients) | yes, API v1 keys endpoints | yes: POST with `public_key`, `PUT …/keys/default` (`chef_key.erl:75-95`) | key `created_at`: `keys` table |
| Client `validator` | yes | yes | |
| Client `admin` | no | only at API v0 | `clients` table |
| Org name, full_name | yes | yes | |
| Org GUID | yes | **no** (`make_guid()`) | not preservable on legacy; nothing depends on it |
| Org membership | yes | yes (superuser `POST /organizations/O/users`) | |
| Invitations | yes | recreated, ids regenerated | |
| Groups incl. nested | yes (`users`/`clients`/`groups`) | yes, split-form PUT. **Unknown names are silently dropped** | verify pass catches it |
| USAGs (per-user groups named by 32-hex user id) | yes, listed like any group | recreated automatically when the user is associated, under a **new** id | remap (§8.4) |
| server-admins (global group) | **no route** | **no route** | `groups` table + bifrost (`chef-server-ctl/plugins/server_admins.rb:36-50,151`) |
| ACLs: 11 object types, org, user | yes, `?detail=granular` splits users from clients | yes, PUT per permission replaces it. Missing actor gives 400 for the **whole** permission. Mixed user/client name gives 422 unless split form | |
| Nodes, roles, envs, data bags | yes | yes | |
| Cookbook versions | yes (every version, `num_versions=all`) | yes. `frozen?` honoured on first PUT, `?force=true` to overwrite a frozen version (`chef_wm_cookbook_version.erl:255-268`) | |
| Cookbook file contents | yes, presigned bookshelf URLs | yes, through sandboxes. `needs_upload:false` when the org already has the checksum | |
| Cookbook artifacts | yes | yes (409 when already present) | |
| Policy revisions (including ones no group uses) | yes | yes: `POST /policies/P/revisions` (`oc_chef_wm_named_policy_revisions.erl:65`) | |
| Policy group assignments | yes | yes: `PUT /policy_groups/G/policies/P` | |
| `created_at`/`updated_at` | **no** handler returns them | no | object tables |
| OAuth (oc-id) apps, grants, tokens | no | no | oc_id DB (`oc-id/db/schema.rb:17-52`) |
| Server secrets (pivotal.pem, webui key, `private-chef-secrets.json`, TLS) | no | no | files under `/etc/opscode` |

Three conclusions shape the design:

- **Over the API, keys are lossless at v1.** knife-ec-backup needed SQL for keys only because it signs at v0.
- **Restores must be built around a few facts:** groups drop unknown names silently, ACL PUTs fail as a whole, and USAGs are re-keyed. These drive the restore ordering and the verify pass.
- **Exactly five things need the host tier:** password hashes and the admin flags, server-admins, external-auth fields, timestamps, and OAuth/secrets. Everything else, the bulk of the data, works remotely.

## 5. Architecture

```
                 ┌───────────────────────── cinc-server-backup ─────────────────────────┐
                 │                                                                      │
 legacy server ──┤  capture engines             format (public Go pkg)    restore drivers ├── legacy server
 cinc-server-ng ─┤   ├─ api   (cinc-api)  ───▶   .csb writer/reader  ───▶  ├─ chef-server ├── cinc-server-ng
                 │   └─ host  (pg, bifrost,     manifest · objects ·       │   (API + host)│
                 │            /etc/opscode)     blobs · host/ · journal    └─ ng (API)     │
                 │                                                                      │
                 │  planner (DAG) · executor (AIMD pool, journal) · verifier · fidelity   │
                 └──────────────────────────────────────────────────────────────────────┘
                                              ▲
                     cinc-server-ng imports format/ for `cinc-server-ng restore --db` (native)
```

Go module `github.com/cinc-project/cinc-server-backup`:

| Package | Responsibility |
|---|---|
| `format` | **Public, stable.** Reads, writes and validates `.csb`. Has no dependency on cinc-api, so cinc-server-ng can import it cheaply |
| `capture/api` | Crawls a server over cinc-api and emits format entries |
| `capture/host` | Reads Postgres, bifrost and `/etc/opscode` on the Chef Server host (read-only) |
| `plan` | Builds the restore DAG from a backup plus a snapshot of the target |
| `exec` | Bounded, adaptive worker pool; retries; journal |
| `target/chefserver`, `target/ng` | Target drivers: capability discovery, API quirks, host-tier writers |
| `verify` | Comparison that understands each object type; produces the fidelity report |
| `cmd/cinc-server-backup` | CLI |

All HTTP goes through cinc-api. §11 lists the changes cinc-api needs.

## 6. Backup format (`.csb`)

### 6.1 Container

A **zip** archive with the extension `.csb`:

- It has random access (a central directory), which the restore planner needs.
- It can be written as a stream.
- Each entry is compressed separately. Blobs that are already compressed are stored as-is.
- Go's stdlib reads it, and every operator can open it with ordinary tools.

Encryption wraps the whole file in an [age](https://age-encryption.org) stream (`.csb.age`). Restore decrypts to a spool file in the temp dir (mode 0600) before opening the zip. Encrypting each entry separately would leak names and sizes; whole-file encryption doesn't.

`cinc-server-backup extract` unpacks a backup to a directory tree and `pack` builds one from a tree, so people can `diff -r` backups, `grep` them and edit them by hand (one thing knife-ec-backup does well).

### 6.2 Layout

```
manifest.json                       format version, source, tiers, time window, counts, entry index
server/
  users/<name>.json                 raw GET /users/<name> (API v1)
  users/<name>/keys/<key>.json      raw GET /users/<name>/keys/<key>
  users/<name>/acl.json             raw GET /users/<name>/_acl?detail=granular
orgs/<org>/
  org.json  members.json  invitations.json
  acl.json                          /organizations/<org>/organizations/_acl
  clients/<name>.json  clients/<name>/keys/<key>.json
  groups/<name>.json  containers/<name>.json
  nodes/ roles/ environments/       <name>.json
  data_bags/<bag>.json              bag-level object + ACL
  data_bags/<bag>/<item>.json
  cookbooks/<name>/<version>.json   raw manifest, includes frozen?
  cookbook_artifacts/<name>/<identifier>.json
  policies/<name>/<revision>.json
  policy_groups/<name>.json         {name, policies:{P: revision_id}}
  acls/<type>/<name>.json           11 types, granular form
blobs/<md5[0:2]>/<md5>              file contents, stored once per backup
host/                               present only when the host tier ran
  users.json                        hash, salt, hash_type, admin, external_auth, recovery, timestamps
  clients.json                      admin flag, timestamps
  server_admins.json                usernames
  timestamps/<org>.json             per-object created_at/updated_at
  oauth.json                        oc-id applications (secrets → secrets/)
  config/chef-server-running.json   redacted, for reference only
secrets/                            only with --with-server-secrets; refuses to write unless encrypted
  pivotal.pem  webui_priv.pem  webui_pub.pem  private-chef-secrets.json  tls/
```

Object files hold the **exact response bytes**. Each manifest entry records:

- `path`, `sha256`, `size`
- `source`: `api` or `host`
- `fetched_at` (RFC 3339 with milliseconds)
- `api_version`
- for API objects, the HTTP `ETag`/`Last-Modified` when present

### 6.3 Blobs

Cookbook and artifact files are stored once per backup, keyed by their Chef checksum (MD5). Each blob is also given a SHA-256 in the manifest, so integrity doesn't depend on MD5.

Blobs are shared across cookbook versions **and orgs**, even though erchef stores checksums per org. Deduplication happens only in the backup. Restore uploads each blob into every org that references it.

### 6.4 Manifest

```json
{
  "format": "cinc-server-backup", "version": 1,
  "tool": "cinc-server-backup 0.1.0",
  "source": { "url": "https://chef.example.com", "flavor": "chef-server", "version": "15.10.127",
              "api_versions": [0, 2] },
  "tiers": { "api": true, "host": { "postgres": true, "bifrost": true, "oauth": true, "secrets": false } },
  "window": { "started_at": "...", "finished_at": "..." },
  "consistency": "fuzzy" ,
  "counts": { "orgs": 3, "users": 412, "nodes": 18311, "blobs": 90211, "...": 0 },
  "warnings": [ { "code": "acl.dangling_actor", "path": "orgs/acme/acls/nodes/web01.json", "detail": "..." } ],
  "entries": [ { "path": "...", "sha256": "...", "size": 0, "source": "api", "fetched_at": "..." } ]
}
```

`consistency` is `fuzzy` for an API crawl and `snapshot` when the host tier read Postgres inside one transaction (§7.4). Readers reject a major `version` they don't know. They ignore unknown fields and unknown paths.

## 7. Backup

### 7.1 Credentials and discovery

The tool authenticates as a superuser. It looks for credentials in this order:

1. `--user`/`--key`
2. `/etc/opscode/pivotal.pem` when run on the host
3. The webui-key impersonation knife-ec-backup uses (`X-Ops-Request-Source: web`), kept only as a fallback

It finds the server version with `GET /_status` and `/server_api_version`, and recognises cinc-server-ng from its status payload. It signs at the highest API version both sides support, and **at least v1**. A server that can't do v1 (pre-keys-API) is unsupported. The tool says so and stops.

### 7.2 Crawl

There is one global work queue, not a loop per org. The crawl expands in waves:

1. **Server scope:** users, then each user's keys and ACL, then orgs.
2. **Per org:**
   - org, ACL, members, invitations
   - every collection listing: clients, groups, containers, nodes, roles, envs, data bags, cookbooks (`num_versions=all`), artifacts, policies, policy_groups
   - then each object, its keys or ACL, and its revisions
3. **Blobs:** every checksum named in a manifest that isn't already in this backup's blob store (or in the `--base` store, §7.5). Each is downloaded once by streaming the presigned URL straight into a SHA-256 + MD5 hasher; an MD5 mismatch fails the request. Cookbook `url` fields are ignored for anything except fetching.

Each response is written as soon as it arrives. A crash leaves a `.csb.partial` with an on-disk journal of what's done. `backup --resume` carries on from it.

### 7.3 Concurrency

The worker pool is AIMD:

- It starts at 8 in-flight requests and grows by 1 per window without errors.
- It halves on any 5xx, timeout, or p95 latency over twice the baseline.
- It caps at `--max-concurrency` (default 32).
- Blob downloads use a separate pool, because their limit is bookshelf/S3 bandwidth, not erchef.

This replaces knife-ec-backup's "set concurrency to 1 if you see 500s" advice. The tool finds the server's limit itself and won't make a struggling server worse. Retries: GETs up to 5 times with jittered backoff, honouring `Retry-After`.

### 7.4 Host tier

The host tier turns on when the tool finds `/etc/opscode/chef-server-running.json` and can read it; `--host=off` disables it. It needs no flags. It uses the same Postgres discovery as knife-ec-backup (including Automate's `automate-cs-oc-erchef`), always **read-only**, inside one `REPEATABLE READ READ ONLY` transaction:

- `users`: hashed_password, salt, hash_type, admin, external_authentication_uid, recovery_authentication_enabled, created_at, updated_at
- `clients`: admin, created_at, updated_at
- `keys`: created_at, matched by (actor name, key name)
- server-admins:
  - `groups` row `name='server-admins' AND org_id='000…0'`
  - then the bifrost group's members, read from bifrost's DB tables in the same way
  - then map authz ids back to usernames
- object timestamps for nodes, roles, environments, data_bag_items, cookbook_versions, policies
- oc-id `oauth_applications` (secrets go to `secrets/` only with `--with-server-secrets`)

The host tier **never** captures content data (nodes, cookbooks). That always comes from the API, so a backup's contents don't depend on erchef's storage encoding.

`consistency: snapshot` is a later option (§13): the host tier would also take a snapshot of object listings, and the API crawl would be checked against it.

### 7.5 Incremental and scheduled backups

- `--base previous.csb` reuses blobs. A checksum already in the base is copied from it, not downloaded. The output is still a self-contained file. This removes nearly all the cost of a daily backup, since cookbook blobs dominate both size and time.
- `--repo DIR` is a directory mode for scheduled use: `snapshots/<ts>/` plus one shared `blobs/`. `prune --keep-daily 7 --keep-weekly 4` removes blobs no snapshot references. `export <snapshot>` writes a self-contained `.csb`.

### 7.6 Integrity and consistency checks

At the end of a backup, the tool checks the result before it calls the backup complete:

- every reference resolves: ACL and group members, policy_group revisions, cookbook checksums
- the `clients` group matches the clients list (knife-ec-backup's warning, kept)
- counts match the listings

Each problem becomes a manifest warning with a stable code. `--strict` makes warnings fatal.

## 8. Restore

### 8.1 Flow

```
open & verify archive ─▶ discover target ─▶ snapshot target ─▶ plan ─▶ (--plan: print & exit)
      ─▶ preflight gates ─▶ apply DAG (journal) ─▶ host-tier writes ─▶ verify ─▶ fidelity report
```

- **Discover target.** Flavor and version, the API versions it supports, whether we have superuser rights, and whether we're running on its host (so the host tier can be used). The driver declares the target's **capabilities**, such as `password_hash_import`, `server_admins`, `frozen_enforced`, `timestamps`, `oauth_apps`, `same_name_client_user`.
- **Snapshot target.** Lists only (plus the GETs needed to compare when merging), so the plan knows what already exists.
- **Plan.** One node per write, each labelled `create`, `update`, `skip` (target already identical), or `conflict`. Edges follow the dependency rules in §8.3. `--plan` prints a summary (`+ 18311 nodes, ~ 2 roles, = 4,102 cookbook versions, ! 1 conflict`) and a JSON plan file for review.

### 8.2 Modes

| Flag | Behaviour |
|---|---|
| default (`--into empty`) | Target must hold nothing but the bootstrap superuser. Any existing object is a conflict, so the run stops at preflight |
| `--merge=skip` | Keep what the target has; add what's missing |
| `--merge=overwrite` | Make the target match the backup for every object in the backup (frozen cookbooks need `--force-frozen`) |
| `--prune` | Also delete target objects missing from the backup (needs `--merge=overwrite`; prompts unless `--yes`) |
| `--org a,b` / `--only nodes,roles` / `--exclude-org` | Restore part of the backup. Server-scope objects that selected orgs depend on are pulled in automatically |
| `--rename-org old=new` | Restore an org under a new name |

### 8.3 Dependency order

The rules (the DAG is built from them; this is not a fixed script):

1. Users, then their extra keys and user ACLs (the ACLs come last of all, because they may name other users).
2. Orgs. Creating an org also creates `<org>-validator` with a **new** key and `_default`. Restore immediately replaces the validator's `default` key with the backed-up public key (`PUT …/clients/<org>-validator/keys/default`) and marks the new private key for discarding. Existing `validation.pem` files keep working.
3. Memberships (superuser association), then invitations.
4. Per org, in parallel across orgs:
   1. Containers that aren't defaults.
   2. Clients, each with its backed-up public key, then extra keys. **Clients come before any group membership.** This removes knife-ec-backup's users-only `admins` pass and its re-apply pass (#22).
   3. Groups created empty, then membership PUT in topological order over `groups` (nested groups), in split form `{"actors":{"users","clients","groups"}}`. USAGs are remapped, not created (§8.4).
   4. Nodes, roles, environments, data bags and items. Raw bytes are PUT/POSTed as they are.
   5. Cookbook blobs, then cookbook manifests:
      - each version: sandbox POST with all its checksums, PUT only blobs where `needs_upload` is true, commit, then PUT the **stored manifest bytes** with `frozen?` intact;
      - sandbox batches span versions, so each blob goes to an org once.
   6. Cookbook artifacts (same blob path).
   7. Every policy revision (`POST /policies/P/revisions`), then policy group assignments.
   8. ACLs last: objects, containers, groups, then the org ACL. Granular split form (avoids 422 `ambiguous_actor`). An ACL is PUT only when it differs from the target's.
5. Host-tier writes (§8.6).
6. Verify (§9).

Writes inside one org run in parallel as soon as their dependencies are satisfied. Nodes don't wait for cookbooks, for example. The executor uses the same AIMD pool as backup.

### 8.4 Reference resolution (preflight)

Before any write, the planner resolves every name every object refers to, against "backup ∪ target ∪ objects the target creates itself":

- **Dangling ACL or group members.** A name that won't exist on the target, such as a user who left the org but is still in a node's ACL (#83). Behaviour depends on `--dangling`:
  - `drop` (default): remove the entry and record it;
  - `fail`;
  - `keep-as-group`: create a placeholder group. Rarely wanted.

  Without this step, one stale ACE fails a whole ACL PUT. Groups are worse: an unknown name is **silently** dropped.
- **USAGs.** A group whose name is 32 hex characters and whose only member is one user is that user's USAG. Restore doesn't create it. It finds the target's USAG for that user (created during association) and rewrites every reference: ACL entries and the parent `users` group. We use only the API for this, since the USAG's own membership names the user.
- **Same name for a client and a user.** Real Chef allows it; cinc-server-ng rejects it on purpose, because its ACLs match actors by bare name. The ng driver raises a preflight error listing the pairs, with `--rename-client old=new`. That also rewrites ACL references, and the operator must re-key that node.
- **Pivotal.** Restore never rotates the key it is signing with. Changing the target's pivotal key needs `--with-server-secrets` (§8.6), which signs with a temporary second key.

### 8.5 Idempotence, journal and resume

Each applied plan node goes into `<backup>.restore.journal`, an append-only JSON-lines file with plan-node id, result and target response hash. A re-run re-plans against the live target. Nodes the target already matches become `skip`, so the journal only speeds things up and correctness doesn't depend on it. `restore --resume` uses it to skip the target snapshot for finished subtrees.

Writes that aren't idempotent:

- Artifacts return 409 when they already exist. The executor compares, then skips.
- Invitations: the planner checks for an existing invite for the same user before recreating.

### 8.6 Host-tier writes

These run only when the target driver has the capability and the backup has the data. Otherwise the fidelity report records them as *not restored*.

**Legacy Chef Server.** Requires running on the target host, or a delegated script.

- `users`: `UPDATE users SET hashed_password=$1, salt=$2, hash_type=$3, admin=$4 WHERE username=$5`. Only the named columns are written, and the row must be the one the API just created. The API create used a throwaway random password, which this overwrites.
- `external_authentication_uid`, `recovery_authentication_enabled`: written through the API (superuser PUT), not SQL.
- `clients.admin`: SQL.
- server-admins: for each user, the bifrost `PUT /groups/<gid>/actors/<authz_id>` call that `chef-server-ctl grant-server-admin-permissions` makes. When possible we shell out to `chef-server-ctl grant-server-admin-permissions <user>`, which keeps us off bifrost's internal API.
- Timestamps (`--with-timestamps`, off by default): `UPDATE <table> SET created_at, updated_at WHERE org_id=… AND name=…`.
- oc-id apps: `INSERT` into the oc_id DB (`--with-oauth`).
- Server secrets (`--with-server-secrets`): this is a *rebuild the same server* scenario. Restore installs `pivotal.pem`, the webui pair and `private-chef-secrets.json`, re-registers the pivotal public key through the keys API, then tells the operator to run `chef-server-ctl reconfigure`.

**When not on the host,** restore writes `host-apply.sh` plus `host-apply.sql`. The operator runs `sudo cinc-server-backup restore --host-only chef.csb` on the target host to finish. The fidelity report shows these items as **pending** until that runs, and `verify` confirms them afterwards.

**cinc-server-ng.** Needs the changes in §11.2:

- A superuser-only `PUT /_cinc/users/<name>/password_hash` accepting `{hash_type, hashed_password, salt}`, plus `/_cinc/server_admins`.
- These are deliberate non-Chef extensions. They go in ng's authz classification, so they can't widen the fail-open surface.

### 8.7 Native restore into cinc-server-ng

```sh
cinc-server-ng restore chef.csb --storage sqlite --db /var/lib/cinc-server-ng/state.db
```

cinc-server-ng imports `format` and writes straight into the store inside one `Tx`. It's the fast path for very large migrations: no HTTP, no signing, one transaction, so it either fully succeeds or fully rolls back. It also keeps things the API path can't: org GUIDs, timestamps, hashes, server-admins.

It must uphold ng's own invariants, which a raw `Put` bypasses:

- Every document goes through `chefjson.Normalize`.
- **Every object gets an explicit ACL from the backup.** No object may fall back to `defaultACL()`, which is fail-open surface #2 in ng's CLAUDE.md.
- Group membership is written to both halves (document and `group_members`).
- Derived indexes rebuild on first start.

The API path (§8.3) stays the reference behaviour. CI runs both paths on the same backup and requires `verify` to give identical results (§10).

## 9. Verification and the fidelity report

`cinc-server-backup verify chef.csb --server URL` captures the target into memory (API tier, plus host tier when available) and compares it with the backup, object by object. The comparison knows each object type:

- **JSON comparison** compares by value: member order doesn't matter, number type and size are preserved exactly (`UseNumber`/exact decimal), and repeated keys are flagged.
- **Fields the target is expected to regenerate**, listed per driver: org `guid`, cookbook file `url`s, key `uri`, invitation ids, USAG names (compared through the remap).
- **ACLs and groups** are compared as sets, after remapping.

`restore` runs `verify` automatically at the end; `--no-verify` skips it. Exit codes:

- `0`: everything preserved, or regenerated as expected.
- `3`: something dropped or pending that the operator accepted with `--accept dangling,timestamps,…`. The run succeeded, but not everything was restored.
- `1`: anything else.

Example report:

```
fidelity: chef-2026-11-11.csb → https://new.example.com (chef-server 15.10.127)
  preserved     18311 nodes · 412 users · 1,033 clients (all keys) · 4,102 cookbook versions (frozen state)
                · 9,876 ACLs · 214 groups · 37 policy revisions
  regenerated   3 org guids · 412 USAG names · 2 invitation ids              (expected)
  dropped       4 dangling ACL entries (former users: alice, bob)            (accepted: --dangling=drop)
  pending       412 password hashes · 2 server-admins                        (run --host-only on target)
  not restored  timestamps                                                   (--with-timestamps not set)
```

`verify` also works by itself against the *source* right after a backup ("did I capture what's there?") and against any server later ("has it drifted?").

## 10. Security

- Backups contain data bag items, every public key and every node's attributes. With the host tier they also contain password hashes. **Encryption is on by default when the host tier ran.** Without `--recipient`/`--passphrase-file`, writing `host/users.json` needs `--insecure-plaintext`. `secrets/` is refused unless the backup is encrypted.
- Output files are created 0600 and written atomically (temp file, then rename). Neither the CLI nor the logs ever print object contents, hashes or keys.
- Host-tier reads use a read-only transaction. Writes name their columns and match on username/name, never on backed-up ids.
- Restore never makes the target more permissive than the backup:
  - dangling entries are removed, never widened;
  - USAG remapping keeps the same membership;
  - the ng native path writes an explicit ACL for every object.
- Every archive path is checked on read (no `..`, no absolute paths, no symlinks) before anything is extracted.

## 11. Changes needed in dependencies

### 11.1 cinc-api

The tool needs raw bytes, and today cinc-api decodes everything through typed structs without `UseNumber` (`transport.go:278-294`). That drops unknown fields, changes `1000.0` to `1000`, and corrupts integers above 2^53.

| Change | Why |
|---|---|
| Exported `Client.Do(ctx, method, path, body []byte, opts...) ([]byte, *Response, error)` with a per-call API version, wrapping the existing `doRaw` | Capture exact bytes; restore exact bytes |
| `Config.Org` optional (a root-scoped client) | `/users`, `/organizations`, user keys and user ACLs live at the root |
| Exported sandbox create, upload and commit; raw manifest PUT with `?force=true` | Restore stored manifests as they are, frozen state included; today `uploadCookbook` rebuilds the manifest from a local directory and has no `frozen?`/`force` |
| `DownloadBlob(ctx, url, w io.Writer, md5)` streaming into a caller-supplied writer | Blob store keyed by checksum, not by cookbook path |
| ACL `GetGranular` (`?detail=granular`) and split-form `SetPermission` | Restore ACLs where a user and a client share a name |
| Exported retry policy hooks (or a `WithRetryPolicy` option) | The AIMD executor needs to see 5xx/429 and `Retry-After` |
| Optional `X-Ops-Request-Source: web` signing | Fallback impersonation with the webui key |

None of this breaks the existing API. The typed services stay as they are.

### 11.2 cinc-server-ng

| Change | Why |
|---|---|
| Verify legacy hash types (`bcrypt`, `erlang-bcrypt-0.5.0`, `SHA1-bcrypt`, `sha1+bcrypt`, `SHA1`) and rehash to PBKDF2 on successful login | Imported web logins keep working |
| Superuser-only `/_cinc/users/<n>/password_hash` and `/_cinc/server_admins`, classified in authz | Host-tier writes over the API path |
| A server-admins concept | Exists in real Chef; currently out of scope in ng |
| Enforce frozen cookbooks (409 without `?force=true`) | Behavioural fidelity; the planner's conflict detection relies on it |
| `cinc-server-ng restore <csb>` native path | §8.7 |
| Decide on client/user same-name collision | Real Chef allows it; ng rejects it for ACL-safety reasons. Restore needs either support (ACLs keyed by principal type) or the `--rename-client` workaround (§8.4) |

## 12. Testing

- **Format:** round-trip property tests (write → read → write gives identical bytes), fuzzing the reader, golden manifests.
- **Round-trip matrix in CI:**
  - Seed a legacy Cinc Server container with a realistic fixture: multiple orgs, nested groups, user/client name collisions, frozen cookbooks, unused policy revisions, rotated keys, dangling ACEs, server-admins.
  - Back it up, then restore into (a) a fresh legacy server, (b) cinc-server-ng over the API, and (c) cinc-server-ng natively.
  - `verify` must pass with exactly the expected report for each target.
  - Then back up the restored target and diff the two `.csb` files, with only the regenerated fields allowed to differ.
- **Live client checks after restore:**
  - a node's existing `client.pem` converges;
  - `validation.pem` bootstraps a new node;
  - `knife` works with a restored user key;
  - a web login works with the original password (host tier).

  These are the facts operators care about.
- **Fault injection:** kill the process at random points during backup and restore, then `--resume`; inject 500s and latency to exercise AIMD; truncate and corrupt archives.
- **Scale:** generate data with cinc-server-ng's `seedgen`/`fleetsim` (50k nodes, 10k cookbook versions) to track backup and restore time and peak server load.

## 13. Milestones

1. **Format + API backup.** The `format` package and `capture/api`, with `inspect`/`extract`/`pack`/`verify` (integrity). cinc-api raw `Do`, root client and blob download. *Gives better knife-ec-backup backups already.*
2. **API restore into legacy.** Planner, executor, journal, reference resolution, frozen manifests, validator key replacement, verify plus fidelity report.
3. **Host tier.** Read on backup; write on restore into legacy (`--host-only` delegation included). Encryption required when it's used.
4. **cinc-server-ng.** API restore (needs ng's legacy-hash and frozen changes), then native restore.
5. **Operations.** `--repo` with `prune`, `--base` incrementals, and a published reference systemd timer.
6. **Later.** `consistency: snapshot`; continuous mirror mode (`sync`), which reuses plan/apply against a live source.

## 14. Open questions

1. **Lowest supported source version.** The keys API needs Chef Server 12.1 or later. Do we need anything older (knife-ec-backup still special-cases pre-12)?
2. **Automate-embedded Chef Server:** support the host tier from milestone 3, or API-only at first?
3. **Bifrost writes:** shell out to `chef-server-ctl grant-server-admin-permissions` (stable, slow, needs root) or call bifrost directly (fast, internal API)? Draft says shell out.
4. **Client/user same-name in ng:** model the principal type in ACLs, or keep refusing and rely on `--rename-client`?
5. **Name and file extension:** `.csb` is short; check for a conflicting extension before release.
6. **Should `format` live in its own module** so cinc-server-ng doesn't pull in the tool's dependencies? Draft: same repo, separate `go.mod` under `format/`.
