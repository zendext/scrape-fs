# Architecture

**English** | [简体中文](../zh-CN/architecture.md)

> This is an English translation. If the English and Chinese documents conflict, the Chinese documentation is authoritative.

This document defines the process boundary, persistence model, Library model, and unified namespace for the first ScrapeFS release.

## 1. Normative Terms

- **MUST**: required for conformance.
- **MUST NOT**: prohibited.
- **SHOULD**: expected unless there is a documented reason not to comply.
- **MAY**: optional and not required for first-version acceptance.

## 2. Process Model

```text
scrapefs CLI
      │ Unix socket
      ▼
scrapefsd
├── Control API
├── Scrape manager
├── Agent session manager
├── Profile renderer
├── Namespace service
├── Projection service
├── Object store
├── SQLite
└── FUSE implementation
```

`scrapefsd` is the sole authoritative process.

Only the daemon may:

- open and write SQLite;
- modify the virtual namespace;
- access the object store;
- mount and unmount FUSE;
- create, remove, or modify ProjectionFiles and OverlayFiles;
- apply and undo Scrapes;
- run garbage collection;
- manage Agent sessions.

The CLI and Agent MUST NOT access SQLite or the object store directly.

The control socket is:

```text
/run/scrapefs/scrapefs.sock
```

The first version may use HTTP over Unix socket, JSON-RPC, or another small local protocol.

## 3. SQLite

The daemon MUST enable at least:

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;
```

Requirements:

1. The first version uses WAL without silent journal-mode fallback.
2. The daemon is the only database writer.
3. All writes use explicit transactions.
4. Namespace writes should use `BEGIN IMMEDIATE`.
5. Migrations finish before the FUSE mount is created.
6. Migration or PRAGMA validation failure prevents readiness.
7. SQLite commit is the persistence point for database state.

## 4. Mount and Library

The first version creates one FUSE mount:

```text
/mnt/scrapefs
```

Libraries are first-level directories:

```text
/mnt/scrapefs/
├── jellyfin/
├── plex/
└── anime/
```

Separate FUSE mounts per Library are prohibited.

Library data:

```text
libraries
├── id
├── name
├── status
├── root_node_id
├── profile_name
├── profile_version
├── uid
├── gid
├── root_mode
├── directory_mode
├── projection_file_mode
├── overlay_file_mode
├── created_at
├── updated_at
└── retired_at
```

States:

```text
ACTIVE
RETIRED
```

Constraints:

- An ACTIVE Library has a non-null `root_node_id`.
- A RETIRED Library has a null `root_node_id`.
- The `root_node_id` foreign key uses `ON DELETE SET NULL`.
- ACTIVE Library names are unique.
- `projection_file_mode` contains no write bits.

Recommended defaults:

```text
root_mode            = 0775
directory_mode       = 0775
projection_file_mode = 0444
overlay_file_mode    = 0664
```

LibraryRoot is managed only through the control API. FUSE requests to create entries under GlobalRoot, or to rename, unlink, remove, chmod, or chown a LibraryRoot, return `EPERM`.

When a Library changes Profile:

- existing Projection paths remain unchanged;
- unapplied Scrapes keep their original Profile;
- new Scrapes use the new Profile;
- existing nodes are not migrated automatically;
- an operation-log entry is written;
- namespace version is unchanged.

## 5. Unified Namespace

Node kinds:

```text
GLOBAL_ROOT
LIBRARY_ROOT
DIRECTORY
PROJECTION_FILE
OVERLAY_FILE
```

GlobalRoot and LibraryRoot are protected directories.

Node IDs are globally unique within a ScrapeFS instance.

Name uniqueness:

```sql
UNIQUE(parent_id, name)
```

Core table:

```text
nodes
├── id
├── parent_id
├── name
├── kind
├── source_path
├── source_device_id
├── source_inode
├── cached_size
├── cached_atime_ns
├── cached_mtime_ns
├── cached_source_ctime_ns
├── verified_at_ns
├── object_id
├── origin_scrape_id
├── creator_kind
├── creator_id
├── managed_directory
├── mode
├── uid
├── gid
├── atime_ns
├── mtime_ns
├── virtual_ctime_ns
├── created_at
└── updated_at
```

Field rules:

- ProjectionFile uses the source fields.
- OverlayFile uses `object_id`.
- Directory uses neither source nor object fields.
- `managed_directory=true` marks a directory created automatically by a Scrape or `projection add`.
- A directly created ProjectionFile has a null `origin_scrape_id`.

Node IDs map stably to FUSE inodes:

- rename does not change inode;
- inode remains stable after daemon restart;
- separate Projections of one source file have separate inodes;
- the source inode is not used as the virtual inode.

## 6. Namespace Version

SQLite stores a monotonically increasing `namespace_version`.

Each successful transaction that changes namespace structure increments it once, including:

- create, mkdir, rename, unlink, and rmdir;
- projection add, mv, and rm;
- scrape apply and undo;
- Library create, rename, and recursive removal;
- Agent-created OverlayFiles.

The following do not increment it:

- write, truncate, flush, and fsync;
- chmod, chown, and utimens;
- Projection rebind;
- Library Profile changes;
- Scrape revision changes.

A ready Scrape stores `validated_namespace_version`. Apply revalidates conflicts when the version changed, but does not fail solely because the version differs.

## 7. Operation Log

```text
operation_log
├── id
├── operation
├── actor_kind
├── actor_id
├── scrape_id
├── namespace_version
├── details_json
└── created_at
```

Requirements:

- namespace changes and their log entry share one SQLite transaction;
- `actor_kind` distinguishes at least control, agent, fuse, and system;
- `details_json` stores required path and node-kind snapshots;
- strong foreign keys MUST NOT prevent node or Library retirement;
- the first version uses the log for audit, doctor, and manual recovery analysis;
- event replay is not provided.
