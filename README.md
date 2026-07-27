# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

ScrapeFS is an external-agent-assisted media organization and virtual directory filesystem for Linux.

It projects media files from one or more local filesystems into player-friendly libraries without moving, renaming, or modifying the source files.

> Status: the v1 target feature set is implemented. The project is in a pre-release stage and has not published a stable release. The daemon loads and validates configuration, acquires its host-filesystem runtime lock, initializes SQLite and the Overlay object store, rebuilds the in-memory index, mounts the persisted namespace, and serves authenticated Library, direct Projection, Projection Batch, status, and read-only doctor operations through the Unix control socket. Readiness is emitted only after the index, mount, and socket are usable; committed namespace changes trigger FUSE cache invalidation, and normal shutdown drains admitted writes before unmounting. FUSE clients can create and modify ordinary directories and object-backed OverlayFiles, read ProjectionFiles through verified source descriptors, and rename or unlink direct ProjectionFiles through shared Projection lifecycle operations. The repository also ships the first installable external Agent skill for media enumeration, probing, naming, batch confirmation, and FUSE artifact workflows.

## Core Model

- **Library**: a virtual media library exposed under the FUSE mount, such as `jellyfin` or `plex`.
- **Projection**: a read-only mapping from an external source file to a virtual library path.
- **Projection Batch**: external JSON input that creates and undoes multiple Projections atomically.
- **OverlayFile**: a writable file managed by ScrapeFS, such as an NFO file, poster, or downloaded subtitle.
- **Profile**: deterministic naming and directory rules for a media player.

`Node` is an internal implementation term for an entry in the virtual filesystem tree. It is not part of the user-facing API.

## Example

Given:

```text
/mnt/disk1/downloads/Dune.2021.REMUX/
├── Dune.2021.REMUX.mkv
├── Dune.zh.ass
└── sample.mkv
```

ScrapeFS can expose:

```text
/mnt/scrapefs/jellyfin/
└── Movies/
    └── Dune (2021) [tmdbid-438631]/
        ├── Dune (2021) [tmdbid-438631].mkv
        ├── Dune (2021) [tmdbid-438631].zh.ass
        ├── movie.nfo
        └── poster.jpg
```

The video and source subtitle are read-only Projections. The NFO and artwork are writable OverlayFiles stored by ScrapeFS.

## Direct Projection

When the source and target paths are already known:

```bash
scrapefs library add jellyfin --profile jellyfin@1

scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"

scrapefs projection list --library jellyfin

scrapefs projection mv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  "jellyfin:/Films/Dune (2021).mkv"
```

`projection add` safely opens the source and atomically creates the mapping. `projection list` returns stable IDs, virtual targets, and cached source identity from the persisted namespace without reopening sources. `projection mv`, `projection rm`, and confirmed `projection rebind` preserve the stable node ID where applicable and never modify either source file.

## External Agent Skill

When media identification is needed, install the repository's
[`organize-scrapefs-media`](skills/organize-scrapefs-media/SKILL.md) Skill. It
reads sources with the invoking user's permissions, runs `ffprobe`, reads NFO,
and generates an explicit batch:

```bash
SKILL_DIR="${CODEX_HOME:-$HOME/.codex}/skills/organize-scrapefs-media"

python3 "$SKILL_DIR/scripts/scrapefs_media.py" plan \
  --source /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin \
  --profile jellyfin \
  --batch /tmp/dune.batch.json \
  --artifacts /tmp/dune.artifacts.json

python3 "$SKILL_DIR/scripts/scrapefs_media.py" validate \
  --batch /tmp/dune.batch.json
```

`scrapefsd` does not run a model or expose file-read, `ffprobe`, or network tools to an Agent. Validate checks the batch without persistence. The user must return the complete validation digest verbatim before the Skill freshly validates and applies. The Skill creates NFO and artwork through ordinary FUSE operations after apply. See [Projection batches and the external Agent workflow](docs/en/scrape.md) for installation, confirmation, and recovery details.

The earlier specification used `scrapefs scrape <source> --library <name>` and assumed the daemon would enumerate and identify media. That command is removed from the target v1 CLI. ScrapeFS also does not persist unapplied plans, drafts, or revisions. The external Agent owns work context, while the daemon stores only minimal provenance for a successfully applied batch and later undo.

## First-Version Boundaries

The first version targets:

- Linux and FUSE;
- Plex and Jellyfin libraries;
- movies, standard `SxxEyy` episodes, and external subtitles;
- read-only source Projections;
- writable OverlayFiles;
- versioned JSON Projection batches;
- a machine-readable CLI for people and external Agent skills.

It does not target an embedded model provider, daemon-side Agent sessions, torrent management, downloader integration, a GUI, writable Projections, a complete POSIX filesystem, dynamic Profile plugins, or cross-host path translation.

## Installation

ScrapeFS can currently be built from source and installed with the repository
script, which initializes the standard paths, permissions, configuration, and
systemd unit:

```bash
mkdir -p build
go build -trimpath -o build/scrapefs ./cmd/scrapefs
go build -trimpath -o build/scrapefsd ./cmd/scrapefsd

sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build
```

The script preserves an existing configuration and does not start the service
automatically. See the [deployment guide](docs/en/deployment.md) for the full
procedure.

## Development

ScrapeFS currently requires Linux and Go 1.25 or newer.

Build both command packages:

```bash
go build ./cmd/...
```

Run the current entry points:

```bash
go run ./cmd/scrapefs --help
go run ./cmd/scrapefsd --version
```

Run the baseline checks:

```bash
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
sh -n scripts/install.sh
systemd-analyze verify packaging/systemd/scrapefsd.service
python3 -m unittest discover \
  -s scripts/tests \
  -p 'test_*.py'
python3 -m unittest discover \
  -s skills/organize-scrapefs-media/tests \
  -p 'test_*.py'
```

`go test ./...` includes real FUSE-mount and cross-UID/GID ACL integration
tests. The FUSE tests require `/dev/fuse` and either root or effective
`CAP_SYS_ADMIN`; the cross-role ACL connection test requires root or a
root-mapped user namespace. Tests explicitly report skipped when these
requirements are unavailable.

## Documentation

- [Use cases](docs/en/use-cases.md)
- [Architecture](docs/en/architecture.md)
- [Projection model](docs/en/projection.md)
- [Projection batches and the external Agent workflow](docs/en/scrape.md)
- [Filesystem and object storage](docs/en/filesystem.md)
- [Deployment guide](docs/en/deployment.md)
- [Operations and CLI](docs/en/operations.md)
- [First-version acceptance criteria](docs/en/acceptance.md)
- [简体中文文档](docs/zh-CN/README.md)

The Chinese documentation is currently the normative source. The English documentation is its maintained translation.
