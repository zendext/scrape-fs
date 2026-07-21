# 第一版用例

[English](../en/use-cases.md) | **简体中文**

本文从参与者目标出发描述 ScrapeFS 第一版的主要使用流程。具体数据结构、错误码和事务语义以对应规格文档为准。

## 1. 参与者

| 参与者 | 说明 |
| --- | --- |
| 管理员 | 管理 Library、daemon 和系统配置 |
| 控制用户 | 创建 Projection、执行 Scrape、确认和撤销结果 |
| Agent | 在受限 session 中识别媒体和生成 Scrape revision |
| 媒体播放器 | 读取 ProjectionFile，并通过 FUSE 创建或修改 OverlayFile |
| daemon | 执行安全校验、持久化、FUSE 操作和恢复 |

## 2. 用例索引

| ID | 用例 |
| --- | --- |
| UC-01 | 创建 Library |
| UC-02 | 手动创建单条 Projection |
| UC-03 | Scrape 一个电影目录 |
| UC-04 | 回答媒体识别歧义 |
| UC-05 | 修改 Scrape 目标路径 |
| UC-06 | 应用 Scrape |
| UC-07 | 撤销 Scrape |
| UC-08 | 重命名或删除 Projection |
| UC-09 | 播放器原子更新 OverlayFile |
| UC-10 | 处理源文件丢失或被替换 |
| UC-11 | 将 Projection rebind 到新源文件 |
| UC-12 | 删除 Library |
| UC-13 | daemon 重启恢复 |
| UC-14 | 使用 doctor 检查异常 |

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

Library 为 ACTIVE，可以接收 Projection 和 Scrape。

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

## UC-03 Scrape 一个电影目录

### 参与者

控制用户、Agent

### 目标

识别一个任意命名的电影目录，并生成可预览的 Projection 草稿。

### 前置条件

- daemon 已 ready；
- 目标 Library 为 ACTIVE；
- daemon 可以读取用户指定目录；
- Agent provider 可用。

### 触发

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

### 主流程

1. daemon 只枚举指定目录及必要后代。
2. daemon 创建不可修改的 Scrape 文件清单。
3. daemon 创建受限 Agent session。
4. Agent 读取文件名、现有文本元数据和受控 probe 结果。
5. Agent 生成 MediaModel。
6. Jellyfin Profile 生成 RenderedEntry。
7. daemon 执行路径安全和冲突检查。
8. Scrape 进入 ready。
9. CLI 展示媒体信息、Projection 树、ignored 文件和 warning。

### 备选流程

- 输入是普通文件：只检查该文件。
- Agent 无法确定媒体：Scrape 进入 waiting_input。
- 源文件在分析期间变化：相关操作返回 `ESTALE`。
- 路径安全检查失败：Scrape 不进入 ready。
- Agent 失败：Scrape 保持 draft 并记录错误。

### 结果

生成未应用的 Scrape revision，FUSE 命名空间尚未变化。

### 相关规格

- [Scrape 与 Agent 工作流](scrape.md)
- [Projection 模型](projection.md)

## UC-04 回答媒体识别歧义

### 参与者

控制用户、Agent

### 目标

解决 Agent 无法确定作品、季集或文件角色的问题。

### 前置条件

- Scrape 状态为 waiting_input；
- Question 属于当前 revision。

### 触发

```bash
scrapefs scrape answer scrape-42 question-1 candidate-2021
```

### 主流程

1. daemon 校验 Question 属于当前 revision。
2. daemon 保存 Answer。
3. Agent 根据 Answer 重新生成 MediaModel。
4. Profile 重新渲染目标路径。
5. daemon重新执行安全和冲突检查。
6. daemon 创建新 revision。
7. 没有剩余问题时 Scrape 进入 ready。

### 备选流程

- Question 属于旧 revision：拒绝 Answer。
- 仍有未回答问题：Scrape 保持 waiting_input。
- 重新渲染失败：Scrape 返回 draft 并记录错误。

### 结果

产生新的不可变 revision，旧 revision 保留用于 diff。

### 相关规格

- [Scrape 与 Agent 工作流](scrape.md)

## UC-05 修改 Scrape 目标路径

### 参与者

控制用户

### 目标

在应用前修正 Agent 或 Profile 生成的目标路径。

### 前置条件

- Scrape 状态为 draft、waiting_input 或 ready；
- 新路径位于同一 Library。

### 触发

```bash
scrapefs scrape revise scrape-42 \
  --target rendered-01="Movies/Dune (2021)/Dune (2021).mkv"
```

### 主流程

1. daemon读取当前 revision。
2. daemon 应用手动 override。
3. daemon执行不可绕过的路径安全检查。
4. Profile 对命名规范生成 warning。
5. daemon 创建新 revision。
6. CLI 展示与前一 revision 的 diff。

### 备选流程

- 路径包含 `..` 或逃逸 LibraryRoot：拒绝。
- 目标与已有节点冲突：Scrape 不进入 ready。
- Scrape 已 applied、undone 或 discarded：拒绝修改。

### 结果

新 revision 成为当前 revision，应用时必须重新确认。

### 相关规格

- [Scrape 与 Agent 工作流](scrape.md)

## UC-06 应用 Scrape

### 参与者

控制用户

### 目标

将用户刚确认的 Scrape revision 原子发布到 FUSE 命名空间。

### 前置条件

- Scrape 状态为 ready；
- CLI 已展示当前 revision；
- 调用者确认应用；
- Library 为 ACTIVE。

### 触发

```bash
scrapefs scrape apply scrape-42
```

### 主流程

1. CLI 提交 Scrape ID 和 expected revision。
2. daemon 确认 revision 未变化。
3. daemon 重新校验全部选中源文件身份。
4. daemon 重新检查路径安全和冲突。
5. daemon 在一个 SQLite 事务中创建 Directory 和 ProjectionFile。
6. daemon 写 operation log。
7. daemon 将 Scrape 标记为 applied。
8. daemon 增加 namespace version 并提交。
9. daemon 更新内存索引并失效 FUSE 缓存。
10. 目标目录立即可见。

### 备选流程

- revision 已变化：拒绝并要求重新查看。
- 源文件变化：返回 `ESTALE`。
- 目标发生冲突：返回 `EEXIST`。
- 事务失败：不留下部分 Projection。
- Scrape 已 applied：返回 `AlreadyApplied`。

### 结果

全部 Projection 同时发布，源文件保持不变。

### 相关规格

- [Scrape 与 Agent 工作流](scrape.md)
- [系统架构](architecture.md)

## UC-07 撤销 Scrape

### 参与者

控制用户

### 目标

删除某次 Scrape 创建的 Projection，同时保留源文件和 OverlayFile。

### 前置条件

- Scrape 状态为 applied。

### 触发

```bash
scrapefs scrape undo scrape-42
```

### 主流程

1. daemon 禁止该 Scrape 的新 Agent artifact 请求。
2. daemon 在事务中删除 `origin_scrape_id` 匹配的 ProjectionFile。
3. daemon 清理空的 managed Directory。
4. daemon 写 operation log。
5. daemon 将 Scrape 标记为 undone。
6. daemon 增加 namespace version 并提交。
7. CLI 列出仍然保留的 Agent OverlayFile。

### 备选流程

- Projection 已被用户删除：视为已经不存在。
- 目录中仍有 OverlayFile 或其他 Projection：保留目录。
- Scrape 已 undone：返回 `AlreadyUndone`。

### 结果

Scrape 的 Projection 消失，源文件和 OverlayFile 保留。

### 相关规格

- [Scrape 与 Agent 工作流](scrape.md)

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
2. CLI 展示旧、新路径和身份。
3. 用户显式确认。
4. daemon 更新 source path、身份和缓存属性。
5. daemon 更新虚拟 ctime、写 operation log 并失效属性缓存。
6. 新 open 从新源文件读取。

### 备选流程

- 新源文件不存在：返回 `ENOENT`。
- 新路径包含 symlink：拒绝。
- 用户取消：不修改 Projection。

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
```

### 主流程

空 Library：

1. daemon 删除 LibraryRoot。
2. daemon 将 Library 标记为 RETIRED。
3. daemon 写 operation log 并增加 namespace version。

非空 Library 使用显式 recursive remove：

1. 管理员先取消活动 session 并 discard 未应用 Scrape。
2. daemon 在事务中删除 Library 节点树。
3. daemon 将 `root_node_id` 置空并标记 RETIRED。
4. 事务提交后回收 Overlay object。

### 备选流程

- 普通删除非空 Library：返回 `ENOTEMPTY`。
- 存在活动 Scrape 或 Agent session：recursive remove 失败。

### 结果

Library 从 FUSE 根目录消失，源文件保持不变。

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

1. daemon 获取单实例锁。
2. daemon 打开 SQLite 并验证 PRAGMA。
3. daemon 执行 migration。
4. daemon 恢复 object store 状态。
5. daemon 从 SQLite 重建命名空间和 inode 索引。
6. daemon 清理确认属于死亡实例的 stale mount。
7. daemon 挂载 FUSE 并创建控制 socket。
8. daemon 发送 readiness。

### 备选流程

- 无法获得单实例锁：立即退出。
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

- daemon 已 ready，或者 doctor 支持离线只读检查。

### 触发

```bash
scrapefs doctor
```

### 主流程

1. doctor 检查 Projection 源路径和身份。
2. doctor 检查 OverlayFile 是否存在对应 live object。
3. doctor 检查无节点引用的 orphan object。
4. doctor 检查 Library 和节点树约束。
5. doctor 输出结构化问题列表和建议操作。

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
