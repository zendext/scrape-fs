# 第一版验收标准

[English](../en/acceptance.md) | **简体中文**

## 1. Direct Projection

- `projection add` 可以从任意允许读取的本地文件系统创建映射。
- 源路径经过安全打开和普通文件校验。
- 创建缺失父目录和 ProjectionFile 在单事务中完成。
- 目标冲突时不产生部分目录或节点。
- `projection rm` 不删除源文件。
- 删除 Projection 后清理空的 managed Directory。

## 2. Scrape

- Scrape 只枚举用户指定路径。
- 输入为文件时不遍历目录。
- Scrape 文件清单创建后不可修改。
- Agent 只能引用当前 Scrape 文件 ID。
- Answer 完成后重新分析、渲染和校验。
- apply 请求必须匹配用户刚确认的 revision。
- revision 变化后旧确认请求失败。
- apply 不产生部分 Projection。
- apply 失败后 Scrape 保持 ready。
- applied 不可再次 apply。
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

## 5. Agent

- Agent 不能 apply、undo、discard 或管理 Library。
- Agent 不能提交任意绝对路径。
- Agent 只能读取当前 Scrape 文件。
- applied 前 Agent 不能创建可见 artifact。
- Agent 只能写入当前 Scrape 的 artifact root。
- Agent 不能覆盖已有文件。
- session 取消、过期、Scrape undo 或 Library retire 后不能继续写入。
- Agent 上传配额生效。

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
- Agent 创建的 OverlayFile 使用执行 apply 的确认者 UID/GID 和 Library overlay mode。
- Overlay backing object 使用 daemon owner 和 `0600`，不跟随虚拟 chmod/chown。
- ProjectionFile 始终只读。
- daemon 在打开 SQLite 前获取宿主文件系统上的 `/run/scrapefs/scrapefsd.lock`。
- readiness 只在数据库、索引、mount 和 socket 全部可用后发送。
- 未挂载时播放器看不到可读空目录。
- 正常停止不使用 lazy unmount。
- daemon 重启后可以从 SQLite 和 object store 恢复。

## 8. 第一版 Agent 范围

应支持：

- 单文件电影；
- 一目录一电影；
- 标准 SxxEyy；
- 一季多集；
- 常规外挂字幕；
- sample 忽略；
- Plex 和 Jellyfin；
- 读取已有 NFO；
- 结构化问题；
- 人工修订和路径 override。

可以返回 `UnsupportedFeature`：

- BDMV、DVD；
- 动画绝对集数；
- 多集单文件；
- edition 自动识别；
- 复杂 extras；
- 音乐和图片库；
- 远程网络源。
