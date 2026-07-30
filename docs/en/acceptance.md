# First-Version Acceptance Criteria

**English** | [简体中文](../zh-CN/acceptance.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

## 1. Direct Projection

- `projection add` creates a mapping from any permitted local filesystem.
- The source path is safely opened and validated as a regular file.
- Missing parent directories and the ProjectionFile are created in one transaction.
- A target conflict leaves no partial directory or node.
- `projection rm` does not delete the source.
- Removing a Projection cleans empty managed directories.

## 2. Projection Batch

- A batch accepts only versioned UTF-8 JSON.
- Unknown fields, invalid types, empty entries, duplicate sources, and duplicate targets are rejected.
- Each entry explicitly contains an absolute source and Library-relative target.
- The daemon does not enumerate directories, read source contents, run commands, or access the network on behalf of a batch.
- Validate safely opens every source and returns the diff, warnings, digest, and namespace version.
- Validate writes nothing to SQLite and creates no pending batch, snapshot, or revision.
- The digest uses `sha256:<64 lowercase hex>`, is independent of JSON whitespace and object-field order, and preserves `entries` array order.
- Apply requires the expected digest and revalidates every source, Profile, path, and conflict.
- Apply creates provenance and every Projection in one transaction without partial results.
- Failed apply leaves no batch provenance.
- Successful apply returns a stable batch ID.
- A Batch-owned Projection can be removed individually while the Batch remains applied and its original entry remains available.
- Undo runs in one transaction.
- Undo removes only remaining Batch-owned Projections and succeeds when none remain.
- Undo does not remove OverlayFiles.

## 3. ProjectionFile

- Projections work across physical disks.
- ProjectionFile supports read and seek.
- O_WRONLY, O_RDWR, O_TRUNC, write, and truncate return `EROFS`.
- Rename does not modify the source.
- Unlink does not delete the source.
- An open handle remains readable after unlink.
- Rename across Libraries returns `EXDEV`.
- Missing source uses cached getattr attributes.
- Same-path source replacement causes open to return `ESTALE`.
- Inode remains stable after daemon restart.

## 4. OverlayFile

- Supports create, open, read, write, truncate, flush, fsync, and rename.
- Supports partial writes, random writes, and atomic single append.
- O_TRUNC completes before open returns.
- Flush does not promise persistence.
- Fsync propagates backing errors.
- Overlay may atomically replace Overlay but not Projection.
- O_CREAT|O_EXCL and RENAME_NOREPLACE return `EEXIST` for an existing target.
- fallocate, link, symlink, mknod, and mkfifo fail.
- Failed object creation leaves no visible node.
- Missing object returns `EIO`.
- GC does not delete referenced, actively opened, or grace-period objects.

## 5. External Agent Integration

- `scrapefsd` embeds no model provider, Agent session, or tool-calling loop.
- An external Agent uses only the invoking user's existing host-filesystem, control CLI, and FUSE permissions.
- The external Agent submits an explicit source-to-target batch using `scrapefs.projection-batch/v1`.
- Automation commands provide machine-readable JSON, and non-TTY mode does not prompt or apply automatically.
- After the batch is applied, the external Agent creates OverlayFiles through ordinary FUSE operations.
- ScrapeFS does not change OverlayFile semantics based on whether a person, script, or Agent created the file.

## 6. Library

- Each Library appears as a first-level directory under the FUSE root.
- Ordinary FUSE users cannot create, remove, or rename LibraryRoot.
- Changing Library Profile does not modify existing Projections.
- Ordinary removal of a non-empty Library returns `ENOTEMPTY`.
- Recursive removal does not delete source files.
- Recursive removal schedules Overlay objects for reclamation.

## 7. Permissions and Lifecycle

- The FUSE mount enables `default_permissions`.
- FUSE create uses caller UID/GID and kernel-processed mode.
- OverlayFiles created by an external Agent through FUSE use the FUSE caller's UID/GID and ordinary create mode.
- Overlay backing objects use daemon ownership and `0600` and do not follow virtual chmod/chown.
- ProjectionFile is always read-only.
- The daemon acquires `/run/scrapefs/scrapefsd.lock` on the host filesystem before opening SQLite.
- The configured administrator and members of `scrapefs-control` can traverse the complete control-socket path and connect before peer-credential authorization; ACL masks cannot hide missing owning-group access, restrictive named-user entries or intermediate ancestors are rejected, and the daemon preserves socket mode `0660` rather than adding world access.
- Control-socket bind and unlink remain anchored to the validated parent-directory descriptor, and replaced path ancestors cannot redirect the daemon to another socket inode.
- Replacing a configured database ancestor after path validation cannot redirect SQLite, and rejecting a newer schema version does not change its journal mode.
- Readiness is sent only after database, index, mount, and socket are available.
- An unmounted mountpoint does not expose a readable empty directory.
- Normal shutdown does not use lazy unmount.
- Daemon restart recovers from SQLite and the object store.

## 8. Status and Doctor

- `status` returns `scrapefs.status/v1` with daemon version/readiness, database schema, namespace, index, mount, socket, object-store, and ACTIVE Library summaries.
- After the control socket starts and before readiness notification succeeds, `status` still reports `ready=false`.
- Mount or socket runtime metadata validation failure prevents `status` from continuing to report ready.
- `doctor` returns `scrapefs.doctor/v1` with stable healthy, warning, and blocking severities.
- Doctor identifies missing/stale Projections, missing Overlay objects, orphaned/conflicting objects, invalid Library/namespace state, database corruption, index divergence, and unsafe runtime metadata.
- Findings use deterministic ordering and stable codes.
- Doctor does not invoke recovery, GC, rebind, or repair and does not modify SQLite, object-store, source-file, or runtime metadata.
- Administrators and control users can run status/doctor; other callers are rejected before business checks.
- The CLI calls only the control socket and does not read SQLite or the object store directly.

## 9. First-Version Agent Skill

The repository provides the installable and executable
`organize-scrapefs-media` Skill, covering:

- single-file movies;
- one movie per directory;
- standard SxxEyy episodes;
- multiple episodes in one season;
- ordinary external subtitles;
- sample-file exclusion;
- Plex and Jellyfin;
- reading existing NFO files;
- generating `scrapefs.projection-batch/v1`;
- batch validate, user confirmation, apply, show, and undo;
- creating NFO files and artwork through FUSE after apply.

In addition:

- plan and validate never apply or prompt in TTY or non-TTY use;
- apply first performs fresh validation and requires the user to explicitly
  return the exact validation digest;
- representative Plex and Jellyfin fixtures pass the real Projection Batch
  parser and validation service;
- a clean-copy installation runs without hidden repository dependencies;
- the Skill stores no daemon-side session, pending plan, provider credential,
  or privileged read capability.

The Skill rejects or requires manual handling for:

- BDMV and DVD;
- anime absolute episode numbering;
- multi-episode files;
- automatic edition detection;
- complex extras;
- music and image libraries;
- remote network sources.

These format capabilities belong to the skill and do not change the ScrapeFS batch schema. ScrapeFS requires only that a Projection source in the batch is a safely openable local regular file.
