# Scrape Plans and the External Agent Workflow

**English** | [简体中文](../zh-CN/scrape.md)

A Scrape is a set of Projections submitted by an external tool, validated by ScrapeFS, and applied atomically.

Media identification does not run inside `scrapefsd`. An external Agent skill, script, or manual tool reads source directories, runs `ffprobe`, queries metadata, resolves ambiguity, and selects target paths. ScrapeFS is responsible only for deterministic source validation, path validation, conflict checks, review, apply, and undo.

## Difference from the Earlier Specification

The earlier specification planned:

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

That command implied that the daemon would enumerate the source directory, run Agent analysis, and generate Projections. The behavior was never implemented and is removed from the target v1 CLI contract without a compatibility alias.

Responsibilities are now split as follows:

1. An external Agent or another tool performs media scraping and generates a `scrapefs.scrape/v1` plan.
2. `scrapefs scrape submit` sends the plan to the daemon.
3. The daemon owns review data, revisions, validation, atomic apply, and undo.

Therefore, Scrape in the current documentation means the plan lifecycle, not a media-identification process running inside the daemon.

## 1. External Agent Boundary

The external Agent:

- reads the host filesystem with the invoking control user's identity;
- chooses its own model, prompts, metadata providers, and media-probing tools;
- produces an explicit source-to-target Projection plan;
- submits and reviews the plan through the `scrapefs` CLI;
- creates NFO files, artwork, and other OverlayFiles through ordinary FUSE operations after the plan is applied.

ScrapeFS does not provide:

- an embedded model provider;
- a daemon-side Agent session or session token;
- a model tool-calling loop;
- arbitrary file-read, `ffprobe`, or network-query Agent APIs;
- a MediaModel, Question/Answer loop, or Agent-specific Overlay API.

The external Agent is not an additional authorization boundary. It can use only its own host-filesystem permissions, control CLI permissions, and FUSE permissions.

## 2. Plan Format

The first version accepts UTF-8 JSON only. The CLI reads a versioned plan from a file or stdin:

```bash
scrapefs scrape submit --file /tmp/dune.scrape.json
scrapefs scrape submit --file -
```

```json
{
  "schema": "scrapefs.scrape/v1",
  "library": "jellyfin",
  "entries": [
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.2021.REMUX.mkv",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021).mkv"
    },
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.zh.ass",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021).zh.ass"
    }
  ],
  "ignored": [
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/sample.mkv",
      "reason": "sample video"
    }
  ]
}
```

Rules:

- `schema` is `scrapefs.scrape/v1`;
- `library` references an ACTIVE Library;
- `entries` is non-empty;
- `source` is a lexically normalized absolute local path;
- `target` is relative to LibraryRoot and passes generic and Profile validation;
- one plan cannot contain a duplicate target;
- `ignored` is review-only, creates no node, and does not authorize the daemon to read that path;
- unknown fields and invalid types are rejected;
- the first version does not support YAML or cross-host path translation.

## 3. Submit and Source Snapshots

`scrape submit` is called by a control user. Control users are trusted source-path selectors.

For each entry, the daemon:

1. opens `source` through the secure source-file primitive;
2. rejects symlinks, magic links, directories, and other non-regular files;
3. stores the normalized path, device, inode, size, mode, and nanosecond timestamps;
4. closes descriptors that are no longer needed after submission;
5. safely reopens and revalidates the identity before apply.

A plan cannot ask the daemon to enumerate a parent directory, read file contents, run a command, or access the network.

## 4. Scrape Data

```text
scrapes
├── id
├── library_id
├── profile_name
├── profile_version
├── current_revision
├── applied_revision
├── status
├── validated_namespace_version
├── confirmed_by_uid
├── confirmed_by_gid
├── confirmed_at
├── last_error
├── created_at
└── updated_at
```

Statuses:

```text
draft
ready
applied
undone
discarded
```

Schema or authorization errors reject submission without creating a Scrape. Reviewable path conflicts, Profile warnings, or source-state problems are retained in a draft revision. A plan without blocking errors becomes ready.

## 5. Revisions

Each `submit` creates revision 1. `scrape revise` creates a new immutable revision from a complete replacement plan:

```bash
scrapefs scrape revise scrape-42 --file corrected.json
```

```text
scrape_revisions
├── scrape_id
├── revision
├── plan
├── source_snapshots
├── validation_result
├── warnings
├── created_by_uid
└── created_at
```

After the CLI displays a revision, apply includes the expected revision. The daemon rejects apply when the current revision has changed and requires another review.

State transitions:

```text
none -> draft
none -> ready
draft -> ready
draft/ready -> draft
draft/ready -> discarded
ready -> applied
applied -> undone
```

An applied, undone, or discarded Scrape cannot be revised.

## 6. Validate, Diff, and Export

```text
scrapefs scrape show scrape-42
scrapefs scrape validate scrape-42
scrapefs scrape diff scrape-42
scrapefs scrape diff scrape-42 --against namespace
scrapefs scrape export scrape-42
```

`validate` rechecks the Library, Profile, source identities, path safety, and namespace conflicts and creates an immutable revision with the same plan plus fresh source snapshots and validation results. It does not modify an existing revision.

`diff --against namespace` displays:

- Projections to create;
- managed directories to create or reuse;
- existing paths;
- path conflicts;
- Profile warnings;
- ignored sources.

`export` returns only the current `scrapefs.scrape/v1` plan in a form that can be submitted again. Daemon-owned source snapshots, validation results, and namespace diffs are available through `show --json` and `diff --json` and cannot bypass validation on resubmission.

Every automation-facing command supports machine-readable JSON output. A non-TTY invocation or explicit machine-readable mode does not prompt or apply automatically.

## 7. Apply

Before apply, the daemon confirms:

- the caller has control permission;
- the Scrape is ready;
- the expected revision equals the current revision;
- every source identity still matches;
- the Library is ACTIVE;
- the Profile version is available;
- every path-safety check passes;
- no target path conflicts.

Atomic flow:

1. Read the ready Scrape and expected revision.
2. Safely reopen and revalidate every source.
3. Rerun generic and Profile path validation.
4. Build the complete candidate namespace change.
5. Start a SQLite write transaction.
6. Recheck path conflicts.
7. Create all Directories and ProjectionFiles.
8. Write one batch operation-log entry.
9. Mark the Scrape applied.
10. Store the applied revision and confirming UID/GID.
11. Increment namespace version once.
12. Commit.
13. Update the in-memory index and invalidate FUSE caches.

Steps 5 through 12 use one transaction. The plan implementation cannot simulate an atomic batch by repeatedly invoking a direct Projection operation that commits independently.

Repeated apply of an applied Scrape returns `AlreadyApplied`.

## 8. Undo

Undo uses one SQLite transaction to:

1. remove ProjectionFiles whose `origin_scrape_id` matches;
2. prune empty managed directories toward LibraryRoot;
3. write the operation log;
4. mark the Scrape undone;
5. increment namespace version once.

Undo does not remove OverlayFiles created through FUSE. The CLI lists OverlayFiles that prevent directory pruning or remain below former target directories.

Repeated undo of an undone Scrape returns `AlreadyUndone`.

## 9. Overlay Artifacts

After the plan is applied, the external Agent can create artifacts through ordinary FUSE operations:

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
- Scrape undo does not automatically remove OverlayFiles;
- ScrapeFS does not distinguish files created by a person, script, or Agent.

## 10. Agent Skill Contract

The external Agent skill documents:

- supported media, subtitle, and local metadata formats;
- source enumeration and sample-ignore rules;
- `ffprobe` and metadata-query behavior;
- Plex and Jellyfin Profile naming rules;
- `scrapefs.scrape/v1` generation;
- submit, show, diff, revise, validate, apply, and undo workflows;
- the user confirmation boundary;
- FUSE OverlayFile creation after apply;
- recovery from `ESTALE`, path conflicts, and partial artifact failures.

The skill is not a daemon plugin. Updating the skill cannot change the ScrapeFS authorization model, plan schema, or path-safety rules.
