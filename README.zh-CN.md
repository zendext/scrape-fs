# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

ScrapeFS 是一个面向 Linux 的外部 Agent 辅助媒体整理与虚拟目录文件系统。

它将一个或多个本地文件系统中的媒体文件投影为适合播放器识别的媒体库，同时不移动、不重命名、不修改源文件。

> 当前状态：处于早期实现阶段。daemon 已支持加载并校验配置、获取宿主文件系统运行时锁、初始化 SQLite，以及提供事务化的持久命名空间核心和 Library 创建、查询、列表应用服务；控制 socket 和 FUSE 行为尚未实现。

## 核心模型

- **Library**：通过 FUSE 挂载点提供的虚拟媒体库，例如 `jellyfin` 或 `plex`。
- **Projection**：外部源文件到 Library 虚拟路径之间的只读映射。
- **OverlayFile**：由 ScrapeFS 管理的可写文件，例如 NFO、海报和下载的字幕。
- **Scrape**：由外部工具提交、经 ScrapeFS 校验并可原子应用的 Projection Plan。
- **Profile**：面向特定播放器的确定性目录和命名规则。

`Node` 仅是虚拟文件系统目录项的内部实现术语，不属于用户 API。

## 示例

给定：

```text
/mnt/disk1/downloads/Dune.2021.REMUX/
├── Dune.2021.REMUX.mkv
├── Dune.zh.ass
└── sample.mkv
```

ScrapeFS 可以提供：

```text
/mnt/scrapefs/jellyfin/
└── Movies/
    └── Dune (2021) [tmdbid-438631]/
        ├── Dune (2021).mkv
        ├── Dune (2021).zh.ass
        ├── movie.nfo
        └── poster.jpg
```

视频和源字幕是只读 Projection，NFO 和海报是由 ScrapeFS 保存的可写 OverlayFile。

## 直接创建 Projection

源路径和目标路径已经明确时：

```bash
scrapefs library add jellyfin --profile jellyfin@1

scrapefs projection add \
  /mnt/disk1/downloads/Dune.mkv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv"
```

删除或重命名 Projection 不会修改源文件。

## 外部 Agent Skill

需要识别媒体时，外部 Agent Skill 读取源目录、执行媒体探测和元数据查询，然后提交显式 Plan：

```bash
scrapefs scrape submit --file /tmp/dune.scrape.json
scrapefs scrape diff scrape-42 --against namespace
scrapefs scrape apply scrape-42 --revision 1
```

`scrapefsd` 不运行模型，也不向 Agent 暴露文件读取、`ffprobe` 或网络工具。它只安全打开 Plan 中显式列出的源文件，执行路径和冲突校验，并原子创建 Projection。NFO 和海报由外部 Agent 在 apply 后通过普通 FUSE 文件操作创建。

早期规范中的 `scrapefs scrape <source> --library <name>` 假设 daemon 会主动枚举和识别媒体。该命令从目标 v1 CLI 中移除；`Scrape` 现在表示外部生成 Plan 的审阅、revision、apply 和 undo 生命周期。

## 第一版边界

第一版面向：

- Linux 和 FUSE；
- Plex 和 Jellyfin Library；
- 电影、标准 `SxxEyy` 剧集和外挂字幕；
- 只读源文件 Projection；
- 可写 OverlayFile；
- 版本化 JSON Scrape Plan；
- 面向人工和外部 Agent Skill 的机器可读 CLI。

第一版不包含内置模型 provider、daemon-side Agent session、torrent 管理、下载器集成、GUI、可写 Projection、完整 POSIX 文件系统、动态 Profile 插件或跨主机路径转换。

## 开发

ScrapeFS 当前要求 Linux 和 Go 1.25 或更高版本。

构建两个命令包：

```bash
go build ./cmd/...
```

运行当前入口：

```bash
go run ./cmd/scrapefs --help
go run ./cmd/scrapefsd --version
```

执行基础检查：

```bash
go test ./...
go vet ./...
gofmt -w .
```

## 文档

- [第一版用例](docs/zh-CN/use-cases.md)
- [系统架构](docs/zh-CN/architecture.md)
- [Projection 模型](docs/zh-CN/projection.md)
- [Scrape Plan 与外部 Agent 工作流](docs/zh-CN/scrape.md)
- [文件系统与对象存储](docs/zh-CN/filesystem.md)
- [运行与 CLI](docs/zh-CN/operations.md)
- [第一版验收标准](docs/zh-CN/acceptance.md)
- [English documentation](docs/en/README.md)

中文文档是当前规范原文，英文文档是同步维护的翻译。
