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

- contains no `..`, NUL, or empty component;
- remains below LibraryRoot;
- uses no reserved name;
- stays within configured length limits;
- passes the Library Profile's path-safety validation.

`projection add` uses one SQLite transaction to:

1. validate the source file;
2. create missing managed directories;
3. create the ProjectionFile;
4. write the operation log;
5. increment namespace version.

A conflict leaves no partial directory or node.

After `projection rm`, ScrapeFS should remove empty managed directories toward LibraryRoot. Directories created by users or players are retained.

## 2. Source-Path Safety

The first version accepts absolute source paths and has no Source Root registry or logical disk aliases.

Control users allowed to run `projection add` or `scrape` are trusted source-path selectors. The daemon service account should have read access only to intended media directories.

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

Rebind:

1. safely opens the new source;
2. displays the old and new paths and identities;
3. requires explicit confirmation;
4. updates path, identity, cached attributes, and `verified_at_ns`;
5. updates virtual ctime;
6. writes the operation log;
7. invalidates the inode attribute cache.

Rebind does not increment namespace version.

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

Rename changes only the virtual path. Unlink removes only the namespace node. An already opened source fd remains readable.

## 7. Rename

The first version supports `flags=0` and `RENAME_NOREPLACE`.

`RENAME_NOREPLACE` with an existing target returns `EEXIST`.

Within one Library, ProjectionFile may be renamed to a missing target.

ProjectionFile cannot replace another ProjectionFile, OverlayFile, or directory.

FUSE rename and `projection mv` across Libraries return `EXDEV`.
