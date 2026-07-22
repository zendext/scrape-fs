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
2. daemon 是唯一数据库写进程。
3. 所有写操作使用显式事务。
4. 命名空间写事务应使用 `BEGIN IMMEDIATE`。
5. migration 必须在 FUSE mount 前完成。
6. migration 或 PRAGMA 校验失败时 daemon 不得进入 ready。
7. SQLite commit 是数据库状态的持久化提交点。

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
- `actor_kind` 至少区分 control、agent、fuse 和 system；
- `details_json` 保存受影响节点的路径和类型快照；
- 日志不得通过强外键阻止节点或 Library retire；
- 第一版日志用于审计、doctor 和人工恢复分析；
- 第一版不提供事件重放。
