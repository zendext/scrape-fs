# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

## ai;dr

Torrent clients need the original download paths for seeding; Jellyfin and Plex
expect organized media-library paths. ScrapeFS gives the same files both views:
keep seeding in place and expose a player-friendly virtual library, without
moving, renaming, or copying the source media.

> This project was written by AI. Use it at your own risk.

## Desktop GUI

The optional Linux desktop app manages local libraries, mappings, service status,
and configuration. Install the desktop archive alongside the core package; see
[desktop setup](docs/en/desktop.md) for dependencies and instructions.

![ScrapeFS desktop browsing an example movie library](docs/assets/desktop-en.png)

## Typical Use Cases

- Keep media in the original paths managed by a BitTorrent client for continued
  seeding while exposing a virtual library with player-friendly directories and
  filenames to Jellyfin or Plex.
- Aggregate media from multiple disks or download directories into one Library
  without copying files or changing the existing directory layout.
- Let an external Agent identify media and generate a Projection Batch, then
  create NFO, artwork, subtitles, and other companion files through FUSE.

## Installation and Setup

ScrapeFS requires Linux, systemd, `/dev/fuse`, `findmnt` from util-linux, and
root access for installing files, creating groups, and mounting FUSE.

The preferred path is to download the `linux/amd64` archive and `SHA256SUMS`
from [GitHub Releases](https://github.com/zendext/scrape-fs/releases):

```bash
version=v0.3.0
archive="scrape-fs_${version}_linux_amd64.tar.gz"

sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "$archive"
cd "${archive%.tar.gz}"
sudo ./install.sh --administrator "$USER"
```

The installer preserves an existing configuration and does not start the
service automatically. Before the first start, review the configuration, then
start the daemon, verify the runtime, and create the first Library:

```bash
sudo cat /etc/scrapefs/config.toml
sudo systemctl enable --now scrapefsd.service

scrapefs status
scrapefs doctor
scrapefs library add jellyfin --profile jellyfin@1
scrapefs library list
```

Manual source builds remain supported. See the
[deployment guide](docs/en/deployment.md) for the complete installer behavior,
source-build procedure, paths, permissions, and update workflow.

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

New and rebound Projections use a source identity that remains stable across ordinary filesystem remounts. Legacy identities from existing databases require a control user to preview and explicitly confirm the digest returned by `scrapefs projection identity migrate --library <name>`; daemon startup never migrates them automatically.

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

`scrapefs scrape <source> --library <name>` is not part of the CLI. ScrapeFS
does not persist unapplied plans, drafts, or revisions. The external Agent owns
the work context, while the daemon stores only the minimal provenance needed
to audit and undo a successfully applied batch.

## Documentation

- [Use cases](docs/en/use-cases.md)
- [Architecture](docs/en/architecture.md)
- [Projection model](docs/en/projection.md)
- [Projection batches and the external Agent workflow](docs/en/scrape.md)
- [Filesystem and object storage](docs/en/filesystem.md)
- [Deployment guide](docs/en/deployment.md)
- [Operations and CLI](docs/en/operations.md)
- [简体中文文档](docs/zh-CN/README.md)

The Chinese documentation is currently the normative source. The English documentation is its maintained translation.

## License

ScrapeFS is licensed under the [Apache License 2.0](LICENSE).
