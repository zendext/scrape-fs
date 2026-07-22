# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

ScrapeFS is an agent-assisted media scraping and virtual directory filesystem for Linux.

It projects media files from one or more local filesystems into player-friendly libraries without moving, renaming, or modifying the source files.

> Status: early implementation. The daemon loads and validates configuration, acquires its host-filesystem runtime lock, initializes SQLite, and provides the transactional persistent namespace core plus Library creation, lookup, and listing services. The control socket and FUSE behavior are not implemented yet.

## Core Model

- **Library**: a virtual media library exposed under the FUSE mount, such as `jellyfin` or `plex`.
- **Projection**: a read-only mapping from an external source file to a virtual library path.
- **OverlayFile**: a writable file managed by ScrapeFS, such as an NFO file, poster, or downloaded subtitle.
- **Scrape**: an agent-assisted workflow that identifies media and generates a reviewable batch of Projections.
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
```

Removing or renaming the Projection does not modify the source file.

## Agent-Assisted Scrape

When media identification is needed:

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

The daemon enumerates only the selected file or directory, gives the resulting file manifest to the Agent, renders target paths through the selected Profile, and shows the result before applying it.

## First-Version Boundaries

The first version targets:

- Linux and FUSE;
- Plex and Jellyfin libraries;
- movies, standard `SxxEyy` episodes, and external subtitles;
- read-only source Projections;
- writable OverlayFiles;
- interactive CLI review and confirmation.

It does not target torrent management, downloader integration, a GUI, writable Projections, a complete POSIX filesystem, dynamic Profile plugins, or cross-host Scrape import.

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
- [Scrape and Agent workflow](docs/en/scrape.md)
- [Filesystem and object storage](docs/en/filesystem.md)
- [Operations and CLI](docs/en/operations.md)
- [First-version acceptance criteria](docs/en/acceptance.md)
- [简体中文文档](docs/zh-CN/README.md)

The Chinese documentation is currently the normative source. The English documentation is its maintained translation.
