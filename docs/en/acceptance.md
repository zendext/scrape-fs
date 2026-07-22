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

## 2. Scrape

- `scrape submit` accepts only a versioned UTF-8 JSON plan.
- Unknown fields, invalid types, and empty entries are rejected.
- Each entry explicitly contains an absolute source and Library-relative target.
- The daemon does not enumerate directories, read source contents, run commands, or access the network on behalf of a plan.
- Submit and revise safely open and snapshot every source.
- Revise creates an immutable revision from a complete replacement plan.
- Apply matches the revision the user just confirmed.
- A changed revision invalidates the old confirmation request.
- Apply revalidates every source identity, Profile, path, and conflict.
- Apply leaves no partial Projection batch.
- Failed apply leaves Scrape ready.
- Applied Scrape cannot apply again.
- Undo runs in one transaction.
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
- The external Agent submits an explicit source-to-target plan using `scrapefs.scrape/v1`.
- Automation commands provide machine-readable JSON, and non-TTY mode does not prompt or apply automatically.
- After the plan is applied, the external Agent creates OverlayFiles through ordinary FUSE operations.
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
- Replacing a configured database ancestor after path validation cannot redirect SQLite, and rejecting a newer schema version does not change its journal mode.
- Readiness is sent only after database, index, mount, and socket are available.
- An unmounted mountpoint does not expose a readable empty directory.
- Normal shutdown does not use lazy unmount.
- Daemon restart recovers from SQLite and the object store.

## 8. First-Version Agent Skill Scope

The skill should cover:

- single-file movies;
- one movie per directory;
- standard SxxEyy episodes;
- multiple episodes in one season;
- ordinary external subtitles;
- sample-file exclusion;
- Plex and Jellyfin;
- reading existing NFO files;
- generating `scrapefs.scrape/v1`;
- plan revise, diff, validate, apply, and undo;
- creating NFO files and artwork through FUSE after apply.

The skill may reject or require manual handling for:

- BDMV and DVD;
- anime absolute episode numbering;
- multi-episode files;
- automatic edition detection;
- complex extras;
- music and image libraries;
- remote network sources.

These format capabilities belong to the skill and do not change the ScrapeFS plan schema. ScrapeFS requires only that a Projection source in the plan is a safely openable local regular file.
