# Operations and CLI

**English** | [简体中文](../zh-CN/operations.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

## 1. Control Authorization

The first-version control plane uses two identity roles:

1. **Administrator**: root or a configured administrator UID; manages Libraries and the daemon.
2. **Control user**: a member of `scrapefs-control`; lists Libraries, selects source paths, creates Projections, validates/applies/undoes Projection batches, and uses status/doctor.

Recommended socket:

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

The daemon resolves the fixed `scrapefs-control` group when the control server starts. A missing group or invalid numeric GID prevents the socket from being created.

The daemon obtains the caller's effective UID/GID through `SO_PEERCRED` and supplementary groups through `SO_PEERGROUPS`. Client-supplied identity is ignored. Root and `control.administrator_uid` may use administrator and control-user commands. Other callers must have the `scrapefs-control` GID as an effective or supplementary group for Library listing, Projection, Projection Batch, status, and doctor commands. Library creation and other Library mutations require an administrator. Unauthorized requests return the stable `PermissionDenied` error before the business service is invoked.

The socket is owned by the daemon effective UID and the `scrapefs-control` GID. `control.socket_mode` must grant read and write permission to both owner and group; the default remains `0660`. When ordinary mode bits do not already provide access, a configured non-root administrator that is not the daemon UID receives a named user ACL with read/write permission on the socket, and the daemon-owned parent receives named traverse ACL entries for that administrator and `scrapefs-control`. A parent initially using `0700` therefore does not block authorized callers. Existing unrelated parent ACL entries and their effective permissions are retained. Because a matching named-user ACL takes precedence over every group entry, startup fails when any unrelated named-user entry lacks the permission required by the control role; the daemon does not broaden or remove that entry. The configured administrator's own entry may be repaired to its documented access. A filesystem that cannot persist a required POSIX access ACL fails startup rather than silently making an authorized role unreachable.

The parent and every ancestor are opened without following symbolic links. Each ancestor must be root- or daemon-owned; non-sticky group/other-writable ancestors are rejected. Every intermediate ancestor must already provide effective traverse access to the configured administrator and `scrapefs-control`, accounting for POSIX ACL masks; startup rejects a restrictive ancestor instead of leaving an authorized caller unable to connect. The final parent must be daemon-owned and not group/other writable, and the daemon adds any missing role ACL there. A stale path is removed only when it is a socket with the expected UID/GID. Bind and automatic unlink use the retained parent descriptor through `/proc/self/fd`, so procfs must be mounted at `/proc`. Startup verifies the final ACLs and configured socket inode before accepting requests.

Members of `scrapefs-control` are trusted source-path selectors. The daemon service account should have read access only to intended media directories so control users cannot use it to read unrelated sensitive files. To set the socket group without root privileges, the daemon service account must itself have permission to use the `scrapefs-control` GID.

An external Agent skill runs with the identity of the user that invokes it. It has no daemon-side session token or dedicated permission and can use only that user's existing host-filesystem, control CLI, and FUSE access. The invoking user owns process isolation, credentials, and network access for a remote or untrusted Agent; those concerns are outside the ScrapeFS authorization model.

## 2. Library Lifecycle

Ordinary removal of a non-empty Library returns `ENOTEMPTY`.

Recursive removal is rejected while the Library has:

- namespace-write transactions in progress.

The administrator waits for in-flight batch apply, undo, or other namespace writes before recursive removal.

Recursive removal uses one SQLite transaction to:

1. remove all ProjectionFile and OverlayFile nodes in the Library;
2. remove the Library node tree;
3. set `libraries.root_node_id` to null;
4. set the Library to RETIRED;
5. write the operation log;
6. increment namespace version.

Overlay objects are reclaimed after commit. Source files are never removed.

## 3. Daemon Process Exclusivity

The daemon opens the following lock file on the host runtime filesystem:

```text
/run/scrapefs/scrapefsd.lock
```

and acquires:

```text
flock(LOCK_EX | LOCK_NB)
```

The lock file and its parent path reject symlinks and invalid ownership or file types. The daemon holds the lock file descriptor for its lifetime. Lock acquisition failure exits before opening or modifying SQLite, mounting FUSE, or creating the control socket.

The runtime directory uses daemon ownership and mode `0755`. The lock file uses daemon ownership and mode `0600`.

This lock establishes exclusive ownership of the full ScrapeFS runtime on the host. It does not replace SQLite's own locking and is not a FUSE file lock.

## 4. Startup

1. Prepare and validate the host runtime lock-file path.
2. Acquire the host-filesystem daemon lock.
3. Prepare and validate the database parent directory and path.
4. Hold the validated parent-directory file descriptor and open SQLite relative to it.
5. Read and validate `PRAGMA user_version` without changing persistent database settings.
6. Enable and verify the required PRAGMAs.
7. Run migrations.
8. Validate and recover the object store.
9. Rebuild the in-memory index.
10. Clean a stale ScrapeFS mount after the daemon lock proves no competing instance owns the runtime.
11. Mount the configured FUSE mountpoint, defaulting to `/mnt/scrapefs`.
12. Create the Unix socket.
13. Signal readiness.

Unknown FUSE filesystems are never unmounted.

The daemon mounts through the retained mountpoint directory descriptor rather
than resolving the configured path again. This descriptor-anchored direct
`mount(2)` requires root or `CAP_SYS_ADMIN`; startup does not fall back to an
unanchored `fusermount` path.

## 5. Normal Shutdown

1. Stop accepting new control requests and namespace-write transactions.
2. Wait for in-flight transactions.
3. Unmount FUSE normally.
4. Close the socket and SQLite.
5. Release the daemon lock.

Normal shutdown does not use lazy unmount.

## 6. Fail-Closed Mountpoint

When FUSE is not mounted, media players must not see a readable empty directory.

Recommended:

```text
owner: root
group: scrapefs
mode: 0710
```

Player users are not members of `scrapefs`. After mount, `allow_other` exposes the Library.

## 7. CLI

```text
scrapefs status
scrapefs doctor

scrapefs library add <name> --profile <name>@<version>
scrapefs library list
scrapefs library rename <name> <new-name>
scrapefs library set-profile <name> --profile <name>@<version>
scrapefs library remove

scrapefs projection add <source> <library:/target>
scrapefs projection list [--library <name>]
scrapefs projection mv <library:/target> <library:/new-target>
scrapefs projection rm <library:/target>
scrapefs projection rebind <library:/target> --source <path> [--confirm <digest>]

scrapefs projection batch validate
scrapefs projection batch apply
scrapefs projection batch list
scrapefs projection batch show
scrapefs projection batch undo

scrapefs gc
```

Daemon:

```text
scrapefsd
```

Every implemented Library, Projection, and Projection Batch command writes machine-readable JSON. Library mutations return `scrapefs.library-result/v1`; `library list` returns `scrapefs.library-list/v1`.

`scrapefs gc` requires administrator authorization and returns
`scrapefs.gc-result/v1`. Its arrays and per-object errors are deterministic, and
its counts distinguish LIVE, STAGING, untracked, referenced, active, grace-period,
and missing-referenced outcomes. The result also includes the daemon's startup
recovery report, including missing referenced object IDs and node IDs. An
object-level cleanup failure is reported in the result without making another
candidate eligible for unsafe deletion.

`library add` creates an ACTIVE Library with the invoking administrator's peer UID/GID and the fixed v1 modes `0775` for the LibraryRoot and directories, `0444` for Projections, and `0664` for OverlayFiles. `library rename` atomically renames the ACTIVE Library record and its protected LibraryRoot, preserves both stable IDs, writes one operation-log entry, and increments namespace version once. A duplicate or invalid new name leaves the Library, LibraryRoot, namespace version, and operation log unchanged.

`library set-profile` changes the Profile used by later direct Projection and Projection Batch validation/apply. It does not move or rename existing nodes, writes one operation-log entry, and does not increment namespace version. Only the built-in `jellyfin@1` and `plex@1` Profiles are accepted. `library add`, `library rename`, and `library set-profile` require administrator authorization. `library list` returns ACTIVE Libraries in deterministic name order and is available to administrators and control users.

`projection add`, `projection mv`, and `projection rm` return `scrapefs.projection-result/v1`; remove returns the deleted Projection snapshot. Source selection, target validation, and namespace mutation run only after control authorization succeeds. `projection list` returns direct and batch Projections using `scrapefs.projection-list/v1`, ordered deterministically by Library, relative target, and node ID. `--library <name>` limits the result to that ACTIVE Library. Listing uses persisted cached metadata and does not open source files.

`projection mv` accepts only a missing target in the same ACTIVE Library, preserves the node ID and source metadata, creates missing managed target directories atomically, writes one operation-log entry, and increments namespace version once. Cross-Library moves return `CrossLibrary`; an existing target returns `TargetConflict`. `projection rm` deletes only the Projection node, prunes empty managed directories toward LibraryRoot, retains user-created or non-empty directories, writes one operation-log entry, and increments namespace version once. Neither command opens, renames, or removes the source file.

`projection rebind` without `--confirm` securely opens the proposed source and returns `scrapefs.projection-rebind-result/v1` with status `confirmation_required`, the stable Projection snapshot, complete previous and proposed cached source metadata, and a confirmation digest bound to the node ID, target, source paths, and strict identities. Repeating the command with `--confirm <digest>` reopens and revalidates the source and applies only the exact preview, returning status `applied`. A changed Projection or source returns `ConfirmationMismatch` with no mutation. Successful rebind preserves the node ID and virtual target, updates source metadata and virtual ctime, writes one audit entry, and does not increment namespace version.

These three lifecycle commands apply only to direct Projections whose `origin_scrape_id` is null. A Projection created by `projection batch apply` returns `BatchOwnedProjection`; its path, source metadata, batch entry, managed directories, namespace version, and operation log remain unchanged. Batch-owned Projections continue to be managed through Batch show and undo.

All Projection commands require administrator or control-user authorization. Missing targets return `ProjectionNotFound`; unsafe, missing, denied, or non-regular rebind sources use the same stable source error codes as `projection add`.

`projection batch validate` and `projection batch apply` read UTF-8 JSON through `--file <path>` and read stdin through `--file -`. A non-TTY invocation or explicit machine-readable mode does not prompt or apply automatically. The CLI connects to `/run/scrapefs/scrapefs.sock` by default and accepts global `--socket <path>` for a configured override.

## 8. First-Version Configuration

### 8.1 Configuration File

The first version uses TOML.

The fixed default configuration file is:

```text
/etc/scrapefs/config.toml
```

The daemon can override it with:

```bash
scrapefsd --config /path/to/config.toml
```

If the selected file is missing, unreadable, syntactically invalid, contains an unknown field, or fails validation, the daemon exits before creating a database, directory, socket, lock, or mount.

The first version does not read configuration from environment variables, merge multiple configuration sources, or support live reload.

### 8.2 Schema and Defaults

```toml
[storage]
database_path = "/var/lib/scrapefs/scrapefs.db"
overlay_store_path = "/var/lib/scrapefs/objects"

[fuse]
mountpoint = "/mnt/scrapefs"
entry_timeout = "2s"
attr_timeout = "2s"
negative_timeout = "0s"

[control]
socket_path = "/run/scrapefs/scrapefs.sock"
socket_mode = 0o660
administrator_uid = 0

[gc]
orphan_grace_period = "24h"
```

`storage.overlay_store_path` is the actual on-disk root for OverlayFile contents and is the object-store root. There is no second Overlay storage path.

The external Agent is not configured through the daemon. Its model, metadata providers, credentials, media tools, and skill configuration are managed externally.

The legacy `[agent]` table is not part of the v1 daemon configuration and is rejected as unknown configuration. When upgrading an older configuration file, remove the entire `[agent]` table. Its former upload and session limits are not interpreted as Overlay quotas; configure any external Agent limits outside `scrapefsd`.

Fixed behavior:

- `kernel_cache = off`;
- `writeback_cache = off`;
- `auto_cache = on`;
- the socket group is `scrapefs-control`.

These are not configuration fields.

### 8.3 Value Representation

- UIDs are non-negative TOML integers from `0` through `4294967294`; `4294967295` is reserved by Linux ACL encoding, and documentation examples use decimal;
- file modes are TOML octal integers such as `0o660` and contain only permission bits from `0o001` through `0o777`; `control.socket_mode` must include owner and group read/write bits (`0o660`);
- durations use Go-duration-style strings such as `"2s"` and `"24h"`;
- paths are strings.

Parsing rejects unknown fields, out-of-range UIDs, and invalid modes. FUSE TTLs are non-negative; `orphan_grace_period` is positive. `negative_timeout` may be `0s`.

### 8.4 Path Constraints

The following four paths are lexically normalized absolute paths:

- SQLite database path;
- Overlay object-store path;
- FUSE mountpoint;
- Unix socket path.

The four paths are pairwise disjoint. No path may equal, contain, or be contained by another path.

Configuration loading and validation do not create, open, or modify these paths. The startup component that uses a path is responsible for existence, ownership, permissions, and symlink-safe opening.

Existing SQLite `-journal`, `-wal`, and `-shm` sidecars are part of the database state and follow the same ownership, private-mode, regular-file, and no-symlink validation as the main database file. The daemon keeps the validated database parent directory open and accesses the main file and sidecars relative to that descriptor for the SQLite lifetime.

The first version does not configure Source Roots, dynamic Profiles, or generic RBAC.
