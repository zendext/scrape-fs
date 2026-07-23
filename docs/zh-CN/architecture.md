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
外部 Agent Skill / 脚本 / 控制用户
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

`scrapefsd` 是唯一权威进程。

只有 daemon 可以：

- 打开和写入 SQLite；
- 修改虚拟命名空间；
- 访问 object store；
- 挂载和卸载 FUSE；
- 创建、删除或修改 ProjectionFile 和 OverlayFile；
- 应用和撤销 Projection Batch；
- 执行垃圾回收。

CLI 和外部 Agent 禁止直接访问 SQLite 或 object store。外部 Agent 使用调用它的用户身份读取宿主源文件、调用自己的媒体和 metadata 工具，并通过 CLI validate 和 apply 显式 Projection Batch。它不是 daemon 插件，也不获得额外文件系统权限。

NFO、海报等 artifact 在 Batch applied 后通过普通 FUSE 文件操作创建。ScrapeFS 不区分 OverlayFile 由人工、脚本还是 Agent 创建。

控制协议使用配置的 Unix socket。默认值：

```text
/run/scrapefs/scrapefs.sock
```

已实现的 Projection Batch 控制协议在每个 Unix socket 连接中使用一条严格 JSON request 和一条 JSON response。daemon 通过 `SO_PEERCRED` 获取调用者的有效 UID/GID，通过 `SO_PEERGROUPS` 获取 supplementary groups，不信任 client 提交的身份。root 和配置的管理员 UID 同时满足管理员与控制用户策略；其他调用者执行控制用户命令时，其有效或 supplementary group 必须包含固定的 `scrapefs-control` GID。当普通 mode bit 尚未提供所需访问权限时，daemon 通过 named POSIX ACL 授予配置的非 root 管理员 socket 访问权限，并授予该管理员与 `scrapefs-control` 对 daemon-owned socket 父目录的 traverse 权限。CLI 可以使用 `--socket` 覆盖默认 endpoint。

socket 父目录及其每一级祖先必须在不跟随 symlink 的情况下打开。祖先必须由 root 或 daemon 所有；group/other 可写的祖先只有在设置 sticky bit 时才允许；最终父目录必须由 daemon 所有且不得允许 group/other 写入。bind、metadata 修改、ACL 安装、stale path 删除和 close-time unlink 都必须相对持续持有的父目录 descriptor 执行；fd-anchored pathname 操作要求 procfs 提供 `/proc/self/fd`。开始接受 request 前，配置路径必须解析到通过该 descriptor bind 的同一个 socket inode。

控制服务最多接受 32 个 active connection。request 上限为 16 MiB，必须在 5 秒内接收完成；response 必须在 30 秒内写完。可重试的 accept 资源错误不会导致 daemon 退出。

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

基础命名空间 migration 创建只有 ID 的 `objects` 和 `scrapes` 表作为外键目标。`scrapes` 是已发布的 legacy storage 名称；schema migration 3 在不修改已发布 migration 的前提下扩展它保存 applied Batch provenance，并新增 `projection_batch_entries` 保存有序的 source identity 和 target 历史。

schema 只使用声明式局部完整性约束：`STRICT` 表、主键、`NOT NULL`、行内 `CHECK`、外键和唯一索引。SQLite trigger 不实现命名空间业务规则、生命周期策略、namespace version 策略或 operation log 策略。

Repository 是唯一的命名空间修改边界，负责跨行和跨表校验、受保护节点行为、冲突分类、namespace version 变更和 operation log 创建。第一版固定使用一个 SQLite 连接，并且只通过支持 context 取消的串行队列访问。队列回调获得有作用域的 connection lease，而不是原始 `sql.Conn`；回调结束后 connection、row 和 transaction wrapper 全部失效，未结束事务会在队列继续前回滚。`Update` 回调使用对应 namespace `Transaction` 的读取方法，不调用 Repository 方法。

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
- 后续 direct Projection 和 Batch validate/apply 使用新的 Profile；
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
- `managed_directory=true` 表示由 Projection Batch 或 `projection add` 自动创建。
- 直接创建的 ProjectionFile 的 `origin_scrape_id` 为空。
- Batch 创建的 ProjectionFile 使用 legacy `origin_scrape_id` 引用 applied Batch provenance。
- LibraryRoot 的父节点必须是 GlobalRoot。
- Directory、ProjectionFile 和 OverlayFile 的父节点必须是 LibraryRoot 或 Directory。

Repository 在创建节点前校验父节点类型。持久命名空间核心不提供通用 rename、move、kind 变更、delete 或任意节点 update 方法。后续操作必须提供各自的精确修改方法，并在同一事务中校验受保护根节点、子节点关系和祖先环。

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
- projection batch apply/undo；
- Library create/rename/recursive remove；
- 通过 FUSE 创建 OverlayFile。

以下操作不增加版本：

- write、truncate、flush、fsync；
- chmod、chown、utimens；
- Projection rebind；
- Library Profile 切换；
- 无状态 projection batch validate。

Batch validate 返回 `namespace_version` 供审阅。apply 始终重新检查冲突；版本不同不得单独导致失败。

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
- daemon 的 Repository API 只允许追加 operation log，不提供 update 或 delete 操作；
- `scrape_id` 是已发布的 legacy storage 名称，用于关联 applied Projection Batch；删除被引用的 provenance 时可以通过 `ON DELETE SET NULL` 清空；
- `actor_kind` 区分 control、fuse 和 system；已发布 schema 中的 `agent` 值保留为 legacy 值，外部 Agent 通过 CLI 或 FUSE 操作时分别记录为 control 或 fuse；
- `details_json` 使用非空且长度相同的 `paths` 与 `node_kinds` 数组，按相同下标保存路径和节点类型快照；Repository 在插入前校验每个路径和节点类型；
- 日志不得通过强外键阻止节点或 Library retire；
- 第一版日志用于审计、doctor 和人工恢复分析；
- 第一版不提供事件重放。
