# 运行与 CLI

[English](../en/operations.md) | **简体中文**

## 1. 控制面授权

第一版控制面使用两个身份角色：

1. **管理员**：root 或配置的管理员 UID，可管理 Library 和 daemon。
2. **控制用户**：属于 `scrapefs-control` 组，可选择源路径、创建 Projection、validate/apply/undo Projection Batch、查看 status/doctor。

推荐 socket：

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

控制服务启动时，daemon 必须解析固定的 `scrapefs-control` group。group 不存在或 GID 不是有效数值时，不得创建 socket。

daemon 通过 `SO_PEERCRED` 获取调用者的有效 UID/GID，通过 `SO_PEERGROUPS` 获取 supplementary groups，并忽略 client 提交的身份。root 和 `control.administrator_uid` 可以执行管理员与控制用户命令；其他调用者执行 Projection、Projection Batch、status 和 doctor 命令时，其有效或 supplementary group 必须包含 `scrapefs-control` GID。未授权请求必须在调用业务服务前返回稳定的 `PermissionDenied` 错误。

socket owner 使用 daemon effective UID，group 使用 `scrapefs-control` GID。socket 父目录必须由 daemon 所有、不得允许 group 或 other 写入，并且必须在不跟随 symlink 的情况下打开。只有类型为 socket 且 UID/GID 符合预期的 stale path 才能删除；其他文件类型、symlink 或异常 ownership 必须 fail closed。

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

不得卸载未知 FUSE 文件系统。

## 5. 正常停止

1. 停止接受新控制请求和命名空间写事务。
2. 等待在途事务完成。
3. 正常卸载 FUSE。
4. 关闭 socket 和 SQLite。
5. 释放 daemon 锁。

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

所有已实现的 Projection Batch 命令都输出机器可读 JSON。`projection batch validate` 和 `projection batch apply` 使用 `--file <path>` 读取 UTF-8 JSON，并使用 `--file -` 从 stdin 读取。非 TTY 或显式机器可读模式不得提示输入或自动 apply。CLI 默认连接 `/run/scrapefs/scrapefs.sock`，可使用全局 `--socket <path>` 覆盖为配置的路径。

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

当前已实现的早期配置解析器仍兼容以下 legacy 字段：

```toml
[agent]
max_text_file_size = "4MiB"
max_binary_file_size = "32MiB"
max_session_total_size = "256MiB"
max_upload_duration = "120s"
```

这些字段不配置外部 Agent，也不属于目标 v1 Agent Skill 工作流。后续实现任务必须在删除或改为通用 Overlay 限制之间作出兼容性决定；在该任务完成前，当前解析和校验行为保持不变。

固定行为：

- `kernel_cache = off`；
- `writeback_cache = off`；
- `auto_cache = on`；
- socket group 为 `scrapefs-control`。

这些行为不作为配置项。

### 8.3 值的表示

- UID 使用 `uint32` 范围内的非负 TOML integer，文档示例使用十进制；
- file mode 使用 TOML 八进制整数，例如 `0o660`，且只能包含 `0o001` 至 `0o777` 的权限位；
- duration 使用 Go duration 风格的字符串，例如 `"2s"`、`"120s"` 和 `"24h"`；
- size 使用带二进制单位的字符串，第一版支持 `KiB`、`MiB` 和 `GiB`；
- 路径使用字符串。

解析必须拒绝未知字段、超出范围的 UID、无效 mode 和非正 size quota。对 legacy `[agent]` 字段，单文件 quota 不得大于 `max_session_total_size`，`max_upload_duration` 必须为正数。FUSE TTL 必须非负；`orphan_grace_period` 必须为正数。`negative_timeout` 可以为 `0s`。

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
