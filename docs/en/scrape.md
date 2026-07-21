# Scrape and Agent Workflow

**English** | [简体中文](../zh-CN/scrape.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

A Scrape is an Agent-assisted workflow that generates a reviewable batch of Projections from a user-selected file or directory.

## 1. Create a Scrape

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

A Scrape does not move or reorganize the source directory.

For a regular-file input, the daemon checks only that file. For a directory input, the daemon enumerates only that directory and the descendants required for media identification.

## 2. Scrape Data

```text
scrapes
├── id
├── source_path
├── library_id
├── profile_name
├── profile_version
├── current_revision
├── applied_revision
├── status
├── validated_namespace_version
├── confirmed_by_uid
├── confirmed_at
├── last_error
├── created_at
└── updated_at
```

States:

```text
draft
waiting_input
ready
applied
undone
discarded
```

## 3. Scrape File Manifest

After enumeration, the daemon stores:

```text
scrape_files
├── id
├── scrape_id
├── source_path
├── relative_path
├── parent_id
├── name
├── file_type
├── device_id
├── inode
├── mode
├── size
├── atime_ns
├── mtime_ns
├── ctime_ns
└── created_at
```

`file_type` is one of:

```text
regular
directory
unsupported
```

Rules:

- the manifest belongs only to the current Scrape;
- the completed manifest is immutable;
- the Agent may reference only regular files in the manifest;
- a new directory read creates a new Scrape;
- there is no separate Source Scan domain model.

## 4. Media Probing

The Agent requests controlled text reads or `ffprobe` through `scrape_file_id`.

The daemon safely reopens the file and validates device, inode, size, and mtime.

The Agent cannot provide an arbitrary local path, command-line arguments, or shell command.

Probe execution has a timeout and maximum output size. The first version does not maintain a generic Probe history.

## 5. Agent Session

```text
agent_sessions
├── id
├── scrape_id
├── library_id
├── status
├── token_hash
├── expires_at
├── created_at
└── updated_at
```

States:

```text
ANALYSIS
ARTIFACT
CANCELLED
EXPIRED
COMPLETED
```

The Agent may:

- read the current Scrape manifest;
- request controlled text reads and ffprobe;
- generate MediaModel;
- generate structured Questions;
- generate a new revision from Answers.

The Agent cannot:

- read outside the current Scrape;
- submit an arbitrary absolute path;
- modify source files;
- access SQLite, the object store, or FUSE directly;
- apply, undo, or discard a Scrape;
- manage Libraries.

An external Agent worker requires both the configured Agent Unix UID and the current session token.

## 6. MediaModel

```yaml
schema: scrapefs.media/v1

media:
  kind: movie | series
  title: string
  original_title: string | null
  year: integer | null
  provider_ids:
    tmdb: string | null
    tvdb: string | null
    imdb: string | null
  edition: string | null
  version: string | null

items:
  - id: media-item-1
    scrape_file_id: file-01
    role: primary_video | episode | subtitle | extra | metadata_source
    season: integer | null
    episodes: [integer]
    language: string | null
    target_item_ids: [media-item-id]
    subtitle:
      default: boolean
      forced: boolean
      sdh: boolean
    extra_type: trailer | featurette | interview | sample | other | null

ignored:
  - scrape_file_id: file-99
    reason: string
```

Constraints:

- a movie has at least one primary video;
- an episode has a season and at least one episode number;
- a subtitle explicitly references its target video or episode;
- every scrape_file_id belongs to the current Scrape;
- only regular files can become Projections.

## 7. Profile Output

```yaml
schema: scrapefs.rendered-entry/v1

id: rendered-01
scrape_file_id: file-01
media_item_id: media-item-1
kind: projection
target: Movies/Dune (2021)/Dune (2021).mkv
artifact_root: Movies/Dune (2021)
manual_override: false
warnings: []
```

`target` and `artifact_root` are relative to LibraryRoot.

Profile interface:

```go
type Profile interface {
    Name() string
    Version() string

    Render(MediaModel, ScrapeFileManifest) (RenderResult, error)
    ValidatePathSafety(relativePath string) error
    ValidateConformance(entry RenderedEntry) []Warning
}
```

Path-safety errors cannot be overridden. Nonconforming player names produce warnings shown at final confirmation.

## 8. Revision

Initial Agent analysis, analysis after an Answer, user revision, manual path override, and Profile rerendering create immutable revisions.

```text
scrape_revisions
├── scrape_id
├── revision
├── media_model
├── rendered_entries
├── questions
├── answers
├── warnings
├── created_by
└── created_at
```

After showing a revision, the CLI sends its expected revision number with apply. A changed current revision causes apply to fail and requires new confirmation.

The first version has no Approval table and no cryptographic revision hash.

## 9. State Transitions

```text
none -> draft
draft -> waiting_input
draft -> ready
draft/waiting_input/ready -> discarded
waiting_input -> draft
ready -> draft
ready -> applied
applied -> undone
applied/undone -> clone as a new draft
```

Answering all Questions does not directly produce ready. Agent analysis, Profile rendering, path safety, and conflict validation run again.

Discarding an unapplied Scrape cancels its Agent session.

## 10. Diff and Export

`scrape diff` compares the current revision with the previous revision.

`scrape diff --against namespace` shows:

- Projections to create;
- directories to create or reuse;
- existing paths;
- path conflicts;
- naming warnings.

`scrape export` uses:

```yaml
schema: scrapefs.scrape/v1
```

It exports the current Scrape, file manifest, MediaModel, and RenderedEntries for diagnostics and review. The first version has no import.

## 11. Apply

Apply verifies:

- the caller has control permission;
- Scrape is ready;
- expected revision matches current revision;
- selected source identities still match;
- Library is ACTIVE;
- Profile is available;
- path-safety validation passes;
- target paths do not conflict.

Atomic flow:

1. read the ready Scrape and expected revision;
2. revalidate source identities and target paths;
3. construct the candidate namespace change;
4. start a SQLite write transaction;
5. recheck conflicts;
6. create Directory and ProjectionFile nodes;
7. write the operation log;
8. set Scrape to applied;
9. store applied revision and confirming user;
10. increment namespace version;
11. commit;
12. update the in-memory index and invalidate FUSE caches.

Steps 4 through 11 share one transaction.

Repeated apply returns `AlreadyApplied`.

## 12. Undo

One SQLite transaction:

1. removes ProjectionFiles whose `origin_scrape_id` matches;
2. removes empty managed directories toward LibraryRoot;
3. writes the operation log;
4. sets Scrape to undone;
5. increments namespace version.

Undo does not remove OverlayFiles. The CLI lists remaining Agent artifacts from that Scrape.

Repeated undo returns `AlreadyUndone`.

## 13. Agent Artifacts

After apply, the daemon resolves Profile artifact roots to node IDs and moves the session to ARTIFACT.

```text
scrape_artifact_roots
├── scrape_id
├── revision
├── root_node_id
└── relative_path_snapshot
```

The daemon restores this scope after restart.

The Agent creates files only below allowed roots and cannot overwrite an existing node.

First-version API:

```text
overlay.put_text
overlay.put_stream
overlay.stat
```

Scrape undo, session cancellation or expiry, and Library retirement immediately stop new artifact writes without deleting committed OverlayFiles.

Recommended quotas:

```text
max_text_file_size     = 4 MiB
max_binary_file_size   = 32 MiB
max_session_total_size = 256 MiB
max_upload_duration    = 120s
```
