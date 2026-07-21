# 运行与 CLI

[English](../en/operations.md) | **简体中文**

## 1. 控制面授权

第一版使用三个身份边界：

1. **管理员**：root 或配置的管理员 UID，可管理 Library 和 daemon。
2. **控制用户**：属于 `scrapefs-control` 组，可选择源路径、创建 Projection、执行 Scrape、apply/undo、查看 status/doctor。
3. **Agent worker**：配置的 Agent UID，并且必须携带有效 session token，只能访问 Agent API。

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
7. 挂载 `/mnt/scrapefs`。
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

至少需要：

- SQLite 数据库路径；
- object store 路径；
- FUSE mountpoint；
- socket 路径和权限；
- 管理员 UID；
- Agent UID；
- FUSE cache TTL；
- Agent 上传配额；
- object GC grace period。

第一版不需要配置 Source Root、动态 Profile 或通用 RBAC。
