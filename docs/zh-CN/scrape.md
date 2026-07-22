# Scrape 与 Agent 工作流

[English](../en/scrape.md) | **简体中文**

Scrape 是 Agent 根据用户指定的文件或目录生成批量 Projection 草稿的工作流。

## 1. 创建 Scrape

```bash
scrapefs scrape \
  /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin
```

Scrape 不移动或整理源目录。

输入为普通文件时只检查该文件。输入为目录时，daemon 只枚举该目录及媒体识别所需的后代，禁止默认遍历其他目录或整个文件系统。

## 2. Scrape 数据

```text
scrapes
├── id
├── source_path
├── library_id
├── profile_name
├── profile_version
├── current_revision
├── applied_revision
├── status
├── validated_namespace_version
├── confirmed_by_uid
├── confirmed_at
├── last_error
├── created_at
└── updated_at
```

状态：

```text
draft
waiting_input
ready
applied
undone
discarded
```

## 3. Scrape 文件清单

daemon 枚举成功后保存：

```text
scrape_files
├── id
├── scrape_id
├── source_path
├── relative_path
├── parent_id
├── name
├── file_type
├── device_id
├── inode
├── mode
├── size
├── atime_ns
├── mtime_ns
├── ctime_ns
└── created_at
```

`file_type`：

```text
regular
directory
unsupported
```

规则：

- 文件清单只属于当前 Scrape。
- 文件清单创建完成后不可修改。
- Agent 只能引用当前清单中的 regular 文件。
- 需要重新读取目录时创建新的 Scrape。
- 第一版不建立独立 Source Scan 模型。

## 4. 媒体探测

Agent 通过 `scrape_file_id` 请求 daemon 执行受控的文本读取或 `ffprobe`。

daemon 必须重新安全打开并校验 device、inode、size 和 mtime。

Agent 不得提供任意本地路径、工具参数或 shell 命令。

probe 必须配置超时和最大输出。第一版不维护通用 Probe 历史表。

## 5. Agent Session

```text
agent_sessions
├── id
├── scrape_id
├── library_id
├── status
├── token_hash
├── expires_at
├── created_at
└── updated_at
```

状态：

```text
ANALYSIS
ARTIFACT
CANCELLED
EXPIRED
COMPLETED
```

Agent 允许：

- 读取当前 Scrape 文件清单；
- 请求受控文本读取和 ffprobe；
- 生成 MediaModel；
- 生成结构化 Question；
- 根据 Answer 生成新 revision。

Agent 禁止：

- 读取当前 Scrape 范围外的路径；
- 提交任意绝对路径；
- 修改源文件；
- 直接访问 SQLite、object store 或 FUSE mount；
- apply、undo 或 discard Scrape；
- 管理 Library。

第一版 Agent provider 在 daemon 进程内运行，或由 daemon 以自身有效 UID 启动为受信 worker。独立 worker 调用 Agent API 时仍必须携带当前 session 的短期 token。

第一版不支持使用独立 Unix UID 或 sandbox 隔离的不可信外部 Agent worker。

## 6. MediaModel

```yaml
schema: scrapefs.media/v1

media:
  kind: movie | series
  title: string
  original_title: string | null
  year: integer | null
  provider_ids:
    tmdb: string | null
    tvdb: string | null
    imdb: string | null
  edition: string | null
  version: string | null

items:
  - id: media-item-1
    scrape_file_id: file-01
    role: primary_video | episode | subtitle | extra | metadata_source
    season: integer | null
    episodes: [integer]
    language: string | null
    target_item_ids: [media-item-id]
    subtitle:
      default: boolean
      forced: boolean
      sdh: boolean
    extra_type: trailer | featurette | interview | sample | other | null

ignored:
  - scrape_file_id: file-99
    reason: string
```

约束：

- movie 至少包含一个 primary_video；
- episode 必须包含 season 和至少一个 episode；
- subtitle 必须显式关联目标视频或 episode；
- 所有 scrape_file_id 必须属于当前 Scrape；
- Projection 只能引用 regular 文件。

## 7. Profile 输出

```yaml
schema: scrapefs.rendered-entry/v1

id: rendered-01
scrape_file_id: file-01
media_item_id: media-item-1
kind: projection
target: Movies/Dune (2021)/Dune (2021).mkv
artifact_root: Movies/Dune (2021)
manual_override: false
warnings: []
```

`target` 和 `artifact_root` 相对于 LibraryRoot。

Profile 接口：

```go
type Profile interface {
    Name() string
    Version() string

    Render(MediaModel, ScrapeFileManifest) (RenderResult, error)
    ValidatePathSafety(relativePath string) error
    ValidateConformance(entry RenderedEntry) []Warning
}
```

路径安全错误不可绕过。命名不符合播放器建议时生成 warning，并在最终确认时展示。

## 8. Revision

Agent 初次分析、Answer 后重新分析、用户 revise、手动路径 override 或 Profile 重新渲染时，创建新的不可变 revision。

```text
scrape_revisions
├── scrape_id
├── revision
├── media_model
├── rendered_entries
├── questions
├── answers
├── warnings
├── created_by
└── created_at
```

CLI 展示 revision 后，apply 请求必须携带期望的 revision number。当前 revision 已变化时，daemon 必须拒绝并要求重新确认。

第一版不建立 Approval 表，也不计算 cryptographic revision hash。

## 9. 状态转换

```text
无 -> draft
draft -> waiting_input
draft -> ready
draft/waiting_input/ready -> discarded
waiting_input -> draft
ready -> draft
ready -> applied
applied -> undone
applied/undone -> clone 为新 draft
```

回答全部问题后不得直接进入 ready，必须重新执行 Agent 分析、Profile 渲染、路径安全检查和冲突检查。

discard 未应用 Scrape 时，daemon 必须取消对应 Agent session。

## 10. Diff 和 Export

`scrape diff` 默认比较当前 revision 与前一个 revision。

`scrape diff --against namespace` 显示：

- 将创建的 Projection；
- 将创建或复用的目录；
- 已存在路径；
- 路径冲突；
- 命名 warning。

`scrape export` 使用：

```yaml
schema: scrapefs.scrape/v1
```

导出当前 Scrape、文件清单、MediaModel 和 RenderedEntry，用于诊断和人工审阅。第一版不提供 import。

## 11. Apply

apply 前必须确认：

- 调用者有控制权限；
- Scrape 状态为 ready；
- expected revision 等于当前 revision；
- 所有选中源文件身份仍匹配；
- Library 为 ACTIVE；
- Profile 可用；
- 所有路径安全检查通过；
- 目标路径不存在冲突。

原子流程：

1. 读取 ready Scrape 和 expected revision。
2. 重新校验源文件身份和目标路径。
3. 构建候选 namespace 变更。
4. 开启 SQLite 写事务。
5. 再次检查路径冲突。
6. 创建 Directory 和 ProjectionFile。
7. 写 operation log。
8. Scrape 状态改为 applied。
9. 保存 applied revision 和确认者。
10. namespace version 增加 1。
11. 提交事务。
12. 更新内存索引并失效 FUSE 缓存。

步骤 4 至 11 必须处于同一事务。

重复 apply 已 applied Scrape 返回 `AlreadyApplied`。

## 12. Undo

undo 在单个 SQLite 事务中：

1. 删除 `origin_scrape_id` 匹配的 ProjectionFile。
2. 向 LibraryRoot 清理空的 managed Directory。
3. 写 operation log。
4. Scrape 状态改为 undone。
5. namespace version 增加 1。

undo 不删除 OverlayFile。CLI 必须列出该 Scrape 残留的 Agent artifact。

重复 undo 已 undone Scrape 返回 `AlreadyUndone`。

## 13. Agent Artifact

Scrape applied 后，daemon 将 Profile 输出的 artifact root 解析为节点 ID，并将 session 切换到 ARTIFACT。

```text
scrape_artifact_roots
├── scrape_id
├── revision
├── root_node_id
└── relative_path_snapshot
```

daemon 重启后必须从该表恢复写入范围。

Agent 只能在允许的 root 下创建文件，禁止覆盖已有节点。

第一版 API：

```text
overlay.put_text
overlay.put_stream
overlay.stat
```

Scrape undo、session cancel/expire 或 Library retire 必须立即禁止新的 artifact 请求，但不删除已提交 OverlayFile。

推荐配额：

```text
max_text_file_size     = 4 MiB
max_binary_file_size   = 32 MiB
max_session_total_size = 256 MiB
max_upload_duration    = 120s
```
