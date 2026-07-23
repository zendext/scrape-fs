# Projection Batch 与外部 Agent 工作流

[English](../en/scrape.md) | **简体中文**

媒体识别不在 `scrapefsd` 中执行。外部 Agent Skill、脚本或人工工具负责读取源目录、调用 `ffprobe`、查询元数据、处理歧义并生成显式的 Projection Batch。ScrapeFS 只负责确定性的校验、预览、原子 apply 和 batch undo。

Batch JSON 是外部输入文档，不是 daemon-side Plan。ScrapeFS 不持久化未应用 Batch，不维护 draft、ready、revision、revise 或 discard 生命周期。

## 1. 与早期规范的差异

早期规范计划使用：

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

该命令意味着 daemon 枚举源目录、运行 Agent 分析并生成 Projection。这个行为尚未实现，并从目标 v1 CLI 契约中移除，不提供兼容 alias。

后续的外部 Agent 设计曾考虑由 daemon 持久化 `scrapefs.scrape/v1` Plan、revision 和待确认状态。外部 Agent 已经拥有工作上下文并负责用户交互，因此这层状态重复且不再属于 v1。

当前职责拆分为：

1. 外部 Agent 或其他工具执行媒体刮削并生成 `scrapefs.projection-batch/v1` JSON。
2. `projection batch validate` 无状态校验并预览 Batch。
3. `projection batch apply` 重新校验并原子创建全部 Projection。
4. apply 成功后，daemon 只保存用于审计和 undo 的最小 Batch provenance。

## 2. 外部 Agent 边界

外部 Agent：

- 使用调用它的控制用户身份读取宿主文件系统；
- 自行决定使用的模型、提示词、metadata provider 和媒体探测工具；
- 自行处理 ignored source、候选比较、用户确认和 Batch 文件保存；
- 通过 `scrapefs` CLI validate 和 apply Batch；
- apply 后通过普通 FUSE 文件操作创建 NFO、海报等 OverlayFile。

ScrapeFS 不提供：

- 内置模型 provider；
- daemon-side Agent session 或 session token；
- 模型工具调用循环；
- 任意文件读取、`ffprobe` 或网络查询 Agent API；
- MediaModel、Question/Answer、pending Plan 或 Agent 专用 Overlay API。

外部 Agent 不是额外的权限边界。它只能使用自身的宿主文件系统权限、控制 CLI 权限和 FUSE 权限。

## 3. Batch 格式

第一版只接受严格的 UTF-8 JSON。CLI 从文件或 stdin 读取：

```bash
scrapefs projection batch validate --file /tmp/dune.batch.json
scrapefs projection batch validate --file -
```

```json
{
  "schema": "scrapefs.projection-batch/v1",
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
  ]
}
```

规则：

- `schema` 必须为 `scrapefs.projection-batch/v1`；
- `library` 必须引用 ACTIVE Library；
- `entries` 必须非空；
- `source` 必须是经过词法规范化的绝对本地路径；
- `target` 相对于 LibraryRoot，必须通过通用和 Profile 路径校验；
- 同一 Batch 不能重复使用 source 或 target；
- 未知字段和无效类型必须拒绝；
- 第一版不支持 YAML、远程 source 或跨主机路径转换。

媒体标题、provider ID、ignored source、置信度和 Agent 推理不属于 Batch Schema。外部 Agent 可以在自己的工作上下文中保留这些信息。

## 4. 无状态 Validate

```bash
scrapefs projection batch validate \
  --file /tmp/dune.batch.json \
  --json
```

daemon：

1. 严格解析 Batch；
2. 解析 ACTIVE Library 和当前 Profile；
3. 使用安全源文件打开能力检查每个 source；
4. 拒绝 symlink、magic link、目录和其他非 regular 文件；
5. 执行通用路径和 Profile 校验；
6. 检查 Batch 内部及当前 namespace 冲突；
7. 计算规范化 Batch 的 SHA-256 digest；
8. 返回 diff、warning、digest 和当前 namespace version；
9. 关闭所有 source fd，不写 SQLite。

示例结果：

```json
{
  "schema": "scrapefs.projection-batch-validation/v1",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "namespace_version": 42,
  "valid": true,
  "entries": [],
  "warnings": []
}
```

digest 根据严格解析后的规范化 Batch 计算，格式为 `sha256:` 后跟 64 位小写十六进制字符。输入 JSON 的空白和对象字段顺序不影响 digest，`entries` 数组顺序保留。`namespace_version` 只用于审阅；apply 时版本变化会触发重新检查，不能仅因版本不同而失败。

Validate 不创建 pending Batch、source snapshot、revision 或 operation log。外部 Agent 持有原始 Batch JSON，并负责修改后重新 validate。

## 5. 原子 Apply

```bash
scrapefs projection batch apply \
  --file /tmp/dune.batch.json \
  --expected-digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

apply 前必须确认：

- 调用者有控制权限；
- expected digest 与提交 Batch 的规范化 digest 相同；
- 所有 source 仍可安全打开且为 regular 文件；
- Library 为 ACTIVE；
- Profile 版本可用；
- 所有路径安全检查通过；
- Batch 内部和 namespace 中没有 target 冲突。

原子流程：

1. 重新解析 Batch 并校验 expected digest。
2. 重新安全打开全部 source，并保存 apply-time identity。
3. 重新执行通用路径、Profile 和冲突检查。
4. 构建完整候选 namespace 变更。
5. 开启 SQLite 写事务。
6. 再次检查目标冲突。
7. 创建最小 applied Batch provenance 记录。
8. 创建全部 managed Directory 和 ProjectionFile。
9. 写一条批次 operation log。
10. namespace version 增加 1。
11. 提交事务。
12. 更新内存索引并失效 FUSE 缓存。

步骤 5 至 11 必须处于同一事务。实现不得通过重复调用各自提交事务的 `projection add` 来模拟原子批次。

成功结果返回稳定 Batch ID：

```json
{
  "schema": "scrapefs.projection-batch-result/v1",
  "batch_id": "batch-42",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "namespace_version": 43
}
```

同一 Batch 可以在目标尚未存在时再次 apply；digest 不是幂等键。已存在的目标必须返回冲突，不得静默视为成功。

## 6. Applied Batch Provenance

只有成功 apply 的 Batch 才持久化：

```text
projection batch provenance
├── id
├── library_id
├── profile_name
├── profile_version
├── digest
├── status
├── applied_by_uid
├── applied_by_gid
├── applied_at
├── undone_by_uid
├── undone_by_gid
└── undone_at
```

状态：

```text
applied
undone
```

基础 namespace migration 已发布 `scrapes`、`nodes.origin_scrape_id` 和 `operation_log.scrape_id` 名称。schema migration 3 扩展这些 legacy storage 名称保存 Batch provenance，避免重写已发布 migration。它们是内部存储名称，不属于 CLI 或 JSON Schema。

Batch 不保存外部 Agent 的 prompt、对话、metadata response 或未应用 JSON。`projection_batch_entries` 保存有序的 source identity 和 target 历史，使 `show` 在 undo 后仍能完整展示；operation log 保存 apply 和 undo 的路径快照。

## 7. Show、List 和 Undo

```text
scrapefs projection batch list
scrapefs projection batch show batch-42
scrapefs projection batch undo batch-42
```

`show` 只展示已 applied 或 undone Batch 的 provenance、创建的 Projection 和 operation-log 快照，不提供待处理 Plan。

undo 在单个 SQLite 事务中：

1. 删除 legacy `origin_scrape_id` 指向该 Batch 的 ProjectionFile；
2. 向 LibraryRoot 清理空的 managed Directory；
3. 写 operation log；
4. 将 Batch 状态改为 undone，并保存调用者 UID/GID；
5. namespace version 增加 1。

undo 不删除通过 FUSE 创建的 OverlayFile。CLI 必须列出阻止目录清理或仍位于原目标目录中的 OverlayFile。

重复 undo 已 undone Batch 返回 `AlreadyUndone`。

## 8. 与 Direct Projection 的关系

Issue #8 实现一次创建一个 Projection 的 direct operation。Direct 和 Batch 必须共享：

- source 安全打开与身份提取；
- Library 解析；
- 通用和 Profile 路径校验；
- 冲突分类；
- 候选 Projection 构建。

Direct operation 自己提交单 Projection 事务。Batch operation 持有一个调用者事务并创建全部 entry，不调用 independently committing direct operation。

## 9. Overlay Artifact

Batch apply 后，外部 Agent 可以通过普通 FUSE 操作创建 artifact：

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
- batch undo 不自动删除 OverlayFile；
- ScrapeFS 不区分文件由人工、脚本或 Agent 创建。

## 10. Agent Skill 契约

外部 Agent Skill 应当说明：

- 支持的媒体、字幕和本地 metadata 格式；
- 源目录枚举和 sample 忽略规则；
- `ffprobe` 和 metadata 查询方式；
- Plex/Jellyfin Profile 命名规则；
- `scrapefs.projection-batch/v1` 生成规则；
- validate、用户确认、apply、show 和 undo 工作流；
- apply 后通过 FUSE 创建 OverlayFile 的方式；
- source 变化、路径冲突和部分 artifact 失败的恢复方式。

Skill 不是 daemon 插件。Skill 更新不能改变 ScrapeFS 的权限模型、Batch Schema 或路径安全规则。
