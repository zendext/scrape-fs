# 第一版验收标准

[English](../en/acceptance.md) | **简体中文**

## 1. Direct Projection

- `projection add` 可以从任意允许读取的本地文件系统创建映射。
- 源路径经过安全打开和普通文件校验。
- 创建缺失父目录和 ProjectionFile 在单事务中完成。
- 目标冲突时不产生部分目录或节点。
- `projection rm` 不删除源文件。
- 删除 Projection 后清理空的 managed Directory。

## 2. Projection Batch

- Batch 只接受版本化 UTF-8 JSON。
- 未知字段、无效类型、空 entries、重复 source 和重复 target 被拒绝。
- 每个 entry 显式包含绝对 source 和 Library-relative target。
- daemon 不根据 Batch 枚举目录、读取 source 内容、执行命令或访问网络。
- validate 安全打开全部 source，返回 diff、warning、digest 和 namespace version。
- validate 不写 SQLite，也不创建 pending Batch、snapshot 或 revision。
- digest 使用 `sha256:<64 位小写十六进制>`，不受 JSON 空白或对象字段顺序影响，并保留 `entries` 数组顺序。
- apply 要求 expected digest，并重新校验全部 source、Profile、路径和冲突。
- apply 在一个事务中创建 provenance 和全部 Projection，不产生部分结果。
- apply 失败不留下 Batch provenance。
- 成功 apply 返回稳定 Batch ID。
- undo 在单事务中完成。
- undo 不删除 OverlayFile。

## 3. ProjectionFile

- 支持跨物理磁盘。
- 可以读取和 seek。
- O_WRONLY、O_RDWR、O_TRUNC、write 和 truncate 返回 `EROFS`。
- rename 不修改源文件。
- unlink 不删除源文件。
- unlink 后已打开 handle 继续可读。
- 跨 Library rename 返回 `EXDEV`。
- 源文件丢失时 getattr 使用缓存属性。
- 同路径替换文件时 open 返回 `ESTALE`。
- daemon 重启后 inode 稳定。

## 4. OverlayFile

- 支持 create、open、read、write、truncate、flush、fsync 和 rename。
- 支持部分写、随机写和原子单次 append。
- O_TRUNC 在 open 返回前完成。
- flush 不承诺持久化。
- fsync 传播底层错误。
- Overlay 可以原子替换 Overlay，但不能覆盖 Projection。
- O_CREAT|O_EXCL 和 RENAME_NOREPLACE 正确返回 `EEXIST`。
- fallocate、link、symlink、mknod 和 mkfifo 失败。
- object 创建失败不产生可见节点。
- missing object 返回 `EIO`。
- GC 不删除仍被引用、有活动 handle 或 grace period 内的 object。

## 5. 外部 Agent 集成

- `scrapefsd` 不内置模型 provider、Agent session 或工具调用循环。
- 外部 Agent 使用调用者已有的宿主文件系统、控制 CLI 和 FUSE 权限。
- 外部 Agent 通过 `scrapefs.projection-batch/v1` 提交显式 source-to-target Batch。
- 自动化命令提供机器可读 JSON，非 TTY 模式不提示输入或自动 apply。
- Batch applied 后，外部 Agent 通过普通 FUSE 操作创建 OverlayFile。
- ScrapeFS 不根据创建者是人工、脚本或 Agent 改变 OverlayFile 语义。

## 6. Library

- Library 作为 FUSE 根目录下的一级目录出现。
- 普通 FUSE 用户不能创建、删除或重命名 LibraryRoot。
- Library Profile 切换不修改已有 Projection。
- 非空普通 remove 返回 `ENOTEMPTY`。
- recursive remove 不删除源文件。
- recursive remove 后 Overlay object 进入回收流程。

## 7. 权限与生命周期

- FUSE mount 启用 `default_permissions`。
- FUSE create 使用调用者 UID/GID 和内核处理后的 mode。
- 外部 Agent 通过 FUSE 创建的 OverlayFile 使用 FUSE 调用者 UID/GID 和普通 create mode。
- Overlay backing object 使用 daemon owner 和 `0600`，不跟随虚拟 chmod/chown。
- ProjectionFile 始终只读。
- daemon 在打开 SQLite 前获取宿主文件系统上的 `/run/scrapefs/scrapefsd.lock`。
- 数据库路径校验后替换祖先目录不得重定向 SQLite；拒绝更新版本 schema 时不得改变 journal mode。
- readiness 只在数据库、索引、mount 和 socket 全部可用后发送。
- 未挂载时播放器看不到可读空目录。
- 正常停止不使用 lazy unmount。
- daemon 重启后可以从 SQLite 和 object store 恢复。

## 8. 第一版 Agent Skill 范围

Skill 应当覆盖：

- 单文件电影；
- 一目录一电影；
- 标准 SxxEyy；
- 一季多集；
- 常规外挂字幕；
- sample 忽略；
- Plex 和 Jellyfin；
- 读取已有 NFO；
- 生成 `scrapefs.projection-batch/v1`；
- Batch validate、用户确认、apply、show 和 undo；
- apply 后通过 FUSE 创建 NFO 和海报。

Skill 可以拒绝或要求人工处理：

- BDMV、DVD；
- 动画绝对集数；
- 多集单文件；
- edition 自动识别；
- 复杂 extras；
- 音乐和图片库；
- 远程网络源。

这些格式能力属于 Skill，不改变 ScrapeFS Batch Schema。ScrapeFS 只要求 Batch 中的 Projection source 是可安全打开的本地 regular 文件。
