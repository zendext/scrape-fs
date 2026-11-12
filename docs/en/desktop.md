---
id: desktop-en
type: module-spec
status: draft
---

# Desktop Manager

**English** | [简体中文](../zh-CN/desktop.md)

> This is an English translation. The Chinese documentation is authoritative.

The Wails 3 / SolidJS GUI manages an already installed daemon on the same Linux
computer. Closing the window leaves the daemon running. It does not install or
automatically upgrade the daemon or expose external Agent batch operations.

## Build and Installation

Install the daemon using the [deployment guide](deployment.md).
[GitHub Releases](https://github.com/zendext/scrape-fs/releases) provides a core
archive and an optional desktop archive. Install the core package first, then
the desktop package:

```bash
version=v0.3.0
archive="scrape-fs-desktop_${version}_linux_amd64.tar.gz"
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "$archive"
sudo "./${archive%.tar.gz}/scripts/install-desktop.sh"
scrapefs-gui
```

The prebuilt v0.3.0 desktop package requires glibc 2.38+, GTK4, and WebKitGTK 6.0.
Desktop components can also be built separately from source. Building
requires Go 1.26+, Node.js 22.12+, npm, a C compiler, pkg-config, and GTK4 / WebKitGTK
6.0 development files. Running requires the corresponding shared libraries,
systemd, Polkit's `pkexec`, and an authorization agent in the desktop session.
Files open through the default system application.

```bash
./scripts/build-desktop.sh
sudo ./scripts/install-desktop.sh
scrapefs-gui
```

The desktop installer installs only `/usr/local/bin/scrapefs-gui`,
`/usr/local/libexec/scrapefs-admin`, and an application menu entry. It requires an
installed daemon and does not change daemon configuration, groups, media libraries,
or service state. `DESTDIR=/absolute/staging/path` stages the files only.

Run the GUI as a normal user. Existing [control roles](operations.md) apply:
control users browse and manage mappings; library and folder management requires
the configured `control.administrator_uid`. System service and configuration
actions separately request Linux administrator authorization.

```bash
scrapefs-gui --socket /absolute/path/control.sock --config /absolute/path/config.toml
```

A custom socket or configuration path disables system service actions and
configuration writes. The GUI never opens SQLite or the object store directly.
Files open through the running FUSE mount.

## Folders and Mappings

A persistent, resizable folder tree sits beside a complete file list. The list
scrolls continuously with a sticky header. Long names have ellipsis and full
tooltips. Selecting an entry shows its details below the list; closing details
clears the selection. Hover highlights cover the whole tree or list row.
The interface defaults to the design's dark palette without changing the system
theme.

Creation, management, and confirmation use modal dialogs in the same window.
Background controls are unavailable until the dialog closes, then focus
returns to the initiating control. Dialogs cannot close while an operation runs.
Language switching is available only at the top of the main window. Language
and naming rules use the same dark selector, with arrow keys, Home/End, typeahead,
Enter/Space to confirm, Escape to cancel, and Tab to leave. Selected options and
the current keyboard option have distinct indicators.

The address bar accepts `library:/relative/path`. Ctrl+L selects it, Enter navigates,
and Escape restores the current address. There are no breadcrumbs, Up button,
sidebar toggle, or pages. Open folder opens the current virtual folder in the
system file manager. Files open in the default application. Arrow keys can resize
the tree. Library rows directly offer rename, naming-rule, and delete icons;
folder rows offer rename/move and delete icons. Each opens the corresponding
dialog without an intermediate management menu, acting on that node without
changing the current browser location. Actions are disabled without administrator
permission or while an operation is running.
The file list also places open, rename/move, source-replacement, and removal
actions in each entry's row, according to its type and permissions. Opening a
directory from its row navigates into it. The details area only shows information
about the selected entry.

Libraries support creation, viewing, renaming, Profile changes, and deletion.
Changing a Profile retains the existing semantics and does not rename files.
Folders support creation, browsing, same-library rename/move, and deletion.
Overwriting targets and moves into the same subtree are rejected. Mappings
support creation, same-library moves, and removal. Source replacement appears
only when allowed by the daemon and requires a source preview and confirmation.

Individual NFO files, artwork, and other library files can only be browsed and
opened; editing belongs to external applications. Removing an entire folder or
library is the explicit exception. The GUI first previews all subfolder, mapping,
and library-file counts and states that source files are retained while library
files are removed. The daemon rechecks the preview on confirmation; changes
return `ConfirmationMismatch`, and the GUI discards the stale preview. See the
[control contract](operations.md#desktop-directory-management-control-api).

Mapping size and modification time are cached and do not imply source health.
Explicit doctor results show their check time and are cleared after management
changes. Connection, mount, and permission failures must not appear as empty
libraries. Older daemons without directory support produce an upgrade message.

## Service and Configuration

The service page separates systemd state, daemon readiness, mount, socket,
database, and file storage. Start/restart checks actual daemon, mount, and socket
readiness after systemctl succeeds. Database and file-storage paths come from
the daemon's opened resources and retain their running values after Save only.
Stop/restart requires confirmation. Missing daemons show the installation
documentation location.

The configuration form covers all ten daemon fields. A raw TOML editor repairs
invalid files. Form previews retain unchanged text and comments; raw editing saves
the user's text. Strict validation and a field diff precede saving. Storage path
changes explicitly state that data is not migrated.

The restricted helper can only read/save `/etc/scrapefs/config.toml` or start,
stop, and restart the fixed `scrapefsd.service`. It accepts no other paths, units,
or commands. The helper and paths must be root-owned and not writable by other
users. Symlink configurations are rejected. Saving uses a lock, raw-text digest
checks, an atomic same-directory replacement, retained UID/GID and permissions,
and a `config.toml.previous` backup, including when repairing an invalid original.
External root editors do not take this helper's lock. Saving rereads the file,
but does not claim mutual exclusion with arbitrary concurrent external writes.

Save only and Save and restart are distinct. The daemon reports the semantic
digest of its startup configuration, allowing the GUI to distinguish saved and
running configurations. Saving alone does not switch the active mount or socket.
A new endpoint is adopted only after a successful connection. Restart failure
after saving explicitly retains and reports the saved result. Unreadable files
can be read with authorization; stale authorized snapshots are still rejected by
the helper on save.

The 中文 / English selector sits at the far right of the header and applies
immediately, retaining folder, scroll, forms, and management previews. Names,
paths, and rule IDs remain unchanged. Language is stored in the user's
`$XDG_CONFIG_HOME/scrapefs/desktop.json` (default
`~/.config/scrapefs/desktop.json`), independently of daemon configuration and
service restarts.

## Verification

The core Go module does not require desktop libraries. Wails lives in the
independent `desktop/` module.

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

End-to-end tests require the `scrapefs-control` group and permission for the test
user to use its GID. They use temporary SQLite/object storage and the real Unix
control API through a test-only Wails loopback server. System service,
authorization, and external file opening are recorded test implementations.
They do not mount FUSE or invoke actual Polkit/systemd actions. The production
entry point rejects the `server` build tag. Native builds and real desktop
authorization/external applications require separate verification.
