# 系统架构

[English](../en/architecture.md) | **简体中文**

本文定义 ScrapeFS 第一版的进程边界、持久化模型、Library 和统一命名空间。

## 1. 规范性术语

- **必须**：实现必须满足。
- **禁止**：实现不得支持。
- **应当**：没有明确理由时应当满足。
- **可以**：可选能力，不影响第一版验收。

## 2. 进程模型

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

`scrapefsd` 是唯一权威进程。

只有 daemon 可以：

- 打开和写入 SQLite；
- 修改虚拟命名空间；
- 访问 object store；
- 挂载和卸载 FUSE；
- 创建、删除或修改 ProjectionFile 和 OverlayFile；
- 应用和撤销 Scrape；
- 执行垃圾回收；
- 管理 Agent session。

CLI 和 Agent 禁止直接访问 SQLite 或 object store。

控制协议使用配置的 Unix socket。默认值：

```text
/run/scrapefs/scrapefs.sock
```

第一版可以采用 HTTP over Unix socket、JSON-RPC 或其他简单本地协议。

## 3. SQLite

必须至少启用：

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;
```

要求：

1. 第一版固定使用 WAL。
2. 打开 SQLite 前，daemon 必须在宿主文件系统的 `/run/scrapefs/scrapefsd.lock` 上获取 non-blocking exclusive `flock(2)`，并在 daemon 生命周期内持续持有。
3. daemon 必须持续持有已校验的数据库父目录文件描述符，并相对该描述符打开数据库和 sidecar，避免后续祖先路径替换将 SQLite 重定向到其他位置。
4. daemon 必须在应用持久化 PRAGMA 前检查 `PRAGMA user_version`。拒绝更新版本的 schema 时不得改变数据库 journal mode。
5. 所有写操作使用显式事务。
6. 命名空间写事务使用 `BEGIN IMMEDIATE`。
7. migration 必须在 FUSE mount 前完成。
8. migration 或 PRAGMA 校验失败时 daemon 不得进入 ready。
9. SQLite commit 是数据库状态的持久化提交点。

SQLite 自身的锁与事务语义负责保护数据库一致性。额外的 daemon 锁使一个进程独占完整 ScrapeFS 运行时，包括数据库、内存索引、mount、object store 和控制 socket，并在打开 SQLite 前拒绝意外重复启动。固定锁路径位于 ScrapeFS FUSE mount 之外的宿主运行时文件系统。该进程锁与通过 FUSE 暴露的文件锁操作无关。

持久化内部 ID 使用 SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`，Go 中表示为 `int64`，因此已删除节点的 ID 不会复用。持久化时间使用 SQLite `INTEGER` 保存 UTC Unix 纳秒。

基础命名空间 migration 创建只有 ID 的 `objects` 和 `scrapes` 表作为外键目标。后续 Object 和 Scrape migration 必须在对应行为开始创建记录前扩展这些表。

第一版固定使用一个 SQLite 连接，并且只通过受控入口访问。Repository 操作进入串行队列等待连接，调用方可以通过 context 取消等待。`Update` 回调使用对应 `Transaction` 的读取方法，不调用 Repository 方法。

## 4. Mount 和 Library

第一版只创建一个配置的 FUSE 挂载点。默认值：

```text
/mnt/scrapefs
```

Library 是挂载点下的一级目录：

```text
/mnt/scrapefs/
├── jellyfin/
├── plex/
└── anime/
```

禁止为每个 Library 创建独立 mount。

Library 数据：

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

状态：

```text
ACTIVE
RETIRED
```

约束：

- ACTIVE Library 的 `root_node_id` 必须非空。
- RETIRED Library 的 `root_node_id` 必须为空。
- `root_node_id` 外键使用 `ON DELETE SET NULL`。
- ACTIVE Library 名称唯一。
- `projection_file_mode` 禁止包含写权限位。

推荐默认值：

```text
root_mode            = 0775
directory_mode       = 0775
projection_file_mode = 0444
overlay_file_mode    = 0664
```

LibraryRoot 只能通过控制面管理。普通 FUSE 用户对 GlobalRoot 执行 mkdir、create，或者对 LibraryRoot 执行 rename、unlink、rmdir、chmod、chown 时，必须返回 `EPERM`。

切换 Library Profile 时：

- 已有 Projection 路径保持不变；
- 未应用 Scrape 继续使用创建时的 Profile；
- 新 Scrape 使用新的 Profile；
- 不自动迁移已有节点；
- 写 operation log；
- 不增加 namespace version。

## 5. 统一命名空间

节点类型：

```text
GLOBAL_ROOT
LIBRARY_ROOT
DIRECTORY
PROJECTION_FILE
OVERLAY_FILE
```

GlobalRoot 和 LibraryRoot 是受保护的特殊目录。

初始 migration 只创建一次 GlobalRoot，其节点 ID 固定为 `1`，名称为空，mode 为 `0755`，UID/GID 使用首次初始化时 daemon 的有效 UID/GID。该 bootstrap 节点不属于普通命名空间操作。

所有节点 ID 在整个实例中全局唯一。

名称唯一约束：

```sql
UNIQUE(parent_id, name)
```

核心表：

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

字段规则：

- ProjectionFile 使用 source 字段。
- OverlayFile 使用 `object_id`。
- Directory 不使用 source/object 字段。
- `managed_directory=true` 表示由 Scrape 或 `projection add` 自动创建。
- 直接创建的 ProjectionFile 的 `origin_scrape_id` 为空。

节点 ID 必须稳定映射为 FUSE inode：

- rename 不改变 inode；
- daemon 重启后 inode 稳定；
- 同一源文件的多个 Projection 拥有不同 inode；
- 不使用源文件 inode 作为虚拟 inode。

## 6. Namespace Version

SQLite 保存单调递增的 `namespace_version`。

初始 namespace version 为 `0`。migration 创建 GlobalRoot 时不增加版本，也不写 operation log。

未发生结构修改的命名空间事务不增加版本，也不写结构操作日志。

任何成功改变节点结构的事务只增加一次版本，包括：

- create、mkdir、rename、unlink、rmdir；
- projection add/mv/rm；
- scrape apply/undo；
- Library create/rename/recursive remove；
- Agent 创建 OverlayFile。

以下操作不增加版本：

- write、truncate、flush、fsync；
- chmod、chown、utimens；
- Projection rebind；
- Library Profile 切换；
- Scrape revision 变化。

ready Scrape 保存 `validated_namespace_version`。apply 时版本不同必须重新检查冲突，但不得仅因版本不同直接失败。

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

要求：

- 命名空间操作的日志与对应修改处于同一 SQLite 事务；
- operation log 记录插入后不可修改；
- `actor_kind` 至少区分 control、agent、fuse 和 system；
- `details_json` 使用非空且长度相同的 `paths` 与 `node_kinds` 数组，按相同下标保存路径和节点类型快照；
- 日志不得通过强外键阻止节点或 Library retire；
- 第一版日志用于审计、doctor 和人工恢复分析；
- 第一版不提供事件重放。
