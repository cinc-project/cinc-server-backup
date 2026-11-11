# cinc-server-backup

[![CI](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cinc-project/cinc-server-backup/actions/workflows/ci.yml?query=branch%3Amain)

Full-fidelity backup and restore for Chef Infra Server and Cinc Server. One portable, verifiable backup file restores into a legacy Chef/Cinc Server or into [cinc-server-ng](https://github.com/cinc-project/cinc-server-ng).

Design: [docs/specs/2026-11-11-cinc-server-backup-design.md](docs/specs/2026-11-11-cinc-server-backup-design.md).

## Status

Milestone 1 (format and API backup) is in progress. Restore is not built yet.

| Command | What it does |
|---|---|
| `backup --server URL --user NAME --key PEM -o FILE` | Crawls the whole server over its API as a superuser and writes a `.csb` file (0600, written atomically) |
| `inspect FILE` | Prints source, time window, per-collection counts and warnings |
| `verify FILE` | Checks every member against the manifest's SHA-256 digests |
| `version` | Prints the version |

Every command has `--help` with a description and examples, and `completion` prints shell completion scripts.

## Development

The root module depends only on [cinc-api](https://github.com/cinc-project/cinc-api). Tests that need a real server live in `integration/`, a separate module that runs an in-memory cinc-server-ng:

```sh
go test -race ./...
cd integration && go test -race ./...
golangci-lint run ./... && (cd integration && golangci-lint run ./...)
```

## License

Apache-2.0. See [LICENSE](LICENSE).
