# Deployment Guide

**English** | [简体中文](../zh-CN/deployment.md)

This guide explains how to install ScrapeFS from a GitHub Release or source,
initialize the standard host paths and permissions, and run the daemon through
systemd. See [Operations and CLI](operations.md) for the complete
configuration and runtime contract.

## 1. Prerequisites

- Linux and systemd;
- `/dev/fuse`;
- `findmnt` from util-linux;
- root access for installing files, creating groups, and mounting FUSE;
- an existing ordinary user who will administer ScrapeFS.

Building manually from source also requires Go 1.25 or newer.

The installer uses the fixed first-version paths:

```text
binaries:     /usr/local/bin/scrapefs
              /usr/local/bin/scrapefsd
configuration:/etc/scrapefs/config.toml
state:        /var/lib/scrapefs
mountpoint:   /mnt/scrapefs
service:      /etc/systemd/system/scrapefsd.service
```

## 2. Install From GitHub Releases

Each versioned tag publishes a `linux/amd64` archive and `SHA256SUMS`. Download
both files for the same version from
[GitHub Releases](https://github.com/zendext/scrape-fs/releases), then verify,
extract, and run the bundled installer:

```bash
version=vX.Y.Z
archive="scrape-fs_${version}_linux_amd64.tar.gz"

sha256sum -c SHA256SUMS
tar -xzf "$archive"
cd "${archive%.tar.gz}"

sudo ./install.sh --administrator "$USER"
```

The bundled `scrapefs` and `scrapefsd` report the corresponding tag version.
The installer, both binaries, and the generic systemd unit are colocated, so
this path does not require a repository checkout or Go toolchain.
Versions whose tags contain `-` are published as GitHub prereleases and are
explicitly excluded from Latest.

## 3. Build From Source

Build both commands from the repository root:

```bash
mkdir -p build

go build -trimpath -o build/scrapefs ./cmd/scrapefs
go build -trimpath -o build/scrapefsd ./cmd/scrapefsd
```

Manually built binaries report the default `dev` version.

Specify an existing administrator and pass the build directory to the
repository installer:

```bash
sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build
```

## 4. Installer Behavior

The script:

- installs or updates both binaries;
- creates the `scrapefs` and `scrapefs-control` groups when missing;
- adds the selected administrator to `scrapefs-control`;
- creates the fail-closed mountpoint as `root:scrapefs` with mode `0710`;
- creates the root-owned state directory with mode `0700`;
- creates the default configuration only when it does not already exist;
- installs the generic systemd `Type=notify` unit;
- runs `systemctl daemon-reload`.

The script does not start or enable the service and does not overwrite an
existing configuration. If the mountpoint is mounted, the script fails before
modifying any installation target. Existing configuration, state, and
mountpoint paths must be real directories; symbolic links and other file types
are also rejected before any modification. Stop a running service before
updating it. A new supplementary-group membership normally requires the
administrator to log in again before new processes inherit it. The user named
by `control.administrator_uid` still receives administrator access through
the control-socket ACL.

The installer supports standard `DESTDIR` staging. Staging creates only the
file layout and does not modify users, groups, or systemd state; it is intended
primarily for packaging and automated verification.

## 5. Review The Configuration

Before the first start, inspect:

```bash
sudo cat /etc/scrapefs/config.toml
```

The default configuration uses `/mnt/scrapefs`, `/var/lib/scrapefs`, and
`/run/scrapefs/scrapefs.sock`. If paths are changed, the database, object
store, mountpoint, and socket remain pairwise disjoint lexically normalized
absolute paths.

## 6. Start The Service

After reviewing the configuration, enable and start the service:

```bash
sudo systemctl enable --now scrapefsd.service
```

The generic unit contains no host-specific source-filesystem, network-storage,
or other service dependency. Administrators should use a local systemd
drop-in for additional ordering or start conditions instead of editing the
unit shipped by the repository.

The daemon runs as root by default because the current descriptor-anchored,
host-visible FUSE mount requires root or effective `CAP_SYS_ADMIN`. Only
trusted users belong in `scrapefs-control`; those users can select source paths
that the daemon opens.

## 7. Verify

Check systemd, FUSE, and the ScrapeFS diagnostics:

```bash
systemctl status scrapefsd.service
findmnt /mnt/scrapefs
scrapefs status
scrapefs doctor
```

For a healthy runtime:

- the service is `active (running)`;
- the mount type is `fuse.scrapefs`;
- `status.state` is `ready`;
- `doctor.severity` is `healthy`.

Create the first Library:

```bash
scrapefs library add jellyfin --profile jellyfin@1
scrapefs library list
```

## 8. Update

Download and extract a new Release or rebuild from source. Stop the service and
run the corresponding installer again; existing configuration is preserved
while project-managed binaries and the generic unit are updated:

```bash
sudo systemctl stop scrapefsd.service

# From an extracted Release:
sudo ./install.sh --administrator "$USER"

# Or from the source repository:
sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build

sudo systemctl start scrapefsd.service
scrapefs doctor
```
