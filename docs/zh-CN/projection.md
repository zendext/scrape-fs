# Projection 模型

[English](../en/projection.md) | **简体中文**

Projection 是外部源文件与 Library 虚拟路径之间的只读映射。

## 1. 直接创建

```bash
scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"
```

目标路径格式：

```text
<library-name>:/<relative-path>
```

相对路径必须：

- 是有效 UTF-8；
- 不包含 `..`、NUL 或空组件；
- 不逃逸 LibraryRoot；
- 不使用保留名称；
- 每个组件不超过 255 字节，规范化相对路径不超过 4095 字节；
- 通过当前 Library Profile 的路径安全检查。

通用校验必须在命名空间修改前拒绝绝对路径和词法上未规范化的路径，随后再执行 Profile 特定的命名与路径安全检查。

内置 `jellyfin@1` 和 `plex@1` Profile 均拒绝 Unicode 控制字符。`jellyfin@1` 还拒绝 Jellyfin 保留的 `<`、`>`、`:`、`"`、`\`、`|`、`?` 和 `*` 字符；`/` 已由通用组件校验拒绝。播放器命名建议不属于硬路径安全错误。

`projection add` 在一个 SQLite 事务中：

1. 校验源文件。
2. 创建缺失的 managed Directory。
3. 创建 ProjectionFile。
4. 写 operation log。
5. namespace version 增加 1。

目标冲突时不得留下部分目录或节点。

成功的 `projection add` 返回 `scrapefs.projection-result/v1` JSON，其中包含稳定的 Projection node ID、Library、完整虚拟 target，以及缓存的 source identity 和属性。

可以列出全部 ACTIVE Library 中的 Projection，或限制到一个 Library：

```bash
scrapefs projection list
scrapefs projection list --library jellyfin
```

`projection list` 返回 `scrapefs.projection-list/v1` JSON。结果必须按 Library 名、Library 内相对 target 和 node ID 确定性排序，并同时包含 direct 与 Batch 创建的 Projection。指定的 Library 不存在时返回 `LibraryNotFound`。列表只读取持久化 namespace 和缓存 source metadata，不重新打开源文件，因此源文件当前不可用不会使 Projection 从列表消失。

可以在不接触源文件的情况下移动或删除 Projection：

```bash
scrapefs projection mv \
  "jellyfin:/Movies/Dune/Dune.mkv" \
  "jellyfin:/Films/Dune.mkv"

scrapefs projection rm "jellyfin:/Films/Dune.mkv"
```

Move 只接受同一 Library 内不存在的目标。它保留稳定 node ID 和 source metadata，在同一事务中创建缺失的 managed 目标目录，写一条 operation log，并使 namespace version 增加 1。跨 Library move 返回 `CrossLibrary`；目标存在任何节点时返回 `TargetConflict`。

Remove 只删除 Projection 节点。它向 LibraryRoot 清理空的 managed Directory，同时保留用户创建和非空目录；写一条 operation log，并使 namespace version 增加 1。Move 或 remove 失败时，namespace、version、operation log 和所有源文件都保持不变。两个命令均返回 `scrapefs.projection-result/v1`。

## 2. 源路径安全

第一版接受绝对源路径，不使用 Source Root 或逻辑磁盘别名。

有权执行 `projection add` 和 `scrape` 的控制用户被视为可信源路径选择者。daemon 服务账号应只拥有预期媒体目录的读取权限。

daemon 必须：

1. 要求输入为绝对路径。
2. 使用根目录 fd 和去除前导 `/` 的相对路径调用 `openat2`。
3. 禁止 symbolic link、magic link、`..` 和路径逃逸。
4. 使用只读 fd 打开。
5. 使用 `fstat` 确认对象是普通文件。
6. 后续读取始终使用已打开 fd。

Linux 实现应使用：

```text
RESOLVE_BENEATH
RESOLVE_NO_MAGICLINKS
RESOLVE_NO_SYMLINKS
```

如果系统不支持安全的 `openat2` 语义，第一版可以拒绝启动，不得回退到存在 TOCTOU 的字符串路径检查。

安全打开成功后，daemon 保存规范化绝对路径。

## 3. 源文件身份

ProjectionFile 必须保存：

```text
source_path
source_device_id
source_inode
cached_size
cached_atime_ns
cached_mtime_ns
cached_source_ctime_ns
verified_at_ns
```

严格身份比较使用：

```text
device ID
inode
size
mtime_ns
```

open 流程：

1. 安全打开 `source_path`。
2. 执行 `fstat`。
3. 比较保存的身份。
4. 不匹配返回 `ESTALE`。
5. 匹配则返回只读 handle。

源路径不可访问时：

- ProjectionFile 继续可见；
- getattr 返回缓存属性；
- open 返回底层安全打开错误，例如 `ENOENT` 或 `ENODEV`；
- doctor 报告 broken projection。

## 4. Rebind

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv
```

首次调用只执行 preview，返回 `scrapefs.projection-rebind-result/v1`，status 为 `confirmation_required`，其中包含新旧 source metadata，以及绑定稳定 node ID、target、两个 source path 和两个严格 identity 的 digest。使用以下命令应用该精确 preview：

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv \
  --confirm sha256:<digest>
```

rebind 必须：

1. 安全打开新源文件。
2. 返回新旧路径、identity 和显式 confirmation digest。
3. 提交 digest 时重新打开并重新校验 source。
4. Projection 或 source 已变化时返回 `ConfirmationMismatch`。
5. 更新路径、身份、缓存属性和 `verified_at_ns`。
6. 更新虚拟 ctime。
7. 写 operation log。
8. 失效 inode attribute cache。

Preview 和确认失败不得修改状态。成功 rebind 保留 node ID 和 target，写一条 audit 记录，但不增加 namespace version。

## 5. 文件属性

ProjectionFile getattr 返回：

```text
type    = regular file
mode    = Library.projection_file_mode
uid/gid = Library.uid/gid
size    = 在线 fstat.size，否则 cached_size
atime   = 在线 fstat.atime，否则 cached_atime_ns
mtime   = 在线 fstat.mtime，否则 cached_mtime_ns
ctime   = max(cached_source_ctime_ns, virtual_ctime_ns)
inode   = ScrapeFS inode
nlink   = 1
```

源路径不可用、文件丢失或身份不匹配时，lookup/getattr 使用缓存属性。缓存缺失或损坏时返回 `EIO`。

## 6. FUSE 语义

支持：

```text
open read-only
read
release
getattr
rename
unlink
```

| 操作 | 错误 |
| --- | --- |
| open(O_WRONLY) | EROFS |
| open(O_RDWR) | EROFS |
| open(O_TRUNC) | EROFS |
| write | EROFS |
| truncate | EROFS |
| fallocate | EOPNOTSUPP |
| chmod/chown/utimens | EROFS |
| link/symlink | EOPNOTSUPP |

rename 只修改虚拟路径。unlink 只删除命名空间节点。已打开的源文件 fd 继续可读。

## 7. Rename

第一版支持 `flags=0` 和 `RENAME_NOREPLACE`。

`RENAME_NOREPLACE` 且目标存在时返回 `EEXIST`。

同一 Library 内允许 ProjectionFile rename 到不存在目标。

ProjectionFile 不得覆盖 ProjectionFile、OverlayFile 或目录。

普通 FUSE rename 和 `projection mv` 禁止跨 Library，返回 `EXDEV`。
