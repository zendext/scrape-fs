# Operations and CLI

**English** | [简体中文](../zh-CN/operations.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

## 1. Control Authorization

The first version uses three identity boundaries:

1. **Administrator**: root or a configured administrator UID; manages Libraries and the daemon.
2. **Control user**: a member of `scrapefs-control`; selects source paths, creates Projections, runs Scrapes, applies/undoes them, and uses status/doctor.
3. **Agent worker**: the configured Agent UID with a valid session token; accesses only Agent APIs.

Recommended socket:

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

The daemon obtains caller identity through Unix peer credentials.

Members of `scrapefs-control` are trusted source-path selectors. The daemon service account should have read access only to intended media directories so control users cannot use it to read unrelated sensitive files.

An Agent session token cannot call ordinary control operations.

## 2. Library Lifecycle

Ordinary removal of a non-empty Library returns `ENOTEMPTY`.

Recursive removal is rejected while the Library has:

- draft, waiting_input, or ready Scrapes;
- active Agent sessions;
- artifact uploads in progress.

The administrator cancels sessions and discards Scrapes before recursive removal.

Recursive removal uses one SQLite transaction to:

1. remove all ProjectionFile and OverlayFile nodes in the Library;
2. remove the Library node tree;
3. set `libraries.root_node_id` to null;
4. set the Library to RETIRED;
5. write the operation log;
6. increment namespace version.

Overlay objects are reclaimed after commit. Source files are never removed.

## 3. Single Instance

The daemon acquires:

```text
/run/scrapefs/scrapefsd.lock
```

using:

```text
flock(LOCK_EX | LOCK_NB)
```

Failure exits immediately without cleaning the mount or writing the database.

## 4. Startup

1. Acquire the single-instance lock.
2. Open SQLite and validate PRAGMAs.
3. Run migrations.
4. validate and recover the object store.
5. Rebuild the in-memory index.
6. After proving no live daemon exists, clean a stale ScrapeFS mount.
7. Mount `/mnt/scrapefs`.
8. Create the Unix socket.
9. Signal readiness.

Unknown FUSE filesystems are never unmounted.

## 5. Normal Shutdown

1. Stop accepting new control requests and namespace-write transactions.
2. Wait for in-flight transactions.
3. Unmount FUSE normally.
4. Close the socket and SQLite.
5. Release the instance lock.

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

scrapefs scrape
scrapefs scrape list
scrapefs scrape show
scrapefs scrape diff
scrapefs scrape answer
scrapefs scrape revise
scrapefs scrape validate
scrapefs scrape apply
scrapefs scrape undo
scrapefs scrape clone
scrapefs scrape discard
scrapefs scrape export

scrapefs gc
```

Daemon:

```text
scrapefsd
```

## 8. First-Version Configuration

Required configuration includes:

- SQLite database path;
- object-store path;
- FUSE mountpoint;
- socket path and permissions;
- administrator UID;
- Agent UID;
- FUSE cache TTLs;
- Agent upload quotas;
- object GC grace period.

The first version does not configure Source Roots, dynamic Profiles, or generic RBAC.
