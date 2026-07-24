# 文件系统与对象存储

[English](../en/filesystem.md) | **简体中文**

本文定义 Directory、OverlayFile、权限、rename、object store 和缓存语义。ProjectionFile 的专用语义见 [Projection 模型](projection.md)。

实现状态：Directory、OverlayFile 和 ProjectionFile 的 FUSE I/O 及受支持的 namespace mutation 均已实现。

## 1. 文件属性

Directory 的 mode、uid、gid、atime、mtime 和虚拟 ctime 保存在 SQLite。

创建、删除或移动子节点时必须更新相关父目录 mtime 和虚拟 ctime。

OverlayFile getattr 返回：

```text
type    = regular file
mode    = SQLite mode
uid/gid = SQLite uid/gid
size    = object fd fstat.size
atime   = object fd fstat.atime
mtime   = object fd fstat.mtime
ctime   = max(object ctime, virtual_ctime_ns)
inode   = ScrapeFS inode
nlink   = 1
```

metadata 修改后必须失效 inode attribute cache。

第一版使用 noatime 或近似行为。

## 2. 权限

FUSE mount 必须启用：

```text
allow_other
default_permissions
nodev
nosuid
noexec
```

普通权限由内核根据 mode、uid 和 gid 执行。

通过 FUSE 创建 OverlayFile 或 Directory 时：

- UID/GID 来自 FUSE request context；
- mode 使用内核传入的、已应用 umask 的值；
- daemon 不得重复应用 umask。

虚拟 UID/GID/mode 保存在 SQLite，与 backing object 的 Unix owner 和 mode 相互独立。

外部 Agent 创建 OverlayFile 时使用普通 FUSE 操作，因此遵循相同的 FUSE request context、umask 和权限检查。ScrapeFS 不提供绕过这些规则的 Agent controller。

确定错误：

- ProjectionFile chmod/chown：`EROFS`；
- GlobalRoot/LibraryRoot chmod/chown：`EPERM`；
- 在 GlobalRoot 下创建目录项：`EPERM`；
- 普通 Directory/OverlayFile：按权限允许或由内核拒绝。

LibraryRoot 自身保持受保护；具备普通内核权限的调用者可以在其下创建和删除普通子节点。

## 3. OverlayFile 操作

支持：

```text
create
open
read
write
truncate
flush
fsync
release
rename
unlink
chmod
chown
utimens
getattr
```

`fallocate` 返回 `EOPNOTSUPP`。

写入语义：

- 普通 write 使用等价于 `pwrite` 的 offset 语义；
- 支持部分写入和非顺序写入；
- backing 层部分成功时保留已写入内容并返回实际长度；
- `O_APPEND` 单次追加在同一 object 的并发 writer 间保持原子；
- `O_TRUNC` 在 open 返回前完成；
- flush 报告延迟错误，但不保证持久化；
- `fsync(false)` 调用 fsync；
- `fsync(true)` 调用 fdatasync；
- release 不提供持久化保证。

## 4. Directory 操作

支持：

```text
mkdir
opendir
readdir
releasedir
rename
rmdir
getattr
chmod
chown
utimens
```

rmdir 只允许删除空普通 Directory。

以下操作返回 `EOPNOTSUPP`：

```text
link
symlink
readlink
mknod
mkfifo
设备节点创建
socket 节点创建
```

## 5. Rename

第一版支持：

```text
flags = 0
RENAME_NOREPLACE
```

不支持 `RENAME_EXCHANGE` 和 `RENAME_WHITEOUT`。

`RENAME_NOREPLACE` 且目标存在时返回 `EEXIST`。

不支持的 rename flags 返回 `EINVAL`。

同一 Library 内允许：

- ProjectionFile rename 到不存在目标；
- OverlayFile rename 到不存在目标；
- Directory rename 到不存在目标；
- OverlayFile 原子替换 OverlayFile，且 flags=0。

拒绝：

- OverlayFile 覆盖 ProjectionFile：`EEXIST`；
- ProjectionFile 覆盖已有文件：`EEXIST`；
- 文件覆盖目录：`EISDIR`；
- 目录覆盖文件：`ENOTDIR`；
- 目录覆盖已有目录：`EEXIST`；
- 目录移动到自身子树：`EINVAL`。

普通 FUSE rename 和 `projection mv` 禁止跨 Library，返回 `EXDEV`。

## 6. Object Store

object store 根目录由 `storage.overlay_store_path` 配置，它是 OverlayFile 内容的实际落盘位置。默认布局：

```text
/var/lib/scrapefs/objects/
├── staging/
└── live/
```

object store 只允许 daemon 访问。目录和 backing object 使用 daemon 的有效 UID/GID：

```text
directory mode: 0700
object mode:    0600
```

FUSE chmod/chown 只修改 SQLite 中的虚拟元数据，不修改 backing object 的 owner 或 mode。

SQLite 保存：

```text
objects
├── id
├── state
├── created_at_ns
├── live_at_ns
└── updated_at_ns
```

状态：

```text
STAGING
LIVE
```

### 创建流程

1. SQLite 创建 STAGING object 记录。
2. 在 staging 中使用 `O_CREAT | O_EXCL` 创建文件。
3. 写入可选初始内容并 fsync object。
4. rename 到 live。
5. fsync staging 和 live 目录。
6. SQLite 事务将 object 改为 LIVE，创建 OverlayFile 节点，写日志并增加 namespace version。
7. 提交后返回成功或打开 handle。

数据库提交失败时不得返回成功。未引用 object 由恢复流程或 GC 清理。

启动恢复必须对照 descriptor-anchored staging/live 目录检查每一条数据库记录：

- 没有对应文件的 STAGING 记录可以删除；
- 文件已经进入 live 的 STAGING 记录必须完成为 LIVE；
- 仍有 staging 文件的 STAGING 记录保留给 GC；
- LIVE 记录被节点引用但 live 文件缺失时，必须报告完整性问题，并保持节点和数据库记录不变；
- 文件冲突或没有数据库记录的文件必须报告并保持不变，等待显式 GC 或管理员检查。

恢复流程不得为缺失但仍被引用的 object 创建替代内容。

### Unlink

1. SQLite 事务删除节点、记录日志并增加 namespace version。
2. 提交事务。
3. 删除 live object。

### 原子替换

1. SQLite 事务移除旧目标节点。
2. 将源节点移动到目标名称。
3. 记录日志并增加 namespace version。
4. 提交事务。
5. 删除旧目标 object。

已打开的旧 object fd 继续有效。

## 7. GC

GC 只能删除：

- 状态为 LIVE；
- 没有节点引用；
- 没有活动 handle；
- 超过 `orphan_grace_period`；

的 object。

推荐：

```text
orphan_grace_period = 24h
```

创建和 GC 必须共享进程级 coordinator。删除前必须再次检查 object 状态、节点引用和活动 handle。

create/open 返回的每一个 object handle 都必须跟踪到第一次 close。unlink/replacement
后的即时清理在存在活动 handle 或仍处于 grace period 时必须保留 object；handle
关闭且 grace period 到期后，后续 GC 才可以回收。

`scrapefs gc` 仅允许管理员执行。命令使用 `scrapefs.gc-result/v1` JSON
返回确定性的 examined、deleted、retained 计数，以及按 object ID 排序的错误。
符合条件的废弃 STAGING 记录、未引用 LIVE 记录和没有数据库记录的文件可以删除；
仍被引用、仍有活动 handle、仍在 grace period、文件状态冲突或缺失但仍被引用的
object 必须保留。

## 8. Missing Object

数据库存在 OverlayFile 节点但 live object 缺失时：

- open/getattr 返回 `EIO`；
- doctor 报告 missing object；
- 不创建空文件。

unlink 和 Overlay-over-Overlay replacement 先删除 namespace entry，再删除旧 live object。已经打开的 descriptor 在 object unlink 后继续有效。清理失败时保留未引用 object，交给恢复和 GC 流程处理；不得恢复已经过期的 namespace entry。

## 9. 缓存

推荐：

```text
kernel_cache      = off
writeback_cache   = off
auto_cache        = on

entry_timeout     = 1-3s
attr_timeout      = 1-3s
negative_timeout  = 0-1s
```

命名空间提交后应主动失效 old/new parent、old/new name 和相关 inode attributes。

metadata 修改后失效相关 inode attributes。

失效失败只产生 warning，由短 TTL 提供最终一致。
