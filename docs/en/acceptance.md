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

- Scrape enumerates only the user-selected path.
- A file input does not trigger directory traversal.
- The Scrape file manifest is immutable after creation.
- The Agent references only file IDs from the current Scrape.
- Answer completion triggers new analysis, rendering, and validation.
- Apply matches the revision the user just confirmed.
- A changed revision invalidates the old confirmation request.
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

## 5. Agent

- Agent cannot apply, undo, discard, or manage Libraries.
- Agent cannot submit an arbitrary absolute path.
- Agent reads only files from the current Scrape.
- Agent cannot create visible artifacts before apply.
- Agent writes only below the current Scrape artifact root.
- Agent cannot overwrite an existing file.
- Session cancellation or expiry, Scrape undo, and Library retirement stop further writes.
- Agent upload quotas are enforced.

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
- Agent-created OverlayFiles use the apply confirmer's UID/GID and the Library overlay mode.
- Overlay backing objects use daemon ownership and `0600` and do not follow virtual chmod/chown.
- ProjectionFile is always read-only.
- The daemon acquires `/run/scrapefs/scrapefsd.lock` on the host filesystem before opening SQLite.
- Readiness is sent only after database, index, mount, and socket are available.
- An unmounted mountpoint does not expose a readable empty directory.
- Normal shutdown does not use lazy unmount.
- Daemon restart recovers from SQLite and the object store.

## 8. First-Version Agent Scope

Should support:

- single-file movies;
- one movie per directory;
- standard SxxEyy episodes;
- multiple episodes in one season;
- ordinary external subtitles;
- sample-file exclusion;
- Plex and Jellyfin;
- reading existing NFO files;
- structured questions;
- manual revision and path override.

May return `UnsupportedFeature` for:

- BDMV and DVD;
- anime absolute episode numbering;
- multi-episode files;
- automatic edition detection;
- complex extras;
- music and image libraries;
- remote network sources.
