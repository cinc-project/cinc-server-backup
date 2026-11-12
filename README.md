# cinc-server-backup

[![CI](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml?query=branch%3Amain)

Full-fidelity backup and restore for Chef Infra Server and Cinc Server. One portable, verifiable backup file restores into a legacy Chef/Cinc Server or into [cinc-server-ng](https://github.com/cinc-project/cinc-server-ng).

Design: [docs/specs/2026-11-11-cinc-server-backup-design.md](docs/specs/2026-11-11-cinc-server-backup-design.md).

## Status

Backup and restore over the API work. Restore needs an empty target; the host tier (password hashes, server-admins), merge modes and resume are still to come.

| Command | What it does |
|---|---|
| `backup --server URL --user NAME --key PEM -o FILE` | Crawls the whole server over its API as a superuser and writes a `.csb` file (0600, written atomically) |
| `restore FILE --server URL --key PEM` | Checks the backup's checksums, then restores it into an empty server over its API; clients and users keep their keys. ACL entries and group members naming a user, client or group the backup lacks are left out and listed (`--dangling fail` stops instead) |
| `inspect FILE` | Prints source, time window, per-collection counts and warnings |
| `verify FILE` | Checks every member against the manifest's SHA-256 digests |
| `diff BEFORE AFTER` | Lists what the first backup holds that the second lacks or holds differently, setting aside what a server regenerates (GUIDs, URLs, per-user group names); exits 1 when anything differs |
| `version` | Prints the version |

Every command has `--help` with a description and examples, and `completion` prints shell completion scripts.

## Development

The root module depends only on [cinc-api](https://github.com/cinc-project/cinc-api). Tests that need a real server live in `integration/`, a separate module that runs an in-memory cinc-server-ng:

```sh
go test -race ./...
cd integration && go test -race ./...
golangci-lint run ./... && (cd integration && golangci-lint run ./...)
```

`scripts/e2e-ng.sh <fixture>` runs the whole cycle against a real cinc-server-ng: it starts a server loaded from the fixture and an empty one (the container image with `NG_IMAGE`, or a binary with `NG_BIN`), backs up, verifies, restores, backs up the target and diffs. CI runs it with the released image and cinc-server-ng's `dev/test-repo`.

## License

Apache-2.0. See [LICENSE](LICENSE).
