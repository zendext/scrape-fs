# Scrape Plan 与外部 Agent 工作流

[English](../en/scrape.md) | **简体中文**

Scrape 是由外部工具提交、经 ScrapeFS 校验并可原子应用的一组 Projection 计划。

媒体识别不在 `scrapefsd` 中执行。外部 Agent Skill、脚本或人工工具负责读取源目录、调用 `ffprobe`、查询元数据、处理歧义并选择目标路径。ScrapeFS 只负责确定性的源文件校验、路径校验、冲突检查、预览、apply 和 undo。

## 与早期规范的差异

早期规范计划使用：

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

该命令意味着 daemon 枚举源目录、运行 Agent 分析并生成 Projection。这个行为尚未实现，并从目标 v1 CLI 契约中移除，不提供兼容 alias。

当前职责拆分为：

1. 外部 Agent 或其他工具执行媒体刮削并生成 `scrapefs.scrape/v1` Plan。
2. `scrapefs scrape submit` 将 Plan 提交给 daemon。
3. daemon 负责审阅数据、revision、validate、原子 apply 和 undo。

因此，当前文档中的 Scrape 指 Plan 生命周期，不再指 daemon 内部执行的媒体识别过程。

## 1. 外部 Agent 边界

外部 Agent：

- 使用调用它的控制用户身份读取宿主文件系统；
- 自行决定使用的模型、提示词、元数据 provider 和媒体探测工具；
- 生成显式的 source-to-target Projection Plan；
- 通过 `scrapefs` CLI 提交和审阅 Plan；
- 在 Plan applied 后，通过普通 FUSE 文件操作创建 NFO、海报等 OverlayFile。

ScrapeFS 不提供：

- 内置模型 provider；
- daemon-side Agent session 或 session token；
- 模型工具调用循环；
- 任意文件读取、`ffprobe` 或网络查询 Agent API；
- MediaModel、Question/Answer 或 Agent 专用 Overlay API。

外部 Agent 不是额外的权限边界。它只能使用自身的宿主文件系统权限、控制 CLI 权限和 FUSE 权限。

## 2. Plan 格式

第一版只接受 UTF-8 JSON。CLI 从文件或 stdin 读取版本化 Plan：

```bash
scrapefs scrape submit --file /tmp/dune.scrape.json
scrapefs scrape submit --file -
```

```json
{
  "schema": "scrapefs.scrape/v1",
  "library": "jellyfin",
  "entries": [
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.2021.REMUX.mkv",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021).mkv"
    },
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/Dune.zh.ass",
      "target": "Movies/Dune (2021) [tmdbid-438631]/Dune (2021).zh.ass"
    }
  ],
  "ignored": [
    {
      "source": "/mnt/disk1/downloads/Dune.2021.REMUX/sample.mkv",
      "reason": "sample video"
    }
  ]
}
```

规则：

- `schema` 必须为 `scrapefs.scrape/v1`；
- `library` 必须引用 ACTIVE Library；
- `entries` 必须非空；
- `source` 必须是经过词法规范化的绝对本地路径；
- `target` 相对于 LibraryRoot，必须经过通用和 Profile 路径校验；
- 同一 Plan 不能重复使用 target；
- `ignored` 只用于审阅，不创建节点，也不授予 daemon 读取对应路径的权限；
- 未知字段和无效类型必须拒绝；
- 第一版不支持 YAML 或跨主机路径转换。

## 3. Submit 和源文件快照

`scrape submit` 由控制用户调用。控制用户是可信源路径选择者。

daemon 对每个 entry：

1. 使用安全源文件打开能力打开 `source`；
2. 拒绝 symlink、magic link、目录和其他非 regular 文件；
3. 保存规范化路径、device、inode、size、mode 和纳秒时间戳；
4. 关闭提交阶段不再需要的描述符；
5. 在 apply 前重新安全打开并校验身份。

Plan 不能请求 daemon 枚举父目录、读取文件内容、执行命令或访问网络。

## 4. Scrape 数据

```text
scrapes
├── id
├── library_id
├── profile_name
├── profile_version
├── current_revision
├── applied_revision
├── status
├── validated_namespace_version
├── confirmed_by_uid
├── confirmed_by_gid
├── confirmed_at
├── last_error
├── created_at
└── updated_at
```

状态：

```text
draft
ready
applied
undone
discarded
```

Schema 错误或权限错误导致 submit 失败且不创建 Scrape。可以审阅和修正的路径冲突、Profile warning 或源文件状态保存在 draft revision 中。没有 blocking error 时状态为 ready。

## 5. Revision

每次 `submit` 创建 revision 1。`scrape revise` 使用完整替换 Plan 创建新的不可变 revision：

```bash
scrapefs scrape revise scrape-42 --file corrected.json
```

```text
scrape_revisions
├── scrape_id
├── revision
├── plan
├── source_snapshots
├── validation_result
├── warnings
├── created_by_uid
└── created_at
```

CLI 展示 revision 后，apply 必须携带 expected revision。当前 revision 已变化时，daemon 拒绝 apply 并要求重新审阅。

状态转换：

```text
无 -> draft
无 -> ready
draft -> ready
draft/ready -> draft
draft/ready -> discarded
ready -> applied
applied -> undone
```

applied、undone 或 discarded Scrape 不能 revise。

## 6. Validate、Diff 和 Export

```text
scrapefs scrape show scrape-42
scrapefs scrape validate scrape-42
scrapefs scrape diff scrape-42
scrapefs scrape diff scrape-42 --against namespace
scrapefs scrape export scrape-42
```

`validate` 重新检查 Library、Profile、源文件身份、路径安全和命名空间冲突，并使用相同 Plan 创建包含新 source snapshot 和 validation result 的不可变 revision。它不修改已有 revision。

`diff --against namespace` 显示：

- 将创建的 Projection；
- 将创建或复用的 managed Directory；
- 已存在路径；
- 路径冲突；
- Profile warning；
- ignored source。

`export` 只输出可再次提交的当前 `scrapefs.scrape/v1` Plan。daemon 保存的 source snapshot、validation result 和 namespace diff 通过 `show --json` 和 `diff --json` 查看，不能作为再次提交时绕过校验的依据。

所有面向自动化的命令必须支持机器可读 JSON 输出。非 TTY 或显式机器可读模式不得提示输入或自动 apply。

## 7. Apply

apply 前必须确认：

- 调用者有控制权限；
- Scrape 状态为 ready；
- expected revision 等于当前 revision；
- 所有源文件身份仍匹配；
- Library 为 ACTIVE；
- Profile 版本可用；
- 所有路径安全检查通过；
- 目标路径不存在冲突。

原子流程：

1. 读取 ready Scrape 和 expected revision。
2. 重新安全打开并校验全部源文件。
3. 重新执行通用路径和 Profile 校验。
4. 构建完整候选 namespace 变更。
5. 开启 SQLite 写事务。
6. 再次检查路径冲突。
7. 创建全部 Directory 和 ProjectionFile。
8. 写一条批次 operation log。
9. Scrape 状态改为 applied。
10. 保存 applied revision 和确认者 UID/GID。
11. namespace version 增加 1。
12. 提交事务。
13. 更新内存索引并失效 FUSE 缓存。

步骤 5 至 12 必须处于同一事务。Plan 实现不得通过重复调用各自提交事务的 direct Projection 操作来模拟原子批次。

重复 apply 已 applied Scrape 返回 `AlreadyApplied`。

## 8. Undo

undo 在单个 SQLite 事务中：

1. 删除 `origin_scrape_id` 匹配的 ProjectionFile；
2. 向 LibraryRoot 清理空的 managed Directory；
3. 写 operation log；
4. Scrape 状态改为 undone；
5. namespace version 增加 1。

undo 不删除通过 FUSE 创建的 OverlayFile。CLI 必须列出阻止目录清理或仍位于原目标目录中的 OverlayFile。

重复 undo 已 undone Scrape 返回 `AlreadyUndone`。

## 9. Overlay Artifact

Plan applied 后，外部 Agent 可以通过普通 FUSE 操作创建 artifact：

```bash
printf '%s' "$NFO" > \
  "/mnt/scrapefs/jellyfin/Movies/Dune (2021) [tmdbid-438631]/movie.nfo"

cp poster.jpg \
  "/mnt/scrapefs/jellyfin/Movies/Dune (2021) [tmdbid-438631]/poster.jpg"
```

这些文件遵循普通 FUSE create、write、rename 和权限语义：

- 虚拟 UID/GID 来自 FUSE 调用者；
- mode 经过普通 umask 和 Library 权限规则处理；
- backing object 使用 daemon 私有权限；
- Scrape undo 不自动删除 OverlayFile；
- ScrapeFS 不区分文件由人工、脚本或 Agent 创建。

## 10. Agent Skill 契约

外部 Agent Skill 应当说明：

- 支持的媒体、字幕和本地元数据格式；
- 源目录枚举和 sample 忽略规则；
- `ffprobe` 和元数据查询方式；
- Plex/Jellyfin Profile 命名规则；
- `scrapefs.scrape/v1` 生成规则；
- submit、show、diff、revise、validate、apply 和 undo 工作流；
- 用户确认边界；
- apply 后通过 FUSE 创建 OverlayFile 的方式；
- `ESTALE`、路径冲突和部分 artifact 失败的恢复方式。

Skill 不是 daemon 插件。Skill 更新不能改变 ScrapeFS 的权限模型、Plan Schema 或路径安全规则。
