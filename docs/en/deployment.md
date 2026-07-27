# Deployment Guide

**English** | [简体中文](../zh-CN/deployment.md)

This guide explains how to build ScrapeFS from source, initialize the standard
host paths and permissions, and run the daemon through systemd. See
[Operations and CLI](operations.md) for the complete configuration and runtime
contract.

## 1. Prerequisites

- Linux and systemd;
- `/dev/fuse`;
- `findmnt` from util-linux;
- Go 1.25 or newer;
- root access for installing files, creating groups, and mounting FUSE;
- an existing ordinary user who will administer ScrapeFS.

The installer uses the fixed first-version paths:

```text
binaries:     /usr/local/bin/scrapefs
              /usr/local/bin/scrapefsd
configuration:/etc/scrapefs/config.toml
state:        /var/lib/scrapefs
mountpoint:   /mnt/scrapefs
service:      /etc/systemd/system/scrapefsd.service
```

## 2. Build From Source

Build both commands from the repository root:

```bash
mkdir -p build

go build -trimpath -o build/scrapefs ./cmd/scrapefs
go build -trimpath -o build/scrapefsd ./cmd/scrapefsd
```

Manually built binaries report the default `dev` version.

## 3. Initialize The Host

Specify an existing administrator and the binary directory:

```bash
sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build
```

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
modifying any installation target; stop a running service before updating it.
A new supplementary-group membership normally requires the administrator to
log in again before new processes inherit it. The user named by
`control.administrator_uid` still receives administrator access through the
control-socket ACL.

The installer supports standard `DESTDIR` staging. Staging creates only the
file layout and does not modify users, groups, or systemd state; it is intended
primarily for packaging and automated verification.

## 4. Review The Configuration

Before the first start, inspect:

```bash
sudo cat /etc/scrapefs/config.toml
```

The default configuration uses `/mnt/scrapefs`, `/var/lib/scrapefs`, and
`/run/scrapefs/scrapefs.sock`. If paths are changed, the database, object
store, mountpoint, and socket remain pairwise disjoint lexically normalized
absolute paths.

## 5. Start The Service

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

## 6. Verify

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

## 7. Update

After rebuilding or obtaining new binaries, run the installer again. Existing
configuration is preserved while project-managed binaries and the generic
unit are updated:

```bash
sudo systemctl stop scrapefsd.service

sudo ./scripts/install.sh \
  --administrator "$USER" \
  --binary-dir ./build

sudo systemctl start scrapefsd.service
scrapefs doctor
```
