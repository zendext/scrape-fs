# 第一版用例

[English](../en/use-cases.md) | **简体中文**

本文从参与者目标出发描述 ScrapeFS 第一版的主要使用流程。具体数据结构、错误码和事务语义以对应规格文档为准。

## 1. 参与者

| 参与者 | 说明 |
| --- | --- |
| 管理员 | 管理 Library、daemon 和系统配置 |
| 控制用户 | 创建 Projection、validate/apply Batch、确认和撤销结果 |
| 外部 Agent Skill | 使用调用者权限识别媒体、生成 Batch 并调用 CLI |
| 媒体播放器 | 读取 ProjectionFile，并通过 FUSE 创建或修改 OverlayFile |
| daemon | 执行安全校验、持久化、FUSE 操作和恢复 |

## 2. 用例索引

| ID | 用例 |
| --- | --- |
| UC-01 | 创建 Library |
| UC-02 | 手动创建单条 Projection |
| UC-03 | Validate 外部 Agent 生成的 Projection Batch |
| UC-04 | 修正无效 Batch |
| UC-05 | 原子应用 Projection Batch |
| UC-06 | 通过 FUSE 创建媒体 artifact |
| UC-07 | 撤销 Projection Batch |
| UC-08 | 重命名或删除 Projection |
| UC-09 | 播放器原子更新 OverlayFile |
| UC-10 | 处理源文件丢失或被替换 |
| UC-11 | 将 Projection rebind 到新源文件 |
| UC-12 | 删除 Library |
| UC-13 | daemon 重启恢复 |
| UC-14 | 使用 doctor 检查异常 |
| UC-15 | 显式迁移 legacy Projection source identity |

## UC-01 创建 Library

### 参与者

管理员

### 目标

创建一个使用指定 Profile 的虚拟媒体库。

### 前置条件

- daemon 已 ready；
- Library 名称尚未被 ACTIVE Library 使用；
- Profile 已内置并可用。

### 触发

```bash
scrapefs library add jellyfin --profile jellyfin@1
```

### 主流程

1. CLI 将名称和 Profile 提交给 daemon。
2. daemon 校验名称和 Profile。
3. daemon 在事务中创建 LibraryRoot 和 Library 记录。
4. daemon 写 operation log 并增加 namespace version。
5. FUSE 根目录出现 `/mnt/scrapefs/jellyfin`。

### 备选流程

- 名称已被使用：返回 `EEXIST`。
- Profile 不存在：返回 `ProfileNotFound`。
- 数据库事务失败：不留下 LibraryRoot。

### 结果

Library 为 ACTIVE，可以接收 direct Projection 和 Projection Batch。

### 相关规格

- [系统架构](architecture.md)
- [运行与 CLI](operations.md)

## UC-02 手动创建单条 Projection

### 参与者

控制用户

### 目标

将一个现有媒体文件映射到 Library，不修改源文件。

### 前置条件

- daemon 已 ready；
- 目标 Library 为 ACTIVE；
- 调用者有控制权限；
- daemon 可以读取源文件；
- 目标路径尚未被占用。

### 触发

```bash
scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"
```

### 主流程

1. daemon 安全打开源文件。
2. daemon 确认对象是普通文件。
3. daemon记录源路径、身份和缓存属性。
4. daemon 校验 Library 目标路径。
5. daemon 在事务中创建缺失目录和 ProjectionFile。
6. daemon 写 operation log 并增加 namespace version。
7. 新虚拟路径通过 FUSE 可见并可读。

### 备选流程

- 源文件不存在：返回 `ENOENT`。
- 源路径包含 symlink：拒绝。
- 目标已存在：返回 `EEXIST`。
- Library 不存在：返回 `LibraryNotFound`。
- 事务失败：不留下部分目录或 ProjectionFile。

### 结果

ProjectionFile 可读且内容只读，源文件保持不变。

### 相关规格

- [Projection 模型](projection.md)
- [文件系统与对象存储](filesystem.md)

## UC-03 Validate 外部 Agent 生成的 Projection Batch

### 参与者

控制用户、外部 Agent Skill

### 目标

识别一个任意命名的电影目录，并在不改变 namespace 的情况下预览 Projection Batch。

### 前置条件

- daemon 已 ready；
- 目标 Library 为 ACTIVE；
- 外部 Agent 可以使用调用者权限读取源目录；
- daemon 服务账号可以读取 Batch 显式列出的 source。

### 触发

```bash
scrapefs projection batch validate --file /tmp/dune.batch.json --json
```

### 主流程

1. 外部 Agent 枚举源目录并使用自己的 `ffprobe` 和 metadata 工具。
2. 外部 Agent 处理媒体识别歧义并生成 `scrapefs.projection-batch/v1` JSON。
3. daemon 严格解析 Batch，并安全打开每个 source。
4. daemon 执行通用路径、Profile 和冲突检查。
5. daemon 返回 diff、warning、digest 和 namespace version。
6. daemon 关闭 source fd，不写 SQLite。

### 备选流程

- Schema、权限或 source 类型无效：validate 失败。
- source 在检查期间变化：返回 `ESTALE`。
- target 不安全或冲突：返回 blocking error。

### 结果

外部 Agent 获得审阅结果和 digest，FUSE namespace 与数据库保持不变。

### 相关规格

- [Projection Batch 与外部 Agent 工作流](scrape.md)
- [Projection 模型](projection.md)

## UC-04 修正无效 Batch

### 参与者

控制用户、外部 Agent Skill

### 目标

在外部解决媒体识别、target 或冲突问题后重新 validate Batch。

### 前置条件

- 上一次 validate 返回 blocking error 或用户要求修改；
- 外部 Agent 持有原始 Batch JSON。

### 触发

```bash
scrapefs projection batch validate --file corrected.batch.json --json
```

### 主流程

1. 外部 Agent 修改自己的 Batch 文件。
2. daemon 将修改后的文件视为全新无状态请求。
3. daemon 重新执行全部 source、路径、Profile 和冲突检查。
4. daemon 返回新的 digest 和 diff。

### 结果

daemon 不保存旧版本或 revision；外部 Agent 负责比较和保存修改历史。

### 相关规格

- [Projection Batch 与外部 Agent 工作流](scrape.md)

## UC-05 原子应用 Projection Batch

### 参与者

控制用户

### 目标

原子发布用户刚确认的 Batch。

### 前置条件

- CLI 已展示 validate 结果；
- 调用者确认应用；
- 调用者持有 validate 返回的 digest；
- Library 为 ACTIVE。

### 触发

```bash
scrapefs projection batch apply \
  --file /tmp/dune.batch.json \
  --expected-digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

### 主流程

1. daemon 重新解析 Batch 并校验 expected digest。
2. daemon 从 Unix peer credentials 获取确认者 UID/GID。
3. daemon 重新安全打开并校验全部 source。
4. daemon 重新检查路径、Profile 和冲突。
5. daemon 在一个 SQLite 事务中创建 applied Batch provenance、Directory 和 ProjectionFile。
6. daemon 写一条 batch operation log。
7. daemon 增加 namespace version 并提交。
8. daemon 更新内存索引并失效 FUSE 缓存。
9. CLI 返回稳定 Batch ID。

### 备选流程

- digest 不匹配：拒绝并要求重新 validate。
- source 变化：返回 `ESTALE`。
- target 发生冲突：返回 `EEXIST`。
- 事务失败：不留下 provenance、目录或 Projection。

### 结果

全部 Projection 同时发布，源文件保持不变。

### 相关规格

- [Projection Batch 与外部 Agent 工作流](scrape.md)
- [系统架构](architecture.md)

## UC-06 通过 FUSE 创建媒体 Artifact

### 参与者

外部 Agent Skill

### 目标

在 Batch applied 后创建 NFO、海报等 OverlayFile。

### 前置条件

- Batch 已 applied；
- 外部 Agent 对目标目录有 FUSE 写权限。

### 触发

```bash
cp poster.jpg \
  "/mnt/scrapefs/jellyfin/Movies/Dune (2021) [tmdbid-438631]/poster.jpg"
```

### 主流程

1. 外部 Agent 使用普通 FUSE create/write/rename。
2. OverlayFile 使用 FUSE 调用者 UID/GID 和普通 mode。
3. backing object 使用 daemon 私有权限。

### 结果

Artifact 可见；ScrapeFS 不记录其创建者是人工、脚本还是 Agent。

### 相关规格

- [文件系统与对象存储](filesystem.md)
- [Projection Batch 与外部 Agent 工作流](scrape.md)

## UC-07 撤销 Projection Batch

### 参与者

控制用户

### 目标

删除某次 Batch 创建的 Projection，同时保留源文件和 OverlayFile。

### 前置条件

- Batch 状态为 applied。

### 触发

```bash
scrapefs projection batch undo batch-42
```

### 主流程

1. daemon 在事务中删除 legacy `origin_scrape_id` 指向 Batch 的 ProjectionFile。
2. daemon 清理空的 managed Directory。
3. daemon 写 operation log。
4. daemon 将 Batch 标记为 undone，并保存调用者 UID/GID。
5. daemon 增加 namespace version 并提交。
6. CLI 列出仍然保留或阻止目录清理的 OverlayFile。

### 备选流程

- Projection 已被用户删除：视为已经不存在。
- 目录中仍有 OverlayFile 或其他 Projection：保留目录。
- Batch 已 undone：返回 `AlreadyUndone`。

### 结果

Batch 的 Projection 消失，源文件和 OverlayFile 保留。

### 相关规格

- [Projection Batch 与外部 Agent 工作流](scrape.md)

## UC-08 重命名或删除 Projection

### 参与者

控制用户或具有 FUSE 权限的用户

### 目标

修改当前虚拟命名空间，不影响源文件。

### 前置条件

- ProjectionFile 存在；
- 目标位于同一 Library；
- 调用者有对应权限。

### 触发

```bash
scrapefs projection mv \
  "jellyfin:/Movies/Dune/Dune.mkv" \
  "jellyfin:/Movies/Dune/Dune (2021).mkv"

scrapefs projection rm \
  "jellyfin:/Movies/Dune/Dune (2021).mkv"
```

### 主流程

1. daemon 校验源节点和目标路径。
2. daemon 在事务中修改或删除虚拟节点。
3. daemon 写 operation log 并增加 namespace version。
4. 删除后 daemon 清理空的 managed Directory。

### 备选流程

- 跨 Library rename：返回 `EXDEV`。
- 目标已存在：返回 `EEXIST`。
- 删除后目录仍有其他内容：保留目录。

### 结果

虚拟路径发生变化或消失，源文件不变。

### 相关规格

- [Projection 模型](projection.md)

## UC-09 播放器原子更新 OverlayFile

### 参与者

媒体播放器

### 目标

通过常见临时文件模式安全更新 NFO、海报或字幕。

### 前置条件

- Library 已挂载；
- 播放器对目标目录有写权限；
- 目标不是 ProjectionFile。

### 触发

播放器执行：

```text
create movie.nfo.tmp
write
fsync
rename movie.nfo.tmp -> movie.nfo
```

### 主流程

1. ScrapeFS 在 staging 创建 object。
2. object 安全进入 live。
3. SQLite 创建 OverlayFile 节点。
4. 播放器写入并 fsync。
5. OverlayFile 原子替换已有 OverlayFile。
6. 旧 object 在事务提交后回收。

### 备选流程

- 目标是 ProjectionFile：返回 `EEXIST`。
- 写入部分成功：保留已写入部分并返回实际长度。
- fsync 失败：传播底层错误。
- object 缺失：返回 `EIO`。

### 结果

播放器通过新路径读取完整的新内容，源文件不受影响。

### 相关规格

- [文件系统与对象存储](filesystem.md)

## UC-10 处理源文件丢失或被替换

### 参与者

媒体播放器、控制用户

### 目标

在源文件异常时保持命名空间可检查，并避免静默读取错误内容。

### 前置条件

- ProjectionFile 已存在。

### 触发

源文件被外部删除、移动或同路径替换。

### 主流程

1. lookup/getattr 使用缓存属性，ProjectionFile 保持可见。
2. open 安全打开源路径并检查身份。
3. 文件不存在时返回 `ENOENT`。
4. 身份不匹配时返回 `ESTALE`。
5. doctor 报告 broken 或 stale Projection。

### 结果

ScrapeFS 不自动删除 Projection，也不静默投影新文件。

### 相关规格

- [Projection 模型](projection.md)

## UC-11 将 Projection rebind 到新源文件

### 参与者

控制用户

### 目标

在源文件被合法移动或替换后，将现有虚拟路径绑定到新的源文件。

### 前置条件

- ProjectionFile 存在；
- 新源文件可安全读取。

### 触发

```bash
scrapefs projection rebind \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  --source /mnt/new-disk/Dune.mkv
```

### 主流程

1. daemon 安全打开新源文件。
2. CLI 返回旧、新路径和 identity，以及 confirmation digest。
3. 用户通过使用 `--confirm <digest>` 重复命令进行显式确认。
4. daemon 重新打开 source，并拒绝已变化的 Projection 或 source。
5. daemon 更新 source path、身份和缓存属性。
6. daemon 更新虚拟 ctime、写 operation log 并失效属性缓存。
7. 新 open 从新源文件读取。

### 备选流程

- 新源文件不存在：返回 `ENOENT`。
- 新路径包含 symlink：拒绝。
- 未提交 confirmation：只返回 preview，不修改 Projection。
- confirmation 已过期：返回 `ConfirmationMismatch`，不修改 Projection。

### 结果

虚拟路径和 inode 保持不变，内容来源切换到新文件。

### 相关规格

- [Projection 模型](projection.md)

## UC-12 删除 Library

### 参与者

管理员

### 目标

删除不再使用的虚拟媒体库，不删除源文件。

### 前置条件

- daemon 已 ready。

### 触发

```bash
scrapefs library remove jellyfin
scrapefs library remove jellyfin --recursive
```

### 主流程

空 Library：

1. daemon 删除 LibraryRoot。
2. daemon 将 Library 标记为 RETIRED。
3. daemon 写 operation log 并增加 namespace version。

非空 Library 使用显式 recursive remove：

1. 管理员等待在途命名空间写事务结束。
2. daemon 在事务中删除 Library 节点树。
3. daemon 将 `root_node_id` 置空并标记 RETIRED。
4. 事务提交后回收 Overlay object。

### 备选流程

- 普通删除非空 Library 返回 `NotEmpty`（在 namespace/FUSE 语义中为 `ENOTEMPTY`）。
- 存在在途命名空间写事务时，recursive remove 返回 `NamespaceWriteInProgress`。

### 结果

Library 从 FUSE 根目录消失，命令返回其 RETIRED 快照。源文件与 Projection Batch 历史保持不变；已经打开的 Overlay handle 在关闭前仍可使用。

### 相关规格

- [运行与 CLI](operations.md)
- [系统架构](architecture.md)

## UC-13 daemon 重启恢复

### 参与者

管理员、systemd

### 目标

在正常重启或崩溃后恢复虚拟媒体库。

### 前置条件

- SQLite 和 object store 仍然存在。

### 触发

`scrapefsd` 启动。

### 主流程

1. daemon 获取 ScrapeFS 运行时的宿主文件系统锁。
2. daemon 打开 SQLite 并验证 PRAGMA。
3. daemon 执行 migration。
4. daemon 恢复 object store 状态。
5. daemon 从 SQLite 重建命名空间和 inode 索引。
6. daemon 清理确认属于死亡实例的 stale mount。
7. daemon 挂载 FUSE 并创建控制 socket。
8. daemon 发送 readiness。

### 备选流程

- 无法获得 daemon 锁：在打开 SQLite 前退出。
- migration 失败：不挂载、不发送 readiness。
- object 缺失：保留节点并由 doctor 报告。
- 发现未知 FUSE mount：不得卸载。

### 结果

已提交 Library、Projection 和 OverlayFile 恢复可见。

### 相关规格

- [运行与 CLI](operations.md)
- [文件系统与对象存储](filesystem.md)

## UC-14 使用 doctor 检查异常

### 参与者

控制用户、管理员

### 目标

检查不会被 ScrapeFS 自动修复的持久化或源文件异常。

### 前置条件

- daemon 控制 socket 可连接；控制用户已经通过授权。

### 触发

```bash
scrapefs doctor
```

### 主流程

1. doctor 检查 SQLite schema、quick check 和 foreign-key check。
2. doctor 检查 Projection 源路径和身份。
3. doctor 检查 OverlayFile 是否存在对应 live object。
4. doctor 检查无节点引用的 orphan object。
5. doctor 检查 Library、节点树和内存 index 约束。
6. doctor 检查 mount/socket runtime metadata。
7. doctor 输出按稳定顺序排列的结构化问题列表和建议操作。

### 报告示例

```text
BROKEN_PROJECTION
STALE_PROJECTION
MISSING_OBJECT
ORPHAN_OBJECT
INVALID_NAMESPACE
```

### 结果

doctor 默认不修改源文件或命名空间。需要修改的操作由用户显式执行。

### 相关规格

- [Projection 模型](projection.md)
- [文件系统与对象存储](filesystem.md)
- [运行与 CLI](operations.md)

## UC-15 显式迁移 legacy Projection source identity

### 参与者

控制用户、管理员

### 目标

在普通文件系统 remount 改变 device ID 后，使未更换、未原地修改的 legacy Projection 使用稳定 filesystem identity，同时继续拒绝真实替换或内容变化。

### 前置条件

- daemon 已 ready；
- 目标 Library 为 ACTIVE；
- 调用者有控制权限；
- legacy source 可以由 daemon 安全打开。

### 触发

```bash
scrapefs projection identity migrate --library jellyfin
```

### 主流程

1. daemon 按稳定顺序读取全部 legacy Projection，并安全重新打开每个 source。
2. path、inode、size、mtime 和缓存 ctime 相同的记录成为 candidate；device ID 可以不同，其他记录作为 rejection 返回。
3. daemon 返回全部 candidate、rejection 和绑定完整结果的 digest，不写数据库。
4. 用户审阅后原样提交 `--confirm sha256:<digest>`。
5. daemon fresh scan；digest 完全相同时，在一个事务中迁移 direct 与 Batch-owned candidate，并保留 Batch provenance。
6. commit 后 daemon 失效受影响 inode attribute cache。

### 备选流程

- ctime 或其他必需字段不同：该记录被拒绝并保持 legacy。
- preview 后 source、Projection、target、Batch ownership 或候选集合变化：返回 `ConfirmationMismatch`，不迁移任何记录。
- Library 不存在或不是 ACTIVE：返回 `LibraryNotFound`。

### 结果

成功 candidate 的 FUSE open 和 doctor 使用 filesystem type、完整 filesystem ID、inode、size 与 mtime 校验，不再比较 device ID。source、虚拟 target、node ID、Batch digest 和 namespace version 保持不变；daemon 重启不会自动迁移仍为 legacy 的记录。

### 相关规格

- [Projection 模型](projection.md)
- [系统架构](architecture.md)
- [运行与 CLI](operations.md)
