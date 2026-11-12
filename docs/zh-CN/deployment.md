# 部署指南

[English](../en/deployment.md) | **简体中文**

本指南说明如何从 GitHub Release 或源码安装 ScrapeFS、初始化标准主机目录和
权限，并通过 systemd 运行 daemon。配置字段和运行时约束的完整契约见
[运行与 CLI](operations.md)。

## 1. 前置条件

- Linux 和 systemd；
- `/dev/fuse`；
- util-linux 提供的 `findmnt`；
- root 权限，用于安装文件、创建组和挂载 FUSE；
- 一个现有的普通用户，作为 ScrapeFS 管理员。

手动源码构建还需要 Go 1.25 或更高版本。

安装脚本使用固定的第一版路径：

```text
binaries:     /usr/local/bin/scrapefs
              /usr/local/bin/scrapefsd
configuration:/etc/scrapefs/config.toml
state:        /var/lib/scrapefs
mountpoint:   /mnt/scrapefs
service:      /etc/systemd/system/scrapefsd.service
```

## 2. 从 GitHub Release 安装

版本化 tag 会自动构建并发布三个文件：

- `scrape-fs_<version>_linux_amd64.tar.gz`：CLI、daemon 和核心安装器。
- `scrape-fs-desktop_<version>_linux_amd64.tar.gz`：可选 GUI、内部助手和桌面安装器。
- `SHA256SUMS`：同时覆盖两个归档的校验文件。

从 [GitHub Releases](https://github.com/zendext/scrape-fs/releases) 下载同一版本的
核心归档和校验文件，然后验证、解压并运行归档内的安装脚本。需要 GUI 时另下载
桌面归档，安装方法见[桌面管理器](desktop.md)。仅下载一个归档时，校验会跳过未下载的另一个：

```bash
version=vX.Y.Z
archive="scrape-fs_${version}_linux_amd64.tar.gz"

sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "$archive"
cd "${archive%.tar.gz}"

sudo ./install.sh --administrator "$USER"
```

归档中的 `scrapefs` 和 `scrapefsd` 会报告对应 tag 版本。安装脚本、两个二进制和
通用 systemd unit 位于同一目录，不需要仓库 checkout 或 Go toolchain。
tag 中含有 `-` 的版本会发布为 GitHub prerelease，并明确排除在 Latest 之外。

## 3. 从源码构建

在仓库根目录构建两个命令：

```bash
mkdir -p build

go build -trimpath -o build/scrapefs ./cmd/scrapefs
go build -trimpath -o build/scrapefsd ./cmd/scrapefsd
```

手动构建的二进制默认报告 `dev` 版本。

指定现有管理员用户，并将构建目录传给仓库安装脚本：

```bash
sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build
```

## 4. 安装脚本行为

脚本执行以下操作：

- 安装或更新两个二进制；
- 创建缺失的 `scrapefs` 和 `scrapefs-control` 组；
- 将指定管理员加入 `scrapefs-control`；
- 创建 `root:scrapefs`、mode `0710` 的 fail-closed mountpoint；
- 创建 root-owned、mode `0700` 的状态目录；
- 仅在配置不存在时创建默认配置；
- 安装通用 `Type=notify` systemd unit；
- 执行 `systemctl daemon-reload`。

脚本不启动或启用服务，也不覆盖已有配置。mountpoint 已挂载时，脚本会在修改任何
安装目标前失败；配置目录、状态目录和 mountpoint 已存在时必须是真实目录，符号
链接和其他文件类型也会在任何修改前被拒绝。更新运行中的实例时必须先停止服务。
新增 supplementary group 成员关系通常需要管理员重新登录才能出现在新进程中；
`control.administrator_uid` 指定的用户仍通过控制 socket ACL 获得管理员访问。

安装脚本支持标准 `DESTDIR` 暂存模式。该模式只创建文件布局，不修改用户、组或
systemd 状态，主要用于打包和自动化验证。

## 5. 检查配置

首次启动前检查：

```bash
sudo cat /etc/scrapefs/config.toml
```

默认配置使用 `/mnt/scrapefs`、`/var/lib/scrapefs` 和
`/run/scrapefs/scrapefs.sock`。如果修改路径，必须保持数据库、object store、
mountpoint 和 socket 两两分离，并使用词法规范化的绝对路径。

## 6. 启动服务

确认配置后启用并启动：

```bash
sudo systemctl enable --now scrapefsd.service
```

通用 unit 不包含主机专属的源文件系统、网络存储或其他服务依赖。需要额外排序或
启动条件时，管理员应使用本机 systemd drop-in，而不是修改仓库提供的 unit。

daemon 默认以 root 运行，因为当前 descriptor-anchored、host-visible FUSE mount
要求 root 或有效的 `CAP_SYS_ADMIN`。只有可信用户可以加入 `scrapefs-control`；
这些用户可以选择 daemon 打开的 source path。

## 7. 验证

检查 systemd、FUSE 和 ScrapeFS 自检：

```bash
systemctl status scrapefsd.service
findmnt /mnt/scrapefs
scrapefs status
scrapefs doctor
```

健康运行时：

- service 为 `active (running)`；
- mount 类型为 `fuse.scrapefs`；
- `status.state` 为 `ready`；
- `doctor.severity` 为 `healthy`。

然后创建第一个 Library：

```bash
scrapefs library add jellyfin --profile jellyfin@1
scrapefs library list
```

## 8. 更新

下载并解压新的 Release，或重新构建源码。停止服务后再次运行对应安装脚本；已有
配置会被保留，项目管理的二进制和通用 unit 会被更新：

```bash
sudo systemctl stop scrapefsd.service

# 在 Release 解压目录中：
sudo ./install.sh --administrator "$USER"

# 或在源码仓库中：
sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build

sudo systemctl start scrapefsd.service
scrapefs doctor
```
