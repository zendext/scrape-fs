# Projection Model

**English** | [简体中文](../zh-CN/projection.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

A Projection is a read-only mapping from an external source file to a virtual path in a Library.

## 1. Direct Creation

```bash
scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"
```

Target syntax:

```text
<library-name>:/<relative-path>
```

The relative path:

- is valid UTF-8;
- contains no `..`, NUL, or empty component;
- remains below LibraryRoot;
- uses no reserved name;
- limits each component to 255 bytes and the normalized relative path to 4095 bytes;
- passes the Library Profile's path-safety validation.

Generic validation rejects absolute or lexically non-normalized paths before namespace mutation. Profile-specific naming and path-safety validation runs afterward.

The built-in `jellyfin@1` and `plex@1` Profiles reject Unicode control characters. `jellyfin@1` also rejects Jellyfin's reserved `<`, `>`, `:`, `"`, `\`, `|`, `?`, and `*` characters; `/` is already rejected by generic component validation. Player naming recommendations are not hard path-safety errors.

`projection add` uses one SQLite transaction to:

1. validate the source file;
2. create missing managed directories;
3. create the ProjectionFile;
4. write the operation log;
5. increment namespace version.

A conflict leaves no partial directory or node.

Successful `projection add` returns `scrapefs.projection-result/v1` JSON containing the stable Projection node ID, Library, full virtual target, and cached source identity and attributes.

List Projections across every ACTIVE Library or scope the query to one Library:

```bash
scrapefs projection list
scrapefs projection list --library jellyfin
```

`projection list` returns `scrapefs.projection-list/v1` JSON. Results are deterministically ordered by Library name, relative target within the Library, and node ID, and include Projections created directly or by a batch. A missing requested Library returns `LibraryNotFound`. Listing reads only the persisted namespace and cached source metadata; it does not reopen sources, so a currently unavailable source does not remove its Projection from the result.

Move or remove a Projection without touching its source:

```bash
scrapefs projection mv \
  "jellyfin:/Movies/Dune/Dune.mkv" \
  "jellyfin:/Films/Dune.mkv"

scrapefs projection rm "jellyfin:/Films/Dune.mkv"
```

Move accepts only a missing target in the same Library. It preserves the stable node ID, source metadata, attributes, creator, and Batch provenance, creates missing managed target directories in the same transaction, writes one operation-log entry, and increments namespace version once. A move of a Batch-owned Projection associates that operation with the originating Batch without changing its original entries or digest, so Batch show presents the apply-time target followed by later move history. Cross-Library move returns `CrossLibrary`; any existing node at the target returns `TargetConflict`.

Remove deletes only the Projection node. It prunes empty managed directories toward LibraryRoot while retaining user-created and non-empty directories, writes one operation-log entry, and increments namespace version once. Removing a Batch-owned Projection associates the operation with its originating Batch. The immutable apply-time entry remains available with a null live node reference, and the Batch remains `applied` until explicit undo. A failed move or remove leaves the namespace, version, operation log, and every source file unchanged. Both commands return `scrapefs.projection-result/v1`.

Rebind remains a direct-Projection operation. It rejects a Projection whose `origin_scrape_id` identifies a Projection Batch with the stable `BatchOwnedProjection` error. A Batch-owned Projection can be moved within its Library or removed individually. Later Batch undo removes only the Batch-owned Projections that still exist and marks the Batch `undone`.

## 2. Source-Path Safety

The first version accepts absolute source paths and has no Source Root registry or logical disk aliases.

Control users that can submit source paths through direct Projection, rebind, or Projection Batch operations are trusted source-path selectors. The daemon service account should have read access only to intended media directories.

The daemon MUST:

1. require an absolute path;
2. call `openat2` relative to a root-directory fd, using the path without the leading slash;
3. reject symbolic links, magic links, `..`, and path escape;
4. open with read-only access;
5. use `fstat` to confirm a regular file;
6. continue reading through the opened fd.

Linux implementations should use:

```text
RESOLVE_BENEATH
RESOLVE_NO_MAGICLINKS
RESOLVE_NO_SYMLINKS
```

If safe `openat2` semantics are unavailable, the first version may refuse to start instead of falling back to a TOCTOU-prone string-path check.

After a successful safe open, the daemon stores the normalized absolute path.

## 3. Source Identity

ProjectionFile stores:

```text
source_path
source_device_id
source_inode
cached_size
cached_atime_ns
cached_mtime_ns
cached_source_ctime_ns
verified_at_ns
```

Strict identity comparison uses:

```text
device ID
inode
size
mtime_ns
```

Open flow:

1. safely open `source_path`;
2. call `fstat`;
3. compare the stored identity;
4. return `ESTALE` on mismatch;
5. otherwise return a read-only handle.

When the source path is unavailable:

- the ProjectionFile remains visible;
- getattr returns cached attributes;
- open returns the safe-open error, such as `ENOENT` or `ENODEV`;
- doctor reports a broken projection.

## 4. Rebind

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv
```

The first invocation is a preview. It returns `scrapefs.projection-rebind-result/v1` with status `confirmation_required`, the previous and proposed source metadata, and a digest bound to the stable node ID, target, both source paths, and both strict identities. Apply exactly that preview with:

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv \
  --confirm sha256:<digest>
```

Rebind:

1. safely opens the new source;
2. returns the old and new paths, identities, and explicit confirmation digest;
3. reopens and revalidates the source when the digest is submitted;
4. rejects a changed Projection or source with `ConfirmationMismatch`;
5. updates path, identity, cached attributes, and `verified_at_ns`;
6. updates virtual ctime;
7. writes the operation log;
8. invalidates the inode attribute cache.

Preview and failed confirmation do not mutate state. Successful rebind preserves the node ID and target, writes one audit entry, and does not increment namespace version.

## 5. Attributes

ProjectionFile getattr returns:

```text
type    = regular file
mode    = Library.projection_file_mode
uid/gid = Library.uid/gid
size    = online fstat.size, otherwise cached_size
atime   = online fstat.atime, otherwise cached_atime_ns
mtime   = online fstat.mtime, otherwise cached_mtime_ns
ctime   = max(cached_source_ctime_ns, virtual_ctime_ns)
inode   = ScrapeFS inode
nlink   = 1
```

When the source is missing, unavailable, or stale, lookup and getattr use cached attributes. Missing or corrupt cache data returns `EIO`.

## 6. FUSE Semantics

Supported:

```text
open read-only
read
release
getattr
rename
unlink
```

| Operation | Error |
| --- | --- |
| open(O_WRONLY) | EROFS |
| open(O_RDWR) | EROFS |
| open(O_TRUNC) | EROFS |
| write | EROFS |
| truncate | EROFS |
| fallocate | EOPNOTSUPP |
| chmod/chown/utimens | EROFS |
| link/symlink | EOPNOTSUPP |

Rename changes only the virtual path and is allowed for direct and Batch-owned Projections. Unlink removes the direct or Batch-owned Projection namespace node without touching its source. An already opened source fd remains readable.

## 7. Rename

The first version supports `flags=0` and `RENAME_NOREPLACE`.

`RENAME_NOREPLACE` with an existing target returns `EEXIST`.

Within one Library, ProjectionFile may be renamed to a missing target.

ProjectionFile cannot replace another ProjectionFile, OverlayFile, or directory.

FUSE rename and `projection mv` across Libraries return `EXDEV`.
