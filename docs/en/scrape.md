# Projection Batches and the External Agent Workflow

**English** | [简体中文](../zh-CN/scrape.md)

Media identification does not run inside `scrapefsd`. An external Agent skill, script, or manual tool reads source directories, runs `ffprobe`, queries metadata, resolves ambiguity, and generates an explicit Projection batch. ScrapeFS is responsible only for deterministic validation, preview, atomic apply, and batch undo.

The batch JSON is an external input document, not a daemon-side plan. ScrapeFS does not persist unapplied batches or maintain draft, ready, revision, revise, or discard lifecycles.

## 1. Difference from the Earlier Specification

The earlier specification planned:

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

That command implied that the daemon would enumerate the source directory, run Agent analysis, and generate Projections. The behavior did not enter the v1 implementation and has been removed from the target v1 CLI contract without a compatibility alias.

A later external-Agent design considered persisting `scrapefs.scrape/v1` plans, revisions, and pending approval state in the daemon. The external Agent already owns work context and user interaction, so that duplicate state is no longer part of v1.

Responsibilities are now split as follows:

1. An external Agent or another tool performs media scraping and generates `scrapefs.projection-batch/v1` JSON.
2. `projection batch validate` validates and previews the batch without persistence.
3. `projection batch apply` revalidates and atomically creates all Projections.
4. After successful apply, the daemon stores only minimal batch provenance for audit and undo.

## 2. External Agent Boundary

The external Agent:

- reads the host filesystem with the invoking control user's identity;
- chooses its own model, prompts, metadata providers, and media-probing tools;
- owns ignored-source handling, candidate comparison, user confirmation, and batch-file storage;
- validates and applies batches through the `scrapefs` CLI;
- creates NFO files, artwork, and other OverlayFiles through ordinary FUSE operations after apply.

ScrapeFS does not provide:

- an embedded model provider;
- a daemon-side Agent session or session token;
- a model tool-calling loop;
- arbitrary file-read, `ffprobe`, or network-query Agent APIs;
- a MediaModel, Question/Answer loop, pending plan, or Agent-specific Overlay API.

The external Agent is not an additional authorization boundary. It can use only its own host-filesystem permissions, control CLI permissions, and FUSE permissions.

## 3. Batch Format

The first version accepts strict UTF-8 JSON only. The CLI reads a batch from a file or stdin:

```bash
scrapefs projection batch validate --file /tmp/dune.batch.json
scrapefs projection batch validate --file -
```

```json
{
  "schema": "scrapefs.projection-batch/v1",
  "library": "jellyfin",
  "entries": [
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.2021.REMUX.mkv",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021) [tmdbid-438631].mkv"
    },
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.zh.ass",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021) [tmdbid-438631].zh.ass"
    }
  ]
}
```

Rules:

- `schema` is `scrapefs.projection-batch/v1`;
- `library` references an ACTIVE Library;
- `entries` is non-empty;
- `source` is a lexically normalized absolute local path;
- `target` is relative to LibraryRoot and passes generic and Profile validation;
- one batch cannot contain a duplicate source or target;
- unknown fields and invalid types are rejected;
- the first version does not support YAML, remote sources, or cross-host path translation.

Media titles, provider IDs, ignored sources, confidence, and Agent reasoning are outside the batch schema. The external Agent can retain those details in its own work context.

## 4. Stateless Validate

```bash
scrapefs projection batch validate \
  --file /tmp/dune.batch.json \
  --json
```

The daemon:

1. strictly parses the batch;
2. resolves the ACTIVE Library and current Profile;
3. checks every source through the secure source-file primitive;
4. rejects symlinks, magic links, directories, and other non-regular files;
5. runs generic and Profile path validation;
6. checks conflicts within the batch and against the current namespace;
7. computes a SHA-256 digest of the normalized batch;
8. returns the diff, warnings, digest, and current namespace version;
9. closes every source fd and writes nothing to SQLite.

Example result:

```json
{
  "schema": "scrapefs.projection-batch-validation/v1",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "namespace_version": 42,
  "valid": true,
  "entries": [],
  "warnings": []
}
```

The digest is computed from the strictly parsed normalized batch and is formatted as `sha256:` followed by 64 lowercase hexadecimal digits. Input JSON whitespace and object-field order do not affect it; `entries` array order is preserved. `namespace_version` is review information only. A changed version causes apply to recheck conflicts but does not fail solely because the version differs.

Validate creates no pending batch, source snapshot, revision, or operation-log entry. The external Agent retains the original batch JSON and revalidates after changing it.

## 5. Atomic Apply

```bash
scrapefs projection batch apply \
  --file /tmp/dune.batch.json \
  --expected-digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Before apply, the daemon confirms:

- the caller has control permission;
- the expected digest matches the normalized submitted batch;
- every source is still safely openable and regular;
- the Library is ACTIVE;
- the Profile version is available;
- every path-safety check passes;
- no target conflicts exist within the batch or namespace.

Atomic flow:

1. Parse the batch again and validate the expected digest.
2. Safely reopen every source and retain its apply-time identity.
3. Rerun generic path, Profile, and conflict validation.
4. Build the complete candidate namespace change.
5. Start a SQLite write transaction.
6. Recheck target conflicts.
7. Create a minimal applied-batch provenance record.
8. Create all managed Directories and ProjectionFiles.
9. Write one batch operation-log entry.
10. Increment namespace version once.
11. Commit.
12. Update the in-memory index and invalidate FUSE caches.

Steps 5 through 11 use one transaction. The implementation cannot simulate an atomic batch by repeatedly invoking independently committing `projection add` operations.

A successful result returns a stable batch ID:

```json
{
  "schema": "scrapefs.projection-batch-result/v1",
  "batch_id": "batch-42",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "namespace_version": 43
}
```

The same batch may be applied again while its targets do not exist; the digest is not an idempotency key. Existing targets return a conflict and are never silently treated as success.

## 6. Applied Batch Provenance

Only a successfully applied batch is persisted:

```text
projection batch provenance
├── id
├── library_id
├── profile_name
├── profile_version
├── digest
├── status
├── applied_by_uid
├── applied_by_gid
├── applied_at
├── undone_by_uid
├── undone_by_gid
└── undone_at
```

Statuses:

```text
applied
undone
```

The foundational namespace migration already released the names `scrapes`, `nodes.origin_scrape_id`, and `operation_log.scrape_id`. Schema migration 3 extends those legacy storage names for batch provenance instead of rewriting the released migration. They are internal storage names and are not part of the CLI or JSON schema.

A batch does not store external Agent prompts, conversations, metadata responses, or unapplied JSON. `projection_batch_entries` stores ordered source identity and target history so `show` remains complete after undo, while the operation log stores apply and undo path snapshots.

## 7. Show, List, and Undo

```text
scrapefs projection batch list
scrapefs projection batch show batch-42
scrapefs projection batch undo batch-42
```

`show` displays provenance, original apply-time Projection entries, and associated operation-log snapshots for an applied or undone batch only. A later same-Library Projection move is appended to that history without changing the entries or the approved Batch digest. It exposes no pending plan.

Batch history remains available to `list` and `show` after its Library is retired. Undo requires an available LibraryRoot and rejects a retired Library without changing batch state.

Undo uses one SQLite transaction to:

1. remove ProjectionFiles whose legacy `origin_scrape_id` references the batch;
2. prune empty managed directories toward LibraryRoot;
3. write the operation log;
4. mark the batch undone and save caller UID/GID;
5. increment namespace version once.

Undo removes each remaining Batch-owned Projection by provenance, including a Projection moved from its original target. It does not remove OverlayFiles created through FUSE. The CLI lists retained OverlayFiles below both original and current target directories.

Repeated undo of an undone batch returns `AlreadyUndone`.

## 8. Relationship to Direct Projection

The direct operation creates one Projection at a time. Direct and batch operations share:

- secure source opening and identity extraction;
- Library resolution;
- generic and Profile path validation;
- conflict classification;
- candidate Projection construction.

The direct operation commits its own single-Projection transaction. The batch operation owns one caller transaction and creates every entry without invoking an independently committing direct operation.

## 9. Overlay Artifacts

After batch apply, the external Agent can create artifacts through ordinary FUSE operations:

```bash
printf '%s' "$NFO" > \
  "/mnt/scrapefs/jellyfin/Movies/Dune (2021) [tmdbid-438631]/movie.nfo"

cp poster.jpg \
  "/mnt/scrapefs/jellyfin/Movies/Dune (2021) [tmdbid-438631]/poster.jpg"
```

These files follow ordinary FUSE create, write, rename, and permission semantics:

- virtual UID/GID comes from the FUSE caller;
- mode follows ordinary umask and Library permission rules;
- backing objects retain daemon-private permissions;
- batch undo does not automatically remove OverlayFiles;
- ScrapeFS does not distinguish files created by a person, script, or Agent.

## 10. Agent Skill Package

The repository includes the installable
[`organize-scrapefs-media`](../../skills/organize-scrapefs-media/SKILL.md) Skill.
Its standard-library Python tool performs deterministic source enumeration,
sample exclusion, `ffprobe` probing, NFO reading, Plex/Jellyfin naming, batch
generation, CLI orchestration, and FUSE artifact writes.

The Skill is not a daemon plugin. It contains no model provider, Agent session,
shared credential, daemon-side pending state, or privileged read capability.
Metadata-provider and model selection remain with the invoking Agent; the
deterministic tool accepts only local metadata JSON already resolved by that
Agent.

### 10.1 Installation And Prerequisites

The first version requires:

- Linux;
- Python 3.9 or newer;
- `ffprobe`;
- `scrapefs`;
- a ready `scrapefsd`;
- an ACTIVE `jellyfin@1` or `plex@1` Library;
- the invoking user's ordinary source, control-socket, and FUSE permissions.

Install from the repository root into the Codex Skill directory:

```bash
destination="${CODEX_HOME:-$HOME/.codex}/skills/organize-scrapefs-media"
test ! -e "$destination"
mkdir -p "$(dirname "$destination")"
cp -R skills/organize-scrapefs-media "$destination"
python3 "$destination/scripts/scrapefs_media.py" --help
SKILL_DIR="$destination"
```

Installation does not modify daemon configuration or install model/provider
credentials. Do not use `sudo` or another identity to bypass the invoking
user's source permissions.

### 10.2 Supported Scope

The Skill supports:

- a single movie file;
- one non-sample movie in a directory;
- episodes containing exactly one standard `SxxEyy` token per file;
- multiple independent episode files in one season;
- ordinary external subtitles matching a media basename;
- `movie.nfo`, `tvshow.nfo`, and episode-basename NFO files;
- one `poster`, `folder`, `cover`, or `movie` artwork candidate;
- deterministic Plex and Jellyfin movie/show/season/episode naming.

Files containing a complete `sample` token or below a `sample`/`samples`
directory are ignored. Every selected video is probed directly by the invoking
user and must contain at least one video stream. Source NFO and artwork do not
enter the batch; they become OverlayFiles through FUSE after apply. When no NFO
exists, the Skill can generate a minimal NFO from resolved metadata.

BDMV/DVD, anime absolute numbering, multi-episode files, automatic edition
detection, complex extras, music/image-only sources, remote URLs, symlinks,
non-regular sources, and ambiguous multi-video layouts return an explicit
`manual` result. The Skill never silently drops a non-sample video.

### 10.3 Plan And Metadata Boundary

Read the actual Profile through `library list`, then generate a local batch and
artifact manifest:

```bash
scrapefs library list

python3 "$SKILL_DIR/scripts/scrapefs_media.py" plan \
  --source /srv/downloads/Dune.2021 \
  --library media \
  --profile jellyfin \
  --batch /tmp/dune.batch.json \
  --artifacts /tmp/dune.artifacts.json
```

`plan` does not connect to the control socket, apply, or query a network
provider. Existing NFO takes precedence over filename inference; explicit
Agent-resolved metadata JSON takes precedence over NFO:

```json
{
  "kind": "movie",
  "title": "Dune: Part One",
  "year": 2021,
  "provider_ids": {
    "tmdb": "438631"
  }
}
```

For `status: manual`, stop and present every code, message, and path. After
resolving ambiguity, regenerate with `--metadata <file>`; do not modify or
relax the batch schema.

### 10.4 Validate, Digest Confirmation, And Apply

```bash
python3 "$SKILL_DIR/scripts/scrapefs_media.py" validate \
  --batch /tmp/dune.batch.json
```

The Agent must present the complete validation JSON, including entries,
warnings, namespace version, and digest, then stop and require the user to
return the complete `sha256:<64 lowercase hex>` digest verbatim. `yes`, `looks
good`, a digest copied into the Agent's own message, or inferred intent is not
confirmation.

`plan` and `validate` never apply in TTY or non-TTY use and never prompt. The
Skill has no combined plan/validate/apply command. Only after the user returns
the exact digest may the Agent run:

```bash
python3 "$SKILL_DIR/scripts/scrapefs_media.py" apply \
  --batch /tmp/dune.batch.json \
  --confirm-digest sha256:<exact-user-confirmed-value>
```

`apply` first validates again, compares the fresh digest byte-for-byte with
`--confirm-digest`, and only then calls `projection batch apply
--expected-digest`. A changed digest, changed source, target conflict,
permission failure, or any other validate/apply error stops the workflow. Show
the new result and request confirmation again; never retry or apply
automatically.

### 10.5 Artifacts, Show, And Undo

The batch digest confirms only the Projection Batch; it does not bind the
artifact manifest. After successful apply and artifact-list review, create
OverlayFiles through the mount:

```bash
python3 "$SKILL_DIR/scripts/scrapefs_media.py" artifacts \
  --manifest /tmp/dune.artifacts.json \
  --mountpoint /mnt/scrapefs
```

The tool uses the invoking user's permissions to create a temporary OverlayFile
in the destination directory, fsync it, and publish it with no-replace rename.
It never overwrites an existing file. Each artifact reports independently; a
partial NFO or artwork failure does not roll back the successful batch or other
artifacts. Resolve the cause and retry only failed entries.

```bash
python3 "$SKILL_DIR/scripts/scrapefs_media.py" show batch-42
python3 "$SKILL_DIR/scripts/scrapefs_media.py" undo batch-42
```

Undo removes only remaining batch-owned Projections. NFO, artwork, and other
OverlayFiles remain and must be handled by the user through ordinary FUSE
operations based on the undo result.

The daemon and control CLI provide shared status, doctor, and runtime
diagnostics. This Skill does not duplicate or replace those commands.
