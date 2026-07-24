# 运行与 CLI

[English](../en/operations.md) | **简体中文**

## 1. 控制面授权

第一版控制面使用两个身份角色：

1. **管理员**：root 或配置的管理员 UID，可管理 Library 和 daemon。
2. **控制用户**：属于 `scrapefs-control` 组，可列出 Library、选择源路径、创建 Projection、validate/apply/undo Projection Batch、查看 status/doctor。

推荐 socket：

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

控制服务启动时，daemon 必须解析固定的 `scrapefs-control` group。group 不存在或 GID 不是有效数值时，不得创建 socket。

daemon 通过 `SO_PEERCRED` 获取调用者的有效 UID/GID，通过 `SO_PEERGROUPS` 获取 supplementary groups，并忽略 client 提交的身份。root 和 `control.administrator_uid` 可以执行管理员与控制用户命令；其他调用者执行 Library list、Projection、Projection Batch、status 和 doctor 命令时，其有效或 supplementary group 必须包含 `scrapefs-control` GID。创建 Library 及其他 Library mutation 需要管理员权限。未授权请求必须在调用业务服务前返回稳定的 `PermissionDenied` 错误。

socket owner 使用 daemon effective UID，group 使用 `scrapefs-control` GID。`control.socket_mode` 必须同时授予 owner 和 group 读写权限，默认值仍为 `0660`。当普通 mode bit 尚未提供所需访问权限时，配置的非 root 管理员如果不是 daemon UID，必须通过 named user ACL 获得 socket 读写权限；daemon-owned 父目录必须为该管理员和 `scrapefs-control` 添加 named traverse ACL。因此初始 mode 为 `0700` 的父目录不会阻止已授权调用者。已有且无关的父目录 ACL entry 及其有效权限必须保留。由于匹配的 named-user ACL 优先于所有 group entry，任何无关 named-user entry 缺少控制角色所需权限时，daemon 必须启动失败，不得扩大或删除该 entry；配置管理员自身的 entry 可以修复为文档规定的权限。文件系统无法持久化所需 POSIX access ACL 时，daemon 必须启动失败，不得静默导致已授权角色不可访问。

父目录及每一级祖先必须在不跟随 symlink 的情况下打开。每个祖先必须由 root 或 daemon 所有；未设置 sticky bit 的 group/other 可写祖先必须被拒绝。每个中间祖先都必须在考虑 POSIX ACL mask 后，已经向配置的管理员和 `scrapefs-control` 提供有效 traverse 权限；限制性祖先必须导致启动失败，不能让已授权调用者在 connect 前被阻断。最终父目录必须由 daemon 所有且不得允许 group/other 写入，daemon 在此补齐缺失的角色 ACL。只有类型为 socket 且 UID/GID 符合预期的 stale path 才能删除。bind 和自动 unlink 必须通过 `/proc/self/fd` 使用持续持有的父目录 descriptor，因此 procfs 必须挂载在 `/proc`。开始接受 request 前，daemon 必须完成最终 ACL 和配置 socket inode 校验。

`scrapefs-control` 成员是可信源路径选择者。daemon 服务账号应只拥有预期媒体目录的读取权限，避免控制用户借 daemon 读取其他敏感文件。daemon 非 root 运行时，服务账号自身必须有权使用 `scrapefs-control` GID，才能设置 socket group。

外部 Agent Skill 以调用它的用户身份运行。它没有 daemon-side session token 或专用权限，只能使用该用户已有的宿主文件系统、控制 CLI 和 FUSE 权限。远程或不可信 Agent 的进程隔离、凭据和网络访问由启动它的用户负责，不属于 ScrapeFS 权限模型。

## 2. Library 生命周期

普通删除非空 Library 返回 `ENOTEMPTY`。

递归删除前不得存在：

- 进行中的命名空间写事务。

管理员必须等待在途 Batch apply、undo 或其他命名空间写事务结束，再执行 recursive remove。

recursive remove 必须在单个 SQLite 事务中：

1. 删除 Library 中所有 ProjectionFile 和 OverlayFile 节点。
2. 删除 Library 节点树。
3. 将 `libraries.root_node_id` 置空。
4. 将 Library 标记为 RETIRED。
5. 写 operation log。
6. namespace version 增加 1。

事务提交后回收 Overlay object。不得删除任何源文件。

## 3. Daemon 进程排他

daemon 必须在宿主运行时文件系统中打开：

```text
/run/scrapefs/scrapefsd.lock
```

并获取：

```text
flock(LOCK_EX | LOCK_NB)
```

锁文件及其父路径必须拒绝 symlink、无效 owner 和无效文件类型。daemon 在自身生命周期内持续持有锁文件描述符。无法获得锁时必须在打开或修改 SQLite、挂载 FUSE 或创建控制 socket 前退出。

运行时目录使用 daemon owner 和 `0755`，锁文件使用 daemon owner 和 `0600`。

该锁使 daemon 独占宿主机上的完整 ScrapeFS 运行时。它不替代 SQLite 自身的锁，也不是 FUSE 文件锁。

## 4. 启动

1. 准备并校验宿主运行时锁文件路径。
2. 获取宿主文件系统 daemon 锁。
3. 准备并校验数据库父目录和路径。
4. 持续持有已校验的父目录文件描述符，并相对该描述符打开 SQLite。
5. 在不改变持久化数据库设置的情况下读取并校验 `PRAGMA user_version`。
6. 启用并验证必需 PRAGMA。
7. 执行 migration。
8. 校验和恢复 object store。
9. 重建内存索引。
10. daemon 锁证明没有竞争实例持有该运行时后，再清理 stale ScrapeFS mount。
11. 挂载配置的 FUSE mountpoint，默认为 `/mnt/scrapefs`。
12. 创建 Unix socket。
13. 发送 readiness。

如果设置了 systemd 兼容的 `NOTIFY_SOCKET`，daemon 只会在 index、FUSE mount 和控制
socket 全部可用后发送精确的 `READY=1` datagram。支持文件系统 socket path 和以 `@`
开头的 Linux abstract socket；未设置该环境变量时不发送通知。通知失败会使启动失败，
并且只清理已经获取的资源。

不得卸载未知 FUSE 文件系统。

daemon 必须通过持续持有的 mountpoint 目录 descriptor 挂载，而不是再次解析配置路径。
该 descriptor-anchored direct `mount(2)` 要求 root 或 `CAP_SYS_ADMIN`；启动不得回退到
未锚定的 `fusermount` 路径。

## 5. 正常停止

1. 停止接受新控制请求。
2. 关闭新的 namespace write admission，并等待已准入写事务完成。
3. 等待已接受的控制请求完成。
4. 正常卸载 FUSE。
5. 关闭控制 socket、object store 和 SQLite。
6. 释放 daemon 锁。

正常停止禁止使用 lazy unmount。

## 6. Fail-closed Mountpoint

未挂载时播放器不得看到可读的普通空目录。

推荐：

```text
owner: root
group: scrapefs
mode: 0710
```

播放器用户不属于 `scrapefs` 组。FUSE 挂载后通过 `allow_other` 提供访问。

## 7. CLI

```text
scrapefs status
scrapefs doctor

scrapefs library add <name> --profile <name>@<version>
scrapefs library list
scrapefs library rename <name> <new-name>
scrapefs library set-profile <name> --profile <name>@<version>
scrapefs library remove <name> [--recursive]

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

daemon：

```text
scrapefsd
```

所有已实现的 Library、Projection 和 Projection Batch 命令都输出机器可读 JSON。Library 修改命令返回 `scrapefs.library-result/v1`，`library list` 返回 `scrapefs.library-list/v1`。

`scrapefs status` 和 `scrapefs doctor` 需要管理员或控制用户授权，且只能通过 daemon 控制 socket 执行。CLI 不得直接打开 SQLite、object store 或 FUSE runtime metadata，也不提供 offline doctor。

`status` 返回稳定的 `scrapefs.status/v1` JSON。顶层 `state` 使用 `ready`、`degraded` 或 `unavailable`，并包含：

- daemon version 与当前 `ready`；
- database 当前和支持的 schema version；
- namespace version、node、Projection 和 OverlayFile 计数；
- 内存 index 的 Library、node 和 object 计数；
- 配置的 mount/socket path 及其当前状态；
- object store 启动恢复 issue 数；
- 来自同一 Repository inspection 快照的 ACTIVE Library 数量、稳定 ID、root node 和 Profile。

control socket 可以在 daemon 通知 service manager 之前接受请求；该窗口内 `daemon.ready` 必须为 `false`。只有 `READY=1` 成功后该值才可以变为 `true`。如果后续 mount 或 socket 的 descriptor-anchored metadata 校验失败，`ready` 必须重新表现为 `false`，对应组件不得继续报告 `ready`。

`doctor` 返回稳定的 `scrapefs.doctor/v1` JSON，包含同一 status 快照、整体 severity、severity 计数和 findings。severity 固定为 `healthy`、`warning` 或 `blocking`。findings 先按 blocking、warning、healthy 排序，再按 component、code、Library、target、node ID、node ID 列表、object ID 和 message 排序。

doctor 通过 daemon 内部 API 执行只读检查：

- SQLite schema、`PRAGMA quick_check` 与 `PRAGMA foreign_key_check`；
- ACTIVE/RETIRED Library、LibraryRoot 和 namespace tree 关系；
- Projection source 的安全 reopen 与 device/inode/size/mtime identity；
- Overlay reference、LIVE/STAGING object、缺失 object、冲突文件和 orphan；
- Repository 快照与当前内存 index 的完整值比较；
- mount path、ScrapeFS mount ownership、socket inode、mode、owner/group 和角色 ACL。

稳定 finding code 包括：

```text
DATABASE_OK
DATABASE_UNAVAILABLE
DATABASE_SCHEMA_MISMATCH
DATABASE_CORRUPT
NAMESPACE_OK
INVALID_NAMESPACE
LIBRARIES_OK
INVALID_LIBRARY
PROJECTIONS_OK
BROKEN_PROJECTION
STALE_PROJECTION
OBJECT_STORE_OK
OBJECT_STORE_UNAVAILABLE
MISSING_OBJECT
ORPHAN_OBJECT
INVALID_OBJECT_STORE
INDEX_OK
INDEX_UNAVAILABLE
INDEX_DIVERGENCE
RUNTIME_OK
RUNTIME_UNAVAILABLE
UNSAFE_RUNTIME
```

doctor 禁止调用 startup recovery、GC、Projection rebind 或任何 repair 路径。它不得修改 namespace version、operation log、Library、node、object record、backing file 或 source file。repair 和显式回收仍由独立的管理员操作完成。

`scrapefs gc` 需要管理员授权，并返回 `scrapefs.gc-result/v1`。数组和逐
object 错误使用确定性顺序；计数区分 LIVE、STAGING、无数据库记录、仍被引用、
活动 handle、grace period 和缺失但仍被引用等结果。结果同时包含 daemon 的启动
恢复报告，其中包括缺失 object ID 和引用它的 node ID。单个 object 清理失败会记录
在结果中，不得因此放宽其他候选 object 的删除条件。

`library add` 使用调用管理员的 peer UID/GID 创建 ACTIVE Library，并使用固定的 v1 mode：LibraryRoot 和目录为 `0775`，Projection 为 `0444`，OverlayFile 为 `0664`。`library rename` 在一个事务中同时重命名 ACTIVE Library 记录及其受保护的 LibraryRoot，保留二者稳定 ID，写一条 operation log，并只增加一次 namespace version。新名称重复或无效时，Library、LibraryRoot、namespace version 和 operation log 均保持不变。

`library remove <name>` 将空 Library 标记为 retired。非空 Library 在未显式指定 `--recursive` 时返回稳定的 `NotEmpty` 错误。存在其他在途 namespace write 时，recursive remove 不等待，而是返回 `NamespaceWriteInProgress`。成功删除会移除节点树、清空 `root_node_id`、将 Library 标记为 RETIRED、写一条 operation log，并只增加一次 namespace version。`scrapefs.library-result/v1` 响应包含 `root_node_id` 为零的 RETIRED Library。源文件与 Projection Batch 历史保持不变；Batch list/show 继续可用，undo 返回 `LibraryNotFound`。Overlay object 在事务提交后进入回收条件，已经打开的 handle 在关闭前仍可使用。

`library set-profile` 修改后续 direct Projection 和 Projection Batch validate/apply 使用的 Profile，不移动或重命名已有节点，写一条 operation log，且不增加 namespace version。只接受内置的 `jellyfin@1` 和 `plex@1` Profile。`library add`、`library rename`、`library remove` 和 `library set-profile` 需要管理员授权。`library list` 按名称确定性排序返回 ACTIVE Library，管理员和控制用户均可调用。

`projection add`、`projection mv` 和 `projection rm` 返回 `scrapefs.projection-result/v1`；remove 返回已删除 Projection 的快照。源文件选择、目标校验与 namespace mutation 仅在 control 授权成功后执行。`projection list` 使用 `scrapefs.projection-list/v1` 返回 direct 与 Batch Projection，按 Library、相对 target 和 node ID 确定性排序。`--library <name>` 只返回指定 ACTIVE Library；列表使用持久化缓存 metadata，不打开源文件。

`projection mv` 只接受同一 ACTIVE Library 内不存在的目标，保留 node ID 和 source metadata，在同一事务中创建缺失的 managed 目标目录，写一条 operation log，并使 namespace version 增加 1。跨 Library move 返回 `CrossLibrary`；目标已存在返回 `TargetConflict`。`projection rm` 只删除 Projection 节点，并向 LibraryRoot 清理空的 managed Directory；用户创建或非空目录必须保留。它写一条 operation log，并使 namespace version 增加 1。两个命令均不得打开、重命名或删除源文件。

不带 `--confirm` 的 `projection rebind` 会安全打开候选源文件，并返回 `scrapefs.projection-rebind-result/v1`：status 为 `confirmation_required`，同时包含稳定 Projection 快照、完整的新旧缓存 source metadata，以及绑定 node ID、target、source path 和严格 identity 的 confirmation digest。使用 `--confirm <digest>` 重复执行时，daemon 会重新打开并重新校验 source，只应用与 preview 完全一致的变更，并返回 `applied`。Projection 或 source 已变化时返回 `ConfirmationMismatch`，不产生 mutation。成功 rebind 保留 node ID 和虚拟 target，更新 source metadata 与虚拟 ctime，写一条 audit 记录，但不增加 namespace version。

这三个 lifecycle 命令只适用于 `origin_scrape_id` 为空的 direct Projection。由 `projection batch apply` 创建的 Projection 返回 `BatchOwnedProjection`；其路径、source metadata、Batch entry、managed Directory、namespace version 和 operation log 均保持不变。Batch-owned Projection 继续通过 Batch show 和 undo 管理。

所有 Projection 命令均要求管理员或控制用户授权。目标不存在返回 `ProjectionNotFound`；rebind 的不安全、缺失、无权限或非普通源文件沿用 `projection add` 的稳定 source error code。

`projection batch validate` 和 `projection batch apply` 使用 `--file <path>` 读取 UTF-8 JSON，并使用 `--file -` 从 stdin 读取。非 TTY 或显式机器可读模式不得提示输入或自动 apply。CLI 默认连接 `/run/scrapefs/scrapefs.sock`，可使用全局 `--socket <path>` 覆盖为配置的路径。

## 8. 第一版配置

### 8.1 配置文件

第一版使用 TOML。

默认配置文件固定为：

```text
/etc/scrapefs/config.toml
```

可以通过以下参数覆盖默认文件：

```bash
scrapefsd --config /path/to/config.toml
```

选中的配置文件不存在、不可读、语法错误、包含未知字段或验证失败时，daemon 必须在创建数据库、目录、socket、lock 或 mount 之前退出。

第一版不读取环境变量配置，不合并多份配置，不支持 live reload。

### 8.2 Schema 和默认值

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

`storage.overlay_store_path` 是 OverlayFile 内容的实际落盘根目录，也就是 object store 根目录，不再配置第二个 Overlay 路径。

外部 Agent 不通过 daemon 配置。模型、metadata provider、凭据、媒体工具和 Skill 配置均由外部 Agent 自己管理。

legacy `[agent]` 表不属于 v1 daemon 配置，解析时必须作为未知配置拒绝。升级旧配置文件时，必须删除整个 `[agent]` 表。原 upload 和 session 限制不得解释为 Overlay quota；外部 Agent 的限制必须在 `scrapefsd` 之外配置。

固定行为：

- `kernel_cache = off`；
- `writeback_cache = off`；
- `auto_cache = on`；
- socket group 为 `scrapefs-control`。

这些行为不作为配置项。

### 8.3 值的表示

- UID 使用 `0` 至 `4294967294` 范围内的非负 TOML integer；`4294967295` 是 Linux ACL encoding 的保留值，文档示例使用十进制；
- file mode 使用 TOML 八进制整数，例如 `0o660`，且只能包含 `0o001` 至 `0o777` 的权限位；`control.socket_mode` 必须包含 owner 和 group 读写位（`0o660`）；
- duration 使用 Go duration 风格的字符串，例如 `"2s"` 和 `"24h"`；
- 路径使用字符串。

解析必须拒绝未知字段、超出范围的 UID 和无效 mode。FUSE TTL 必须非负；`orphan_grace_period` 必须为正数。`negative_timeout` 可以为 `0s`。

### 8.4 路径约束

以下四个路径必须是经过词法规范化的绝对路径：

- SQLite 数据库路径；
- Overlay object store 路径；
- FUSE mountpoint；
- Unix socket 路径。

四个路径必须两两分离。任一路径不得与另一路径相等，也不得是另一路径的祖先或后代。

配置加载和验证不得创建、打开或修改这些路径。路径存在性、所有权、权限和 symlink-safe 打开由实际使用该路径的启动组件负责。

已有的 SQLite `-journal`、`-wal` 和 `-shm` sidecar 属于数据库状态，必须采用与主数据库文件相同的 owner、私有 mode、普通文件和 no-symlink 校验。daemon 在 SQLite 生命周期内持续持有已校验的数据库父目录，并相对该描述符访问主文件和 sidecar。

第一版不需要配置 Source Root、动态 Profile 或通用 RBAC。
