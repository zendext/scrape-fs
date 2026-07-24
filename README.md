# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

ScrapeFS is an external-agent-assisted media organization and virtual directory filesystem for Linux.

It projects media files from one or more local filesystems into player-friendly libraries without moving, renaming, or modifying the source files.

> Status: early implementation. The daemon loads and validates configuration, acquires its host-filesystem runtime lock, initializes SQLite and the Overlay object store, mounts the persisted namespace, and serves authenticated Library, direct Projection, and Projection Batch operations through the Unix control socket. FUSE clients can create and modify ordinary directories and object-backed OverlayFiles, read ProjectionFiles through verified source descriptors, and rename or unlink direct ProjectionFiles through shared Projection lifecycle operations.

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
        ├── Dune (2021).mkv
        ├── Dune (2021).zh.ass
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

When media identification is needed, an external Agent skill reads the source directory, probes media, queries metadata, and generates an explicit batch:

```bash
scrapefs projection batch validate --file /tmp/dune.batch.json --json
scrapefs projection batch apply \
  --file /tmp/dune.batch.json \
  --expected-digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

`scrapefsd` does not run a model or expose file-read, `ffprobe`, or network tools to an Agent. Validate checks the batch without persistence. Apply safely reopens the sources, validates paths and conflicts, and creates every Projection atomically. The external Agent creates NFO files and artwork through ordinary FUSE operations after apply.

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
go vet ./...
gofmt -w .
```

## Documentation

- [Use cases](docs/en/use-cases.md)
- [Architecture](docs/en/architecture.md)
- [Projection model](docs/en/projection.md)
- [Projection batches and the external Agent workflow](docs/en/scrape.md)
- [Filesystem and object storage](docs/en/filesystem.md)
- [Operations and CLI](docs/en/operations.md)
- [First-version acceptance criteria](docs/en/acceptance.md)
- [简体中文文档](docs/zh-CN/README.md)

The Chinese documentation is currently the normative source. The English documentation is its maintained translation.
