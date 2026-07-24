# Filesystem and Object Storage

**English** | [简体中文](../zh-CN/filesystem.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

This document defines Directory and OverlayFile behavior, permissions, rename rules, object storage, and caching. Projection-specific behavior is defined in the [Projection model](projection.md).

Implementation status: Directory and OverlayFile FUSE mutation and I/O are implemented. ProjectionFile content, rename, and unlink remain a separate implementation unit.

## 1. Attributes

Directory mode, uid, gid, atime, mtime, and virtual ctime are stored in SQLite.

Creating, removing, or moving a child updates the affected parent-directory mtime and virtual ctime.

OverlayFile getattr returns:

```text
type    = regular file
mode    = SQLite mode
uid/gid = SQLite uid/gid
size    = object fd fstat.size
atime   = object fd fstat.atime
mtime   = object fd fstat.mtime
ctime   = max(object ctime, virtual_ctime_ns)
inode   = ScrapeFS inode
nlink   = 1
```

Metadata changes invalidate the inode attribute cache.

The first version uses noatime or approximately equivalent behavior.

## 2. Permissions

The FUSE mount enables:

```text
allow_other
default_permissions
nodev
nosuid
noexec
```

The kernel enforces normal access permissions using mode, uid, and gid.

For an OverlayFile or Directory created through FUSE:

- UID/GID come from the FUSE request context;
- mode is the kernel-supplied mode after umask;
- the daemon does not apply umask again.

Virtual UID/GID/mode are stored in SQLite and are independent from the backing object's Unix owner and mode.

An external Agent creates OverlayFiles through ordinary FUSE operations and therefore follows the same FUSE request context, umask, and permission checks. ScrapeFS provides no Agent controller that bypasses these rules.

Defined errors:

- ProjectionFile chmod/chown: `EROFS`;
- GlobalRoot/LibraryRoot chmod/chown: `EPERM`;
- creating an entry under GlobalRoot: `EPERM`;
- ordinary Directory/OverlayFile: allowed by normal permissions or rejected by the kernel.

LibraryRoot itself remains protected, while callers with normal kernel permission may create and remove ordinary children below it.

## 3. OverlayFile Operations

Supported:

```text
create
open
read
write
truncate
flush
fsync
release
rename
unlink
chmod
chown
utimens
getattr
```

`fallocate` returns `EOPNOTSUPP`.

Write semantics:

- ordinary write uses `pwrite`-equivalent offset semantics;
- partial and non-sequential writes are supported;
- a partially successful backing write remains visible and returns its actual length;
- one `O_APPEND` write is atomic among concurrent writers of the same object;
- `O_TRUNC` completes before open returns;
- flush reports delayed errors but does not guarantee persistence;
- `fsync(false)` calls fsync;
- `fsync(true)` calls fdatasync;
- release does not guarantee persistence.

## 4. Directory Operations

Supported:

```text
mkdir
opendir
readdir
releasedir
rename
rmdir
getattr
chmod
chown
utimens
```

rmdir removes only an empty ordinary Directory.

The following return `EOPNOTSUPP`:

```text
link
symlink
readlink
mknod
mkfifo
device node creation
socket node creation
```

## 5. Rename

The first version supports:

```text
flags = 0
RENAME_NOREPLACE
```

`RENAME_EXCHANGE` and `RENAME_WHITEOUT` are unsupported.

`RENAME_NOREPLACE` with an existing target returns `EEXIST`.

Unsupported rename flags return `EINVAL`.

Within one Library:

- ProjectionFile may move to a missing target;
- OverlayFile may move to a missing target;
- Directory may move to a missing target;
- OverlayFile may atomically replace OverlayFile with flags=0.

Rejected:

- OverlayFile replacing ProjectionFile: `EEXIST`;
- ProjectionFile replacing an existing file: `EEXIST`;
- file replacing directory: `EISDIR`;
- directory replacing file: `ENOTDIR`;
- directory replacing an existing directory: `EEXIST`;
- directory moving into its own subtree: `EINVAL`.

FUSE rename and `projection mv` across Libraries return `EXDEV`.

## 6. Object Store

The object-store root is configured by `storage.overlay_store_path` and is the actual on-disk location of OverlayFile contents. Default layout:

```text
/var/lib/scrapefs/objects/
├── staging/
└── live/
```

Only the daemon accesses the object store. Directories and backing objects use the daemon's effective UID/GID:

```text
directory mode: 0700
object mode:    0600
```

FUSE chmod/chown changes only virtual metadata in SQLite and does not change the backing object's owner or mode.

SQLite stores:

```text
objects
├── id
├── state
├── created_at_ns
├── live_at_ns
└── updated_at_ns
```

States:

```text
STAGING
LIVE
```

### Creation

1. Create an STAGING object record in SQLite.
2. Create the file in staging with `O_CREAT | O_EXCL`.
3. Write optional initial content and fsync the object.
4. Rename it to live.
5. Fsync both staging and live directories.
6. In one SQLite transaction, change the object to LIVE, create the OverlayFile node, write the log, and increment namespace version.
7. Return success or an open handle after commit.

A database failure does not return success. Recovery or GC removes unreferenced objects.

At startup, recovery compares every database record with the descriptor-anchored
staging and live directories:

- a STAGING record with no file is removed;
- a STAGING record whose file already reached live is completed as LIVE;
- a STAGING record with a staging file remains available to GC;
- a referenced LIVE record with no live file is reported as an integrity issue,
  while its node and database record remain unchanged;
- conflicting or untracked files are reported and left untouched for explicit GC
  or administrator investigation.

Recovery never creates replacement contents for a missing referenced object.

### Unlink

1. Delete the node, log the operation, and increment namespace version in one SQLite transaction.
2. Commit.
3. Delete the live object.

### Atomic Replacement

1. Remove the old target node in a SQLite transaction.
2. Move the source node to the target name.
3. Log the operation and increment namespace version.
4. Commit.
5. Delete the old target object.

An already opened old object fd remains valid.

## 7. Garbage Collection

GC may remove an object only when it:

- is LIVE;
- has no node reference;
- has no active handle;
- is older than `orphan_grace_period`.

Recommended:

```text
orphan_grace_period = 24h
```

Object creation and GC share a process-level coordinator. GC rechecks object state, node references, and active handles immediately before deletion.

Every object handle returned by create or open is tracked until its first close.
Immediate unlink/replacement cleanup retains an object while any such handle is
active or while it remains inside the grace period. A later GC run may reclaim
it after the handle closes and the grace period expires.

`scrapefs gc` is administrator-only. It returns
`scrapefs.gc-result/v1` JSON with deterministic examined, deleted, and retained
counts plus object-ID-ordered errors. Eligible abandoned STAGING records,
unreferenced LIVE records, and filesystem entries without database records are
removed. Referenced, active, recent, conflicting, and missing referenced objects
are retained.

## 8. Missing Object

When an OverlayFile node references a missing live object:

- open/getattr returns `EIO`;
- doctor reports a missing object;
- ScrapeFS does not create an empty replacement.

Unlink and Overlay-over-Overlay replacement remove the namespace entry before deleting the old live object. Existing open descriptors remain valid after that object is unlinked. Cleanup failures leave an unreferenced object for the recovery and garbage-collection workflow; they do not restore a stale namespace entry.

## 9. Cache

Recommended:

```text
kernel_cache      = off
writeback_cache   = off
auto_cache        = on

entry_timeout     = 1-3s
attr_timeout      = 1-3s
negative_timeout  = 0-1s
```

After a namespace commit, invalidate old/new parents, old/new names, and related inode attributes.

After metadata changes, invalidate related inode attributes.

Invalidation failure produces a warning; short TTLs provide eventual consistency.
