package fuse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

type mountedServer interface {
	Unmount() error
}

type mountFilesystem func(
	string,
	gofs.InodeEmbedder,
	*gofs.Options,
) (mountedServer, error)

type Mounted struct {
	server     mountedServer
	mountpoint *preparedMountpoint
	mu         sync.Mutex
	finished   bool
	unmountErr error
}

func Mount(
	ctx context.Context,
	configuration config.FUSE,
	repository *namespace.Repository,
	objects objectMetadataReader,
) (_ *Mounted, err error) {
	if repository == nil {
		return nil, errors.New("namespace repository is required")
	}
	root, err := newRoot(ctx, repository, objects)
	if err != nil {
		return nil, err
	}
	if err := clearStaleMount(configuration.Mountpoint); err != nil {
		return nil, err
	}

	prepared, err := prepareMountpointDirectory(configuration.Mountpoint)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()
	if err := prepared.verifyPath(); err != nil {
		return nil, err
	}

	options := filesystemOptions(configuration)
	server, err := mountPrepared(
		prepared,
		root,
		&options,
		mountNodeFS,
	)
	if err != nil {
		return nil, fmt.Errorf("mount ScrapeFS at %q: %w", configuration.Mountpoint, err)
	}
	if err := verifyMounted(configuration.Mountpoint); err != nil {
		return nil, errors.Join(err, unmountPrepared(prepared, server))
	}
	return &Mounted{server: server, mountpoint: prepared}, nil
}

type directMountedServer struct {
	server    *gofuse.Server
	target    string
	mu        sync.Mutex
	unmounted bool
}

func (server *directMountedServer) Unmount() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.unmounted {
		return nil
	}
	if err := unix.Unmount(server.target, 0); err != nil {
		return err
	}
	server.server.Wait()
	server.unmounted = true
	return nil
}

func mountNodeFS(
	target string,
	root gofs.InodeEmbedder,
	options *gofs.Options,
) (_ mountedServer, err error) {
	var stat unix.Stat_t
	if err := unix.Stat(target, &stat); err != nil {
		return nil, fmt.Errorf("inspect anchored FUSE mount target: %w", err)
	}

	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/fuse: %w", err)
	}
	mounted := false
	defer func() {
		if err == nil {
			return
		}
		if mounted {
			err = errors.Join(err, unix.Unmount(target, 0))
		}
		err = errors.Join(err, unix.Close(fd))
	}()

	mountFlags := uintptr(unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC)
	mountData := []string{
		fmt.Sprintf("fd=%d", fd),
		fmt.Sprintf("rootmode=%o", stat.Mode&unix.S_IFMT),
		fmt.Sprintf("user_id=%d", os.Geteuid()),
		fmt.Sprintf("group_id=%d", os.Getegid()),
		fmt.Sprintf("max_read=%d", 128*1024),
		"default_permissions",
		"allow_other",
	}
	if err := unix.Mount(
		filesystemName,
		target,
		"fuse."+filesystemName,
		mountFlags,
		strings.Join(mountData, ","),
	); err != nil {
		return nil, fmt.Errorf(
			"mount anchored ScrapeFS target: %w; CAP_SYS_ADMIN or root is required",
			err,
		)
	}
	mounted = true

	raw := gofs.NewNodeFS(root, options)
	server, err := gofuse.NewServer(
		raw,
		fmt.Sprintf("/dev/fd/%d", fd),
		&options.MountOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("start ScrapeFS FUSE server: %w", err)
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return nil, fmt.Errorf("wait for ScrapeFS FUSE server: %w", err)
	}
	return &directMountedServer{server: server, target: target}, nil
}

func mountPrepared(
	prepared *preparedMountpoint,
	root gofs.InodeEmbedder,
	options *gofs.Options,
	mount mountFilesystem,
) (mountedServer, error) {
	var server mountedServer
	err := prepared.withTarget(func(target string) error {
		var err error
		server, err = mount(target, root, options)
		return err
	})
	if err != nil {
		return nil, err
	}
	return server, nil
}

func unmountPrepared(
	prepared *preparedMountpoint,
	server mountedServer,
) error {
	return prepared.withTarget(func(string) error {
		return server.Unmount()
	})
}

func (mounted *Mounted) Unmount() error {
	if mounted == nil {
		return nil
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.finished {
		return mounted.unmountErr
	}
	if err := unmountPrepared(mounted.mountpoint, mounted.server); err != nil {
		return err
	}
	mounted.unmountErr = errors.Join(
		mounted.mountpoint.verifyPath(),
		mounted.mountpoint.Close(),
	)
	mounted.finished = true
	return mounted.unmountErr
}

func verifyMounted(path string) error {
	mounts, err := readCurrentMounts()
	if err != nil {
		return err
	}
	var mounted *mountRecord
	for index := range mounts {
		if mounts[index].mountpoint == path {
			if mounted != nil {
				return fmt.Errorf("FUSE mountpoint %q has multiple mounts after startup", path)
			}
			mounted = &mounts[index]
		}
	}
	if mounted == nil {
		return fmt.Errorf("FUSE mountpoint %q is not mounted after startup", path)
	}
	if !mounted.ownedScrapeFS() {
		return fmt.Errorf(
			"FUSE mountpoint %q resolved to an unexpected or unowned mount %s from %s",
			path,
			mounted.filesystemType,
			mounted.source,
		)
	}
	return nil
}
