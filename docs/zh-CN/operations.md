# 运行与 CLI

[English](../en/operations.md) | **简体中文**

## 1. 控制面授权

第一版使用三个身份边界：

1. **管理员**：root 或配置的管理员 UID，可管理 Library 和 daemon。
2. **控制用户**：属于 `scrapefs-control` 组，可选择源路径、创建 Projection、执行 Scrape、apply/undo、查看 status/doctor。
3. **Agent provider**：在 daemon 进程内运行，或由 daemon 以自身有效 UID 启动为受信 worker；只能通过 Agent controller 访问当前 session。

推荐 socket：

```text
path: /run/scrapefs/scrapefs.sock
owner: scrapefs
group: scrapefs-control
mode: 0660
```

daemon 必须通过 Unix peer credentials 获取调用者身份。

`scrapefs-control` 成员是可信源路径选择者。daemon 服务账号应只拥有预期媒体目录的读取权限，避免控制用户借 daemon 读取其他敏感文件。

Agent session token 不能用于普通控制命令。

第一版不单独配置 Agent UID，也不将 Unix UID 作为 daemon 与 Agent 之间的隔离边界。独立进程形式的 Agent worker 必须使用 daemon 的有效 UID，并携带当前 session token。不可信外部 Agent worker 需要独立 UID 或 sandbox，不属于第一版范围。

## 2. Library 生命周期

普通删除非空 Library 返回 `ENOTEMPTY`。

递归删除前不得存在：

- draft、waiting_input 或 ready Scrape；
- 活动 Agent session；
- 进行中的 artifact upload。

管理员必须先手动取消 session、discard Scrape，再执行 recursive remove。

recursive remove 必须在单个 SQLite 事务中：

1. 删除 Library 中所有 ProjectionFile 和 OverlayFile 节点。
2. 删除 Library 节点树。
3. 将 `libraries.root_node_id` 置空。
4. 将 Library 标记为 RETIRED。
5. 写 operation log。
6. namespace version 增加 1。

事务提交后回收 Overlay object。不得删除任何源文件。

## 3. Daemon 单实例

daemon 启动时必须获取：

```text
/run/scrapefs/scrapefsd.lock
```

使用：

```text
flock(LOCK_EX | LOCK_NB)
```

无法获得锁时立即退出，不得清理 mount 或写数据库。

## 4. 启动

1. 获取单实例锁。
2. 打开 SQLite 并验证 PRAGMA。
3. 执行 migration。
4. 校验和恢复 object store。
5. 重建内存索引。
6. 确认不存在存活 daemon 后清理 stale ScrapeFS mount。
7. 挂载配置的 FUSE mountpoint，默认为 `/mnt/scrapefs`。
8. 创建 Unix socket。
9. 发送 readiness。

不得卸载未知 FUSE 文件系统。

## 5. 正常停止

1. 停止接受新控制请求和命名空间写事务。
2. 等待在途事务完成。
3. 正常卸载 FUSE。
4. 关闭 socket 和 SQLite。
5. 释放单实例锁。

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

daemon：

```text
scrapefsd
```

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

[agent]
max_text_file_size = "4MiB"
max_binary_file_size = "32MiB"
max_session_total_size = "256MiB"
max_upload_duration = "120s"

[gc]
orphan_grace_period = "24h"
```

`storage.overlay_store_path` 是 OverlayFile 内容的实际落盘根目录，也就是 object store 根目录，不再配置第二个 Overlay 路径。

第一版不配置 Agent UID。Agent 使用 daemon 进程的有效 UID。

固定行为：

- `kernel_cache = off`；
- `writeback_cache = off`；
- `auto_cache = on`；
- socket group 为 `scrapefs-control`。

这些行为不作为配置项。

### 8.3 值的表示

- UID 使用 `uint32` 范围内的非负十进制整数；
- file mode 使用 TOML 八进制整数，例如 `0o660`，且只能包含 `0o001` 至 `0o777` 的权限位；
- duration 使用 Go duration 风格的字符串，例如 `"2s"`、`"120s"` 和 `"24h"`；
- size 使用带二进制单位的字符串，第一版支持 `KiB`、`MiB` 和 `GiB`；
- 路径使用字符串。

解析必须拒绝未知字段、超出范围的 UID、无效 mode 和非正 size quota。FUSE TTL 必须非负；`max_upload_duration` 和 `orphan_grace_period` 必须为正数。`negative_timeout` 可以为 `0s`。

### 8.4 路径约束

以下四个路径必须是经过词法规范化的绝对路径：

- SQLite 数据库路径；
- Overlay object store 路径；
- FUSE mountpoint；
- Unix socket 路径。

四个路径必须两两分离。任一路径不得与另一路径相等，也不得是另一路径的祖先或后代。

配置加载和验证不得创建、打开或修改这些路径。路径存在性、所有权、权限和 symlink-safe 打开由实际使用该路径的启动组件负责。

第一版不需要配置 Source Root、动态 Profile 或通用 RBAC。
