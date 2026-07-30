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
external Agent skill / script / control user
                    │
                    ▼
               scrapefs CLI
                    │ Unix socket
                    ▼
scrapefsd
├── Control API
├── Projection batch service
├── Profile validator
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
- apply and undo Projection batches;
- run garbage collection.

The CLI and external Agent MUST NOT access SQLite or the object store directly. The external Agent reads host source files with the invoking user's identity, uses its own media and metadata tools, and validates and applies an explicit Projection batch through the CLI. It is not a daemon plugin and receives no additional filesystem permission.

Artifacts such as NFO files and artwork are created through ordinary FUSE operations after the batch is applied. ScrapeFS does not distinguish an OverlayFile created by a person, script, or Agent.

The control protocol uses the configured Unix socket. Default:

```text
/run/scrapefs/scrapefs.sock
```

The implemented Projection Batch control protocol uses one strict JSON request and one JSON response per Unix-socket connection. The daemon obtains the caller's effective UID/GID from `SO_PEERCRED` and supplementary groups from `SO_PEERGROUPS`; client-supplied identity is not trusted. Root and the configured administrator UID satisfy administrator and control-user policy. Other callers must have the fixed `scrapefs-control` GID as their effective or supplementary group for control-user commands. When ordinary mode bits do not already provide access, the daemon grants the configured non-root administrator socket access through a named POSIX ACL and grants the administrator plus `scrapefs-control` traverse access to the daemon-owned socket parent. The CLI can override the default endpoint with `--socket`.

The socket parent and every path ancestor are opened without following symbolic links. Ancestors are root- or daemon-owned, and a group/other-writable ancestor is accepted only when sticky. Each intermediate ancestor must already grant effective traversal to the configured administrator and `scrapefs-control`; ACL checks use named-user precedence, the owning-group and named-group entries, and the ACL mask rather than treating stat group bits as owning-group permissions. A restrictive unrelated named-user entry therefore fails closed because it can override control-group membership. The final parent is daemon-owned and not group/other writable, and missing role access is added there and on the socket. Bind, metadata changes, ACL installation, stale removal, and close-time unlink remain relative to the retained parent descriptor; fd-anchored pathname operations require procfs at `/proc/self/fd`. The server becomes ready only after role ACL installation, effective-access revalidation, and verification that the configured pathname resolves to the socket inode bound through the descriptor.

The control server accepts at most 32 active connections. A request is limited to 16 MiB and must be received within 5 seconds; a response must be written within 30 seconds. Retryable accept resource errors do not terminate the daemon.

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
2. Before opening SQLite, the daemon acquires a non-blocking exclusive host-filesystem `flock(2)` on `/run/scrapefs/scrapefsd.lock` and holds it for the daemon lifetime.
3. The daemon holds the validated database parent-directory file descriptor and opens the database and sidecars relative to it, so later ancestor-path replacement cannot redirect SQLite.
4. The daemon checks `PRAGMA user_version` before applying persistent PRAGMA changes. Rejecting a newer schema version must not change the database journal mode.
5. All writes use explicit transactions.
6. Namespace writes use `BEGIN IMMEDIATE`.
7. Migrations finish before the FUSE mount is created.
8. Migration or PRAGMA validation failure prevents readiness.
9. SQLite commit is the persistence point for database state.

SQLite's own locking and transaction semantics protect database consistency. The additional daemon lock gives one process exclusive ownership of the full ScrapeFS runtime, including the database, in-memory indexes, mount, object store, and control socket, and fails an accidental duplicate start before SQLite is opened. The fixed lock path is on the host runtime filesystem outside the ScrapeFS FUSE mount. This process lock is unrelated to file-lock operations exposed through FUSE.

Persistent internal identities use SQLite `INTEGER PRIMARY KEY AUTOINCREMENT` values and Go `int64`, so deleted node IDs are not reused. Persisted timestamps use UTC Unix nanoseconds in SQLite `INTEGER` columns.

The foundational namespace migration creates ID-only `objects` and `scrapes` tables as foreign-key targets. `scrapes` is a released legacy storage name. Schema migration 3 extends it for applied-batch provenance without rewriting the released migration and adds `projection_batch_entries` for ordered source identity and target history. Schema migration 4 extends the released `objects` placeholder with `STAGING`/`LIVE` lifecycle state and timestamps. Schema migration 5 permits an associated `projection.batch.undo` operation to record empty path and node-kind arrays when no Batch-owned Projection remains, while every other operation still requires at least one affected path.

The schema uses declarative local integrity only: `STRICT` tables, primary keys, `NOT NULL`, row-local `CHECK` constraints, foreign keys, and unique indexes. SQLite triggers do not implement namespace business rules, lifecycle policy, namespace-version policy, or operation-log policy.

The Repository is the sole namespace mutation boundary. It owns cross-row and cross-table validation, protected-node behavior, conflict classification, namespace-version changes, and operation-log creation. The first version pins one SQLite connection and exposes it only through a serialized, context-aware queue. A queue callback receives a scoped connection lease instead of the raw `sql.Conn`; connection, row, and transaction wrappers expire when the callback ends, and unfinished transactions are rolled back before the queue advances. An `Update` callback uses the read methods on its namespace `Transaction` and does not call Repository methods.

## 4. Mount and Library

The first version creates one configured FUSE mount. Default:

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

Renaming an ACTIVE Library updates the Library record and LibraryRoot name in one transaction, preserves their stable IDs, writes one operation-log entry, and increments namespace version once. Validation or uniqueness failures roll back both names and the audit record.

When a Library changes Profile:

- existing Projection paths remain unchanged;
- later direct Projection and batch validate/apply operations use the new Profile;
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

The initial migration creates GlobalRoot once with node ID `1`, an empty name, mode `0755`, and the daemon effective UID/GID. This bootstrap node is not an ordinary namespace operation.

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
- `managed_directory=true` marks a directory created automatically by a Projection batch or `projection add`.
- A directly created ProjectionFile has a null `origin_scrape_id`.
- A ProjectionFile created by a batch uses legacy `origin_scrape_id` to reference applied-batch provenance.
- LibraryRoot has GlobalRoot as its parent.
- Directory, ProjectionFile, and OverlayFile have LibraryRoot or Directory as their parent.

The Repository validates parent kinds before creation. It does not expose generic rename, move, kind-change, delete, or arbitrary node-update methods in the persistent namespace core. Later operations must provide their own exact mutation methods and validate protected roots, child relationships, and ancestor cycles within the same transaction.

Node IDs map stably to FUSE inodes:

- rename does not change inode;
- inode remains stable after daemon restart;
- separate Projections of one source file have separate inodes;
- the source inode is not used as the virtual inode.

## 6. Namespace Version

SQLite stores a monotonically increasing `namespace_version`.

The initial namespace version is `0`. Creating GlobalRoot during migration does not increment it and does not write an operation-log entry.

A namespace transaction that makes no structural change does not increment the version or write a structural operation-log entry.

Each successful transaction that changes namespace structure increments it once, including:

- create, mkdir, rename, unlink, and rmdir;
- projection add, mv, and rm;
- projection batch apply and undo;
- Library create, rename, and recursive removal;
- OverlayFiles created through FUSE.

The following do not increment it:

- write, truncate, flush, and fsync;
- chmod, chown, and utimens;
- Projection rebind;
- Library Profile changes;
- stateless projection batch validation.

Batch validation returns `namespace_version` for review. Apply always revalidates conflicts and does not fail solely because the version differs.

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
- the daemon Repository API is append-only and exposes no operation-log update or delete operation;
- `scrape_id` is a released legacy storage name used to associate an applied Projection batch; deleting referenced provenance may clear it through `ON DELETE SET NULL`;
- `actor_kind` distinguishes control, fuse, and system; the `agent` value in the released schema remains a legacy value, while an external Agent operation is recorded as control or fuse according to the interface it uses;
- `details_json` stores non-empty, equally sized `paths` and `node_kinds` arrays whose entries are paired snapshots; the Repository validates each path and node kind before insertion;
- strong foreign keys MUST NOT prevent node or Library retirement;
- the first version uses the log for audit, doctor, and manual recovery analysis;
- event replay is not provided.
