# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

ScrapeFS 是一个面向 Linux 的外部 Agent 辅助媒体整理与虚拟目录文件系统。

它将一个或多个本地文件系统中的媒体文件投影为适合播放器识别的媒体库，同时不移动、不重命名、不修改源文件。

> 当前状态：v1 目标功能已实现，项目处于预发布阶段，尚未发布稳定版本。daemon 已支持加载并校验配置、获取宿主文件系统运行时锁、初始化 SQLite 和 Overlay object store、重建内存索引、挂载持久化 namespace，并通过 Unix 控制 socket 提供经过授权的 Library、direct Projection、Projection Batch、status 和只读 doctor 操作。readiness 只会在 index、mount 和 socket 可用后发送；已提交的 namespace 变更会触发 FUSE cache invalidation，正常停止会排空在途写入后再卸载。FUSE client 可以创建和修改普通 Directory 及 object-backed OverlayFile，通过经过身份校验的 source descriptor 读取 ProjectionFile，并通过共享 Projection lifecycle 操作 rename 或 unlink direct 和 Batch-owned ProjectionFile。仓库同时提供第一版可安装外部 Agent Skill，用于媒体枚举、探测、命名、Batch 确认与 FUSE artifact 工作流。

## 核心模型

- **Library**：通过 FUSE 挂载点提供的虚拟媒体库，例如 `jellyfin` 或 `plex`。
- **Projection**：外部源文件到 Library 虚拟路径之间的只读映射。
- **Projection Batch**：一次原子创建和撤销多个 Projection 的外部 JSON 输入。
- **OverlayFile**：由 ScrapeFS 管理的可写文件，例如 NFO、海报和下载的字幕。
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
        ├── Dune (2021) [tmdbid-438631].mkv
        ├── Dune (2021) [tmdbid-438631].zh.ass
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

scrapefs projection list --library jellyfin

scrapefs projection mv \
  "jellyfin:/Movies/Dune (2021)/Dune (2021).mkv" \
  "jellyfin:/Films/Dune (2021).mkv"
```

`projection add` 安全打开源文件并原子创建映射；`projection list` 从持久化 namespace 返回稳定 ID、虚拟目标和缓存的源文件身份，不重新打开源文件。`projection mv`、`projection rm` 和已确认的 `projection rebind` 在适用时保留稳定 node ID，并且不会修改任一源文件。

## 外部 Agent Skill

需要识别媒体时，安装仓库中的
[`organize-scrapefs-media`](skills/organize-scrapefs-media/SKILL.md) Skill。
它以调用用户权限读取源目录、执行 `ffprobe`、读取 NFO，并生成显式 Batch：

```bash
SKILL_DIR="${CODEX_HOME:-$HOME/.codex}/skills/organize-scrapefs-media"

python3 "$SKILL_DIR/scripts/scrapefs_media.py" plan \
  --source /mnt/disk1/downloads/Dune.2021.REMUX \
  --library jellyfin \
  --profile jellyfin \
  --batch /tmp/dune.batch.json \
  --artifacts /tmp/dune.artifacts.json

python3 "$SKILL_DIR/scripts/scrapefs_media.py" validate \
  --batch /tmp/dune.batch.json
```

`scrapefsd` 不运行模型，也不向 Agent 暴露文件读取、`ffprobe` 或网络工具。Validate 无状态检查 Batch；用户必须原样确认 validate 返回的完整 digest，Skill 才会 fresh validate 并 apply。NFO 和海报在 apply 后由 Skill 通过普通 FUSE 文件操作创建。完整安装、确认和失败恢复流程见 [Projection Batch 与外部 Agent 工作流](docs/zh-CN/scrape.md)。

早期规范中的 `scrapefs scrape <source> --library <name>` 假设 daemon 会主动枚举和识别媒体。该命令从目标 v1 CLI 中移除。ScrapeFS 也不持久化未应用 Plan、draft 或 revision；外部 Agent 保存工作上下文，daemon 只保存成功 apply 后用于审计和 undo 的最小 Batch provenance。

## 第一版边界

第一版面向：

- Linux 和 FUSE；
- Plex 和 Jellyfin Library；
- 电影、标准 `SxxEyy` 剧集和外挂字幕；
- 只读源文件 Projection；
- 可写 OverlayFile；
- 版本化 JSON Projection Batch；
- 面向人工和外部 Agent Skill 的机器可读 CLI。

第一版不包含内置模型 provider、daemon-side Agent session、torrent 管理、下载器集成、GUI、可写 Projection、完整 POSIX 文件系统、动态 Profile 插件或跨主机路径转换。

## 安装

推荐从 [GitHub Releases](https://github.com/zendext/scrape-fs/releases) 下载
`linux/amd64` 归档和 `SHA256SUMS`：

```bash
version=vX.Y.Z
archive="scrape-fs_${version}_linux_amd64.tar.gz"

sha256sum -c SHA256SUMS
tar -xzf "$archive"
cd "${archive%.tar.gz}"
sudo ./install.sh --administrator "$USER"
```

也可以从源码手动构建。两条路径使用同一个安装脚本；脚本不会覆盖已有配置，也
不会自动启动服务。完整步骤见[部署指南](docs/zh-CN/deployment.md)。

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
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
sh -n \
  scripts/install.sh \
  scripts/package-release.sh \
  scripts/release-version.sh
python3 -m unittest discover \
  -s scripts/tests \
  -p 'test_*.py'
python3 -m unittest discover \
  -s skills/organize-scrapefs-media/tests \
  -p 'test_*.py'
```

`go test ./...` 包含真实 FUSE mount 和跨 UID/GID ACL 集成测试。FUSE 测试要求
`/dev/fuse` 以及 root 或有效的 `CAP_SYS_ADMIN`；ACL 跨角色连接测试要求 root
或 root-mapped user namespace。环境不满足时，对应测试会明确标记为 skipped。

## 文档

- [第一版用例](docs/zh-CN/use-cases.md)
- [系统架构](docs/zh-CN/architecture.md)
- [Projection 模型](docs/zh-CN/projection.md)
- [Projection Batch 与外部 Agent 工作流](docs/zh-CN/scrape.md)
- [文件系统与对象存储](docs/zh-CN/filesystem.md)
- [部署指南](docs/zh-CN/deployment.md)
- [运行与 CLI](docs/zh-CN/operations.md)
- [第一版验收标准](docs/zh-CN/acceptance.md)
- [English documentation](docs/en/README.md)

中文文档是当前规范原文，英文文档是同步维护的翻译。
