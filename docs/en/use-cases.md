# First-Version Use Cases

**English** | [简体中文](../zh-CN/use-cases.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

This document describes the main first-version workflows from an actor-goal perspective. Data structures, error codes, and transaction semantics are defined by the linked specifications.

## 1. Actors

| Actor | Description |
| --- | --- |
| Administrator | Manages Libraries, the daemon, and system configuration |
| Control user | Creates Projections, runs Scrapes, and confirms or undoes results |
| Agent | Identifies media and generates Scrape revisions inside a restricted session |
| Media player | Reads ProjectionFiles and creates or modifies OverlayFiles through FUSE |
| Daemon | Performs validation, persistence, FUSE operations, and recovery |

## 2. Use-Case Index

| ID | Use case |
| --- | --- |
| UC-01 | Create a Library |
| UC-02 | Create one Projection manually |
| UC-03 | Scrape a movie directory |
| UC-04 | Answer an identification ambiguity |
| UC-05 | Revise a Scrape target path |
| UC-06 | Apply a Scrape |
| UC-07 | Undo a Scrape |
| UC-08 | Rename or remove a Projection |
| UC-09 | Atomically update an OverlayFile |
| UC-10 | Handle a missing or replaced source file |
| UC-11 | Rebind a Projection to a new source |
| UC-12 | Remove a Library |
| UC-13 | Recover after daemon restart |
| UC-14 | Diagnose problems with doctor |

## UC-01 Create a Library

### Actor

Administrator

### Goal

Create a virtual media Library using a selected Profile.

### Preconditions

- The daemon is ready.
- No ACTIVE Library uses the requested name.
- The Profile is built in and available.

### Trigger

```bash
scrapefs library add jellyfin --profile jellyfin@1
```

### Main Flow

1. The CLI submits the name and Profile.
2. The daemon validates both values.
3. One transaction creates the LibraryRoot and Library record.
4. The daemon logs the operation and increments namespace version.
5. `/mnt/scrapefs/jellyfin` appears through FUSE.

### Alternative Flows

- Name already exists: return `EEXIST`.
- Profile is unavailable: return `ProfileNotFound`.
- Transaction fails: leave no LibraryRoot.

### Result

The Library is ACTIVE and can receive Projections and Scrapes.

### Related Specifications

- [Architecture](architecture.md)
- [Operations and CLI](operations.md)

## UC-02 Create One Projection Manually

### Actor

Control user

### Goal

Map an existing media file into a Library without modifying the source.

### Preconditions

- The daemon is ready.
- The target Library is ACTIVE.
- The caller has control permission.
- The daemon can read the source file.
- The target path is free.

### Trigger

```bash
scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"
```

### Main Flow

1. The daemon safely opens the source.
2. It confirms a regular file.
3. It records source path, identity, and cached attributes.
4. It validates the Library target path.
5. One transaction creates missing directories and the ProjectionFile.
6. The daemon logs the operation and increments namespace version.
7. The virtual path becomes visible and readable.

### Alternative Flows

- Source is missing: return `ENOENT`.
- Source path contains a symbolic link: reject it.
- Target exists: return `EEXIST`.
- Library is missing: return `LibraryNotFound`.
- Transaction fails: leave no partial directory or ProjectionFile.

### Result

The ProjectionFile is readable and read-only. The source file is unchanged.

### Related Specifications

- [Projection model](projection.md)
- [Filesystem and object storage](filesystem.md)

## UC-03 Scrape a Movie Directory

### Actors

Control user, Agent

### Goal

Identify an arbitrarily named movie directory and produce a reviewable Projection draft.

### Preconditions

- The daemon is ready.
- The target Library is ACTIVE.
- The daemon can read the selected directory.
- The Agent provider is available.

### Trigger

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

### Main Flow

1. The daemon enumerates only the selected directory and required descendants.
2. It creates an immutable Scrape file manifest.
3. It creates a restricted Agent session.
4. The Agent reads filenames, existing text metadata, and controlled probe results.
5. The Agent generates MediaModel.
6. The Jellyfin Profile generates RenderedEntries.
7. The daemon validates path safety and conflicts.
8. The Scrape becomes ready.
9. The CLI shows media identity, the Projection tree, ignored files, and warnings.

### Alternative Flows

- Input is one regular file: inspect only that file.
- Agent cannot identify the media: move to waiting_input.
- A source changes during analysis: return `ESTALE` for the affected operation.
- Path-safety validation fails: do not enter ready.
- Agent fails: retain draft and record the error.

### Result

An unapplied Scrape revision exists. The FUSE namespace is unchanged.

### Related Specifications

- [Scrape and Agent workflow](scrape.md)
- [Projection model](projection.md)

## UC-04 Answer an Identification Ambiguity

### Actors

Control user, Agent

### Goal

Resolve an uncertain work, season/episode, or file role.

### Preconditions

- Scrape is waiting_input.
- The Question belongs to the current revision.

### Trigger

```bash
scrapefs scrape answer scrape-42 question-1 candidate-2021
```

### Main Flow

1. The daemon validates that the Question belongs to the current revision.
2. It stores the Answer.
3. The Agent regenerates MediaModel.
4. The Profile rerenders target paths.
5. The daemon reruns safety and conflict validation.
6. It creates a new revision.
7. With no remaining Questions, Scrape becomes ready.

### Alternative Flows

- Question belongs to an old revision: reject the Answer.
- Questions remain: keep waiting_input.
- Rerendering fails: return to draft and record the error.

### Result

A new immutable revision becomes current. The previous revision remains available for diff.

### Related Specifications

- [Scrape and Agent workflow](scrape.md)

## UC-05 Revise a Scrape Target Path

### Actor

Control user

### Goal

Correct a target path generated by the Agent or Profile before apply.

### Preconditions

- Scrape is draft, waiting_input, or ready.
- The new target remains in the same Library.

### Trigger

```bash
scrapefs scrape revise scrape-42 \
  --target rendered-01="Movies/Dune (2021)/Dune (2021).mkv"
```

### Main Flow

1. The daemon reads the current revision.
2. It applies the manual override.
3. It runs mandatory path-safety validation.
4. The Profile produces naming warnings when necessary.
5. The daemon creates a new revision.
6. The CLI shows the diff from the previous revision.

### Alternative Flows

- Path contains `..` or escapes LibraryRoot: reject it.
- Target conflicts with an existing node: do not enter ready.
- Scrape is applied, undone, or discarded: reject modification.

### Result

The new revision becomes current and requires new confirmation at apply.

### Related Specifications

- [Scrape and Agent workflow](scrape.md)

## UC-06 Apply a Scrape

### Actor

Control user

### Goal

Atomically publish the Scrape revision the user just confirmed.

### Preconditions

- Scrape is ready.
- The CLI displayed the current revision.
- The caller confirmed apply.
- Library is ACTIVE.

### Trigger

```bash
scrapefs scrape apply scrape-42
```

### Main Flow

1. The CLI submits Scrape ID and expected revision.
2. The daemon obtains the confirming UID/GID from Unix peer credentials.
3. It verifies the revision did not change.
4. It revalidates selected source identities.
5. It rechecks path safety and conflicts.
6. One SQLite transaction creates Directory and ProjectionFile nodes.
7. It writes the operation log.
8. It marks Scrape applied and stores the confirming UID/GID.
9. It increments namespace version and commits.
10. It updates the in-memory index and invalidates FUSE caches.
11. The target tree becomes visible.

### Alternative Flows

- Revision changed: reject and require new review.
- Source changed: return `ESTALE`.
- Target conflicts: return `EEXIST`.
- Transaction fails: leave no partial Projection batch.
- Scrape is already applied: return `AlreadyApplied`.

### Result

All Projections are published together. Source files remain unchanged. Later Agent artifacts use the saved confirming UID/GID as their virtual owner.

### Related Specifications

- [Scrape and Agent workflow](scrape.md)
- [Architecture](architecture.md)

## UC-07 Undo a Scrape

### Actor

Control user

### Goal

Remove Projections created by one Scrape while retaining sources and OverlayFiles.

### Preconditions

- Scrape is applied.

### Trigger

```bash
scrapefs scrape undo scrape-42
```

### Main Flow

1. The daemon stops new Agent artifact requests for this Scrape.
2. One transaction removes ProjectionFiles with matching `origin_scrape_id`.
3. It removes empty managed directories.
4. It writes the operation log.
5. It marks Scrape undone.
6. It increments namespace version and commits.
7. The CLI lists retained Agent OverlayFiles.

### Alternative Flows

- A Projection was already removed: treat it as absent.
- A directory still contains OverlayFiles or other Projections: retain it.
- Scrape is already undone: return `AlreadyUndone`.

### Result

Scrape Projections disappear. Source files and OverlayFiles remain.

### Related Specifications

- [Scrape and Agent workflow](scrape.md)

## UC-08 Rename or Remove a Projection

### Actors

Control user or a FUSE user with permission

### Goal

Modify the virtual namespace without modifying the source.

### Preconditions

- ProjectionFile exists.
- Target remains in the same Library.
- Caller has permission.

### Trigger

```bash
scrapefs projection mv \
  "jellyfin:/Movies/Dune/Dune.mkv" \
  "jellyfin:/Movies/Dune/Dune (2021).mkv"

scrapefs projection rm \
  "jellyfin:/Movies/Dune/Dune (2021).mkv"
```

### Main Flow

1. The daemon validates the source node and target path.
2. One transaction moves or removes the virtual node.
3. It writes the operation log and increments namespace version.
4. After removal, it cleans empty managed directories.

### Alternative Flows

- Cross-Library rename: return `EXDEV`.
- Target exists: return `EEXIST`.
- Directory still contains content: retain it.

### Result

The virtual path changes or disappears. The source file remains unchanged.

### Related Specifications

- [Projection model](projection.md)

## UC-09 Atomically Update an OverlayFile

### Actor

Media player

### Goal

Safely update NFO, artwork, or subtitles through a temporary-file replacement pattern.

### Preconditions

- Library is mounted.
- Player has write permission.
- Target is not a ProjectionFile.

### Trigger

```text
create movie.nfo.tmp
write
fsync
rename movie.nfo.tmp -> movie.nfo
```

### Main Flow

1. ScrapeFS creates an object in staging.
2. The object safely enters live.
3. SQLite creates the OverlayFile node.
4. The player writes and fsyncs.
5. OverlayFile atomically replaces an existing OverlayFile.
6. The old object is reclaimed after commit.

### Alternative Flows

- Target is ProjectionFile: return `EEXIST`.
- Backing write partially succeeds: retain the written bytes and return the actual length.
- Fsync fails: propagate the backing error.
- Object is missing: return `EIO`.

### Result

The player reads complete new content from the target path. Source files are unaffected.

### Related Specifications

- [Filesystem and object storage](filesystem.md)

## UC-10 Handle a Missing or Replaced Source

### Actors

Media player, control user

### Goal

Keep the namespace inspectable while preventing silent reads from the wrong file.

### Preconditions

- ProjectionFile exists.

### Trigger

The source is externally removed, moved, or replaced at the same path.

### Main Flow

1. lookup/getattr uses cached attributes and keeps the Projection visible.
2. open safely resolves the source and checks identity.
3. Missing source returns `ENOENT`.
4. Identity mismatch returns `ESTALE`.
5. doctor reports a broken or stale Projection.

### Result

ScrapeFS does not automatically remove the Projection and does not silently project a replacement file.

### Related Specifications

- [Projection model](projection.md)

## UC-11 Rebind a Projection to a New Source

### Actor

Control user

### Goal

Bind an existing virtual path to a legitimately moved or replaced source.

### Preconditions

- ProjectionFile exists.
- The new source can be safely read.

### Trigger

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv
```

### Main Flow

1. The daemon safely opens the new source.
2. The CLI displays old and new paths and identities.
3. The user explicitly confirms.
4. The daemon updates source path, identity, and cached attributes.
5. It updates virtual ctime, writes the log, and invalidates attributes.
6. New opens read from the new source.

### Alternative Flows

- New source is missing: return `ENOENT`.
- New source path contains a symbolic link: reject it.
- User cancels: do not modify the Projection.

### Result

The virtual path and inode remain stable while content comes from the new source.

### Related Specifications

- [Projection model](projection.md)

## UC-12 Remove a Library

### Actor

Administrator

### Goal

Remove an unused virtual media Library without deleting source files.

### Preconditions

- The daemon is ready.

### Trigger

```bash
scrapefs library remove jellyfin
```

### Main Flow

For an empty Library:

1. Remove LibraryRoot.
2. Mark Library RETIRED.
3. Write the operation log and increment namespace version.

For explicit recursive removal:

1. The administrator first cancels active sessions and discards unapplied Scrapes.
2. One transaction removes the Library node tree.
3. It clears `root_node_id` and marks Library RETIRED.
4. Overlay objects are reclaimed after commit.

### Alternative Flows

- Ordinary removal of non-empty Library: return `ENOTEMPTY`.
- Active Scrape or Agent session exists: recursive removal fails.

### Result

The Library disappears from the FUSE root. Source files remain.

### Related Specifications

- [Operations and CLI](operations.md)
- [Architecture](architecture.md)

## UC-13 Recover after Daemon Restart

### Actors

Administrator, systemd

### Goal

Restore virtual Libraries after a normal restart or crash.

### Preconditions

- SQLite and the object store remain available.

### Trigger

`scrapefsd` starts.

### Main Flow

1. Acquire the host-filesystem daemon lock for the ScrapeFS runtime.
2. Open SQLite and validate PRAGMAs.
3. Run migrations.
4. Recover object-store state.
5. Rebuild namespace and inode indexes from SQLite.
6. Clean a stale mount proven to belong to a dead instance.
7. Mount FUSE and create the control socket.
8. Signal readiness.

### Alternative Flows

- The daemon lock is unavailable: exit before opening SQLite.
- Migration fails: do not mount or signal readiness.
- Object is missing: retain the node and let doctor report it.
- Unknown FUSE mount exists: do not unmount it.

### Result

Committed Libraries, Projections, and OverlayFiles become visible again.

### Related Specifications

- [Operations and CLI](operations.md)
- [Filesystem and object storage](filesystem.md)

## UC-14 Diagnose Problems with Doctor

### Actors

Control user, administrator

### Goal

Find persistent or source-file problems that ScrapeFS does not repair automatically.

### Preconditions

- The daemon is ready, or doctor supports a read-only offline mode.

### Trigger

```bash
scrapefs doctor
```

### Main Flow

1. Check Projection source paths and identities.
2. Check that each OverlayFile has its live object.
3. Check for orphan objects with no node reference.
4. Check Library and namespace-tree constraints.
5. Output structured findings and suggested actions.

### Example Findings

```text
BROKEN_PROJECTION
STALE_PROJECTION
MISSING_OBJECT
ORPHAN_OBJECT
INVALID_NAMESPACE
```

### Result

Doctor does not modify source files or the namespace by default. Repairs require an explicit user operation.

### Related Specifications

- [Projection model](projection.md)
- [Filesystem and object storage](filesystem.md)
- [Operations and CLI](operations.md)
