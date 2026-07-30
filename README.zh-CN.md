# ScrapeFS

[English](README.md) | [简体中文](README.zh-CN.md)

> 本项目由 AI 编写，使用本项目所产生的风险由使用者自行承担。

ScrapeFS 是一个面向 Linux 的外部 Agent 辅助媒体整理与虚拟目录文件系统。

它将一个或多个本地文件系统中的媒体文件投影为适合播放器识别的媒体库，同时不移动、不重命名、不修改源文件。

## 典型场景

- 保持媒体文件位于 BT 下载器管理的原始路径中持续做种，同时为 Jellyfin 或 Plex
  提供具有规范目录和文件名的虚拟媒体库。
- 将多个磁盘或下载目录中的媒体聚合到同一个 Library，而不复制文件或改变原有
  目录结构。
- 由外部 Agent 识别媒体并生成 Projection Batch，再通过 FUSE 创建 NFO、海报和
  字幕等附属文件。

## 安装与设置

ScrapeFS 要求 Linux、systemd、`/dev/fuse`、util-linux 提供的 `findmnt`，
以及用于安装文件、创建组和挂载 FUSE 的 root 权限。

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

安装脚本不会覆盖已有配置，也不会自动启动服务。首次启动前检查配置，然后启动
daemon、验证运行状态并创建第一个 Library：

```bash
sudo cat /etc/scrapefs/config.toml
sudo systemctl enable --now scrapefsd.service

scrapefs status
scrapefs doctor
scrapefs library add jellyfin --profile jellyfin@1
scrapefs library list
```

也可以从源码手动构建。完整的安装脚本行为、源码构建、路径、权限和升级步骤见
[部署指南](docs/zh-CN/deployment.md)。

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

`scrapefs scrape <source> --library <name>` 不属于 CLI。ScrapeFS 不持久化未应用
Plan、draft 或 revision；外部 Agent 保存工作上下文，daemon 只保存成功 apply
后用于审计和 undo 的最小 Batch provenance。

## 文档

- [第一版用例](docs/zh-CN/use-cases.md)
- [系统架构](docs/zh-CN/architecture.md)
- [Projection 模型](docs/zh-CN/projection.md)
- [Projection Batch 与外部 Agent 工作流](docs/zh-CN/scrape.md)
- [文件系统与对象存储](docs/zh-CN/filesystem.md)
- [部署指南](docs/zh-CN/deployment.md)
- [运行与 CLI](docs/zh-CN/operations.md)
- [English documentation](docs/en/README.md)

中文文档是当前规范原文，英文文档是同步维护的翻译。
