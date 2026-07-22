# Operations and CLI

**English** | [简体中文](../zh-CN/operations.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

## 1. Control Authorization

The first-version control plane uses two identity roles:

1. **Administrator**: root or a configured administrator UID; manages Libraries and the daemon.
2. **Control user**: a member of `scrapefs-control`; selects source paths, creates Projections, runs Scrapes, applies/undoes them, and uses status/doctor.

Recommended socket:

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

The daemon obtains caller identity through Unix peer credentials.

Members of `scrapefs-control` are trusted source-path selectors. The daemon service account should have read access only to intended media directories so control users cannot use it to read unrelated sensitive files.

An external Agent skill runs with the identity of the user that invokes it. It has no daemon-side session token or dedicated permission and can use only that user's existing host-filesystem, control CLI, and FUSE access. The invoking user owns process isolation, credentials, and network access for a remote or untrusted Agent; those concerns are outside the ScrapeFS authorization model.

## 2. Library Lifecycle

Ordinary removal of a non-empty Library returns `ENOTEMPTY`.

Recursive removal is rejected while the Library has:

- draft or ready Scrapes;
- namespace-write transactions in progress.

The administrator discards unapplied Scrapes and waits for in-flight writes before recursive removal.

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

scrapefs library add
scrapefs library list
scrapefs library rename
scrapefs library set-profile
scrapefs library remove

scrapefs projection add
scrapefs projection list
scrapefs projection mv
scrapefs projection rm
scrapefs projection rebind

scrapefs scrape submit
scrapefs scrape list
scrapefs scrape show
scrapefs scrape diff
scrapefs scrape revise
scrapefs scrape validate
scrapefs scrape apply
scrapefs scrape undo
scrapefs scrape discard
scrapefs scrape export

scrapefs gc
```

Daemon:

```text
scrapefsd
```

Every automation-facing command supports machine-readable JSON output. `scrape submit` and `scrape revise` read UTF-8 JSON through `--file <path>` and read stdin through `--file -`. A non-TTY invocation or explicit machine-readable mode does not prompt or apply automatically.

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

The currently implemented early configuration parser still accepts these legacy fields:

```toml
[agent]
max_text_file_size = "4MiB"
max_binary_file_size = "32MiB"
max_session_total_size = "256MiB"
max_upload_duration = "120s"
```

These fields do not configure the external Agent and are not part of the target v1 Agent-skill workflow. A follow-up implementation task will decide whether to remove them or repurpose them as general Overlay limits; current parsing and validation remain unchanged until then.

Fixed behavior:

- `kernel_cache = off`;
- `writeback_cache = off`;
- `auto_cache = on`;
- the socket group is `scrapefs-control`.

These are not configuration fields.

### 8.3 Value Representation

- UIDs are non-negative TOML integers in the `uint32` range; documentation examples use decimal;
- file modes are TOML octal integers such as `0o660` and contain only permission bits from `0o001` through `0o777`;
- durations use Go-duration-style strings such as `"2s"`, `"120s"`, and `"24h"`;
- sizes use binary-unit strings; the first version supports `KiB`, `MiB`, and `GiB`;
- paths are strings.

Parsing rejects unknown fields, out-of-range UIDs, invalid modes, and non-positive size quotas. For the legacy `[agent]` fields, a per-file quota does not exceed `max_session_total_size` and `max_upload_duration` is positive. FUSE TTLs are non-negative; `orphan_grace_period` is positive. `negative_timeout` may be `0s`.

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
