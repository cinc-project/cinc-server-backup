# Contributing

## Layout

| Path | What it holds |
|---|---|
| `format/` | The `.csb` file format: reading, writing and checking backups. Standard library only, because cinc-server-ng imports it |
| `capture/` | Taking a backup over a server's API |
| `restore/` | Writing a backup into a server over its API |
| `compare/` | Comparing two backups, setting aside what a server regenerates |
| `internal/refs/` | Resolving the names ACLs and groups refer to, and picking part of a backup (`--org`) |
| `internal/cli/` | The command line |
| `integration/` | Tests against a real cinc-server-ng, in a separate Go module so that dependency never reaches users of the root module |

The [design document](docs/specs/2026-11-11-cinc-server-backup-design.md) explains the approach and the milestones.

## Build and test

```sh
go build ./cmd/cinc-server-backup
go test -race ./...
cd integration && go test -race ./...
golangci-lint run ./... && (cd integration && golangci-lint run ./...)
```

The integration tests start in-memory cinc-server-ng servers, so they need no setup.

## End-to-end run

`.github/scripts/e2e-ng.sh <fixture>` runs the whole cycle against a real cinc-server-ng: the container image (`NG_IMAGE`) or a binary (`NG_BIN`). It:

1. starts a server loaded from the fixture, and an empty one;
2. backs up the source and checks the file;
3. restores into the empty server (restore checks its own result), runs `verify --server`, backs up the target and diffs the two backups;
4. gives a copy of the backup a deleted user and client in an ACL and a group, as an erchef backup can have, then checks that:
   - `--dangling fail` writes nothing;
   - the default leaves out exactly those names and exits 3;
   - the result matches the clean backup.

CI runs it with the released image and cinc-server-ng's `dev/test-repo` at the version in `integration/go.mod`. Locally:

```sh
git clone --depth 1 --branch v0.20.1 https://github.com/cinc-project/cinc-server-ng /tmp/ng
(cd /tmp/ng && go build -o /tmp/ng/cinc-server-ng ./cmd/cinc-server-ng)
NG_BIN=/tmp/ng/cinc-server-ng .github/scripts/e2e-ng.sh /tmp/ng/dev/test-repo
```
