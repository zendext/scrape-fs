---
id: desktop
type: module-spec
status: draft
---

# 桌面管理器

[English](../en/desktop.md) | **简体中文**

ScrapeFS GUI 使用 Wails 3 和 SolidJS，管理同一台 Linux 电脑上已经安装的后台。
关闭窗口不会停止后台。GUI 不安装、自动升级后台，也不提供外部 Agent 的批次操作。

## 构建与安装

后台安装见[部署指南](deployment.md)。[GitHub Releases](https://github.com/zendext/scrape-fs/releases)
分别提供核心包和可选桌面包，核心包不包含 GUI。先安装核心包，再安装桌面包：

```bash
version=v0.3.0
archive="scrape-fs-desktop_${version}_linux_amd64.tar.gz"
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "$archive"
sudo "./${archive%.tar.gz}/scripts/install-desktop.sh"
scrapefs-gui
```

预编译的 v0.3.0 桌面包需要 glibc 2.38+、GTK4 和 WebKitGTK 6.0。
也可以独立从源码构建桌面组件。
构建需要 Go 1.26+、Node.js 22.12+、npm、C 编译器、pkg-config，以及 GTK4、
WebKitGTK 6.0 的开发文件。运行需要对应的动态库、systemd、Polkit 的 `pkexec`
和桌面会话中的授权代理；打开文件使用系统默认应用。

```bash
./scripts/build-desktop.sh
sudo ./scripts/install-desktop.sh
scrapefs-gui
```

桌面安装器仅安装 `/usr/local/bin/scrapefs-gui`、
`/usr/local/libexec/scrapefs-admin` 和应用菜单入口。它要求后台已经安装，不修改
后台配置、组、媒体库或服务状态。`DESTDIR=/absolute/staging/path` 可只暂存文件。

GUI 使用普通用户身份运行。日常控制权限沿用[运行与 CLI](operations.md)：
控制用户可以浏览并管理映射，库和目录管理需要 `control.administrator_uid`
配置的管理员身份。系统服务和系统配置操作另外请求 Linux 管理员授权。

```bash
scrapefs-gui --socket /absolute/path/control.sock --config /absolute/path/config.toml
```

自定义 socket 或配置路径只用于本机控制连接，禁用系统服务操作和配置写入。
GUI 从不直接打开数据库或对象存储；文件通过正在运行的 FUSE 挂载打开。

## 目录与映射

左侧为常驻、可调整宽度的目录树，右侧列出当前目录全部目录、映射和库内文件。
列表连续滚动，表头固定；长名称单行省略并保留完整提示。选中条目即在列表下方显示详情，
关闭详情会清除选中状态。树节点和列表条目的悬停效果覆盖整行。
界面默认采用设计稿的深色配色，不修改系统主题。

新建、管理和确认操作使用同一窗口内的模态弹窗；弹窗打开时背景不可操作，关闭后焦点
返回原来的控件。处理操作期间不能关闭弹窗。语言切换只放在主窗口顶部，弹窗中不重复
提供。语言和命名规则使用同一套深色选择器，支持方向键、Home／End、输入首字母定位、
Enter／Space 确认、Escape 取消和 Tab 离开；展开菜单中的已选项与键盘当前项分别标记。

地址栏接受 `library:/relative/path`，`Ctrl+L` 聚焦并全选，Enter 导航，Esc 恢复当前地址。
没有面包屑、上一级按钮、侧边栏切换或分页。“打开文件夹”打开当前虚拟目录；
文件通过系统默认应用打开。调整树宽也支持左右方向键。
每个媒体库节点直接提供重命名、修改规则和删除图标，目录节点直接提供重命名／移动和
删除图标；点击即打开对应弹窗，不再经过管理菜单。操作绑定该节点，不要求先切换当前
浏览目录；没有管理权限或操作处理中时禁用。
右侧列表的打开、重命名／移动、换源和删除操作也位于条目所在行，并按条目类型和权限
提供。行内打开目录会进入该目录；详情区域只显示当前选中条目的信息。

库支持创建、查看、重命名、修改 Profile 和删除。Profile 修改沿用既有语义，
不会重新命名现有文件。目录支持创建、查看、同库重命名／移动和删除，禁止覆盖目标
或移动到自身子树。映射支持创建、同库移动和移除；后台允许更换源文件时才提供
该入口，并先预览新源文件信息再确认。

单个 NFO、海报等库内文件只提供浏览和打开，修改交给外部应用。删除整个目录或库
是例外：必须先显示全部子目录、映射和库内文件计数，明确源文件保留、库内文件移除，
再确认。后台在执行时复核预览；变化返回 `ConfirmationMismatch`，GUI 清除旧预览。
事务和回收语义见[运行与 CLI](operations.md#桌面目录管理控制接口)。

映射的大小和修改时间来自缓存，不代表源文件已经检查。检查结果来自显式执行的
doctor，并显示检查时间；管理操作后清除旧检查结果。挂载、连接或权限异常不能显示为空库。
旧后台缺少目录接口时明确提示升级。

## 服务与配置

运行状态页分别显示 systemd 状态、后台就绪状态、挂载、socket、数据库和文件存储。
数据库和文件存储的路径来自后台实际打开的资源；仅保存新配置时仍显示当前运行路径。
启动／重启成功后继续检查后台、挂载和 socket，不能只凭 systemctl 成功显示就绪。
停止／重启需要确认；未安装后台时显示安装文档位置。

配置表单涵盖全部十个后台字段，并提供原始 TOML 编辑器修复无效配置。表单预览保留
未修改文本和注释；原始编辑器按用户输入保存。保存前严格校验并列出字段变更。
数据库或文件存储路径变更明确提示不会迁移数据。

受限助手只允许读取／保存 `/etc/scrapefs/config.toml`，或对固定的
`scrapefsd.service` 执行 start、stop、restart；不能指定其他路径、unit 或命令。
助手和路径必须由 root 所有且不能被其他用户写入，不接受符号链接配置。
配置保存使用锁、原始文本摘要复核、同目录原子替换，保留 UID/GID 和权限，
上一版备份为 `config.toml.previous`。损坏的原文件也保留为备份。
外部 root 编辑器不使用助手的锁；保存前会再次读取检查，但不承诺与这类编辑器的
任意并发写入互斥。

“仅保存”和“保存并重启”是分开的操作。后台报告启动时配置的语义摘要，GUI 据此
区分磁盘配置和运行配置。仅保存不会切换正在使用的挂载或 socket；新 endpoint
只有在实际连接成功后才采用。保存成功但重启失败时，明确保留“已保存”的结果并显示错误。
配置不可读时可授权读取；授权快照的旧写入仍由助手在保存时拒绝。

中文／English 切换位于顶部最右侧，立即生效。目录、滚动位置、表单和管理预览保持，
文件名、路径及规则 ID 不翻译。语言保存在当前用户的
`$XDG_CONFIG_HOME/scrapefs/desktop.json`（默认 `~/.config/scrapefs/desktop.json`），
不修改后台配置，不重启服务。

## 验证

核心 Go 模块不依赖桌面动态库；Wails 放在独立 `desktop/` 模块。

```bash
go test ./...
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
cd desktop
go test -race ./internal/gui
go vet ./internal/gui
go build -tags 'server desktoptest' -o bin/test-server ./tests/server
cd frontend
npm ci
npm run build
npx playwright install chromium --no-shell
npm test
```

端到端测试需要主机上存在 `scrapefs-control` 组，并且测试用户可使用该 GID。
测试使用临时 SQLite、对象存储和真实 Unix 控制接口，通过 Wails 的测试专用
loopback server 连接前端。系统服务、授权和系统文件打开由临时测试实现记录；
不挂载 FUSE，不执行真实 Polkit/systemd 操作。生产入口拒绝 `server` 构建标签。
原生构建和真实桌面授权／外部应用集成需另行验证。
