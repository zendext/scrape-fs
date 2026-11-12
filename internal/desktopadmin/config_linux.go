package desktopadmin

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zendext/scrape-fs/internal/config"
	"golang.org/x/sys/unix"
)

const HelperPath = "/usr/local/libexec/scrapefs-admin"
const MaxConfigBytes = 64 << 10

var ErrChanged = errors.New("configuration changed after preview")
var ErrUnsafe = errors.New("unsafe system configuration path or ownership")

type configStore struct {
	directory string
	owner     uint32
}

func ReadConfiguration() (config.Document, error) {
	store := configStore{directory: filepath.Dir(config.DefaultPath)}
	root, err := store.open()
	if err != nil {
		return config.Document{}, err
	}
	defer root.Close()
	document, _, err := store.read(root)
	return document, err
}

func SaveConfiguration(text, expected string) (config.Document, error) {
	return (configStore{directory: filepath.Dir(config.DefaultPath)}).save(text, expected)
}

func (store configStore) open() (*os.Root, error) {
	info, err := os.Lstat(store.directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafe
	}
	root, err := os.OpenRoot(store.directory)
	if err != nil {
		return nil, err
	}
	anchored, err := root.Stat(".")
	if err != nil || !store.trusted(anchored) || !os.SameFile(info, anchored) {
		root.Close()
		return nil, ErrUnsafe
	}
	return root, nil
}

func (store configStore) trusted(info os.FileInfo) bool {
	if info == nil || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == store.owner
}

func (store configStore) read(root *os.Root) (config.Document, os.FileInfo, error) {
	file, err := root.OpenFile("config.toml", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return config.Document{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !store.trusted(info) {
		return config.Document{}, nil, ErrUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return config.Document{}, nil, err
	}
	if len(data) > MaxConfigBytes {
		return config.Document{}, nil, fmt.Errorf("configuration exceeds %d bytes", MaxConfigBytes)
	}
	document, _ := config.ParseDocument(string(data))
	return document, info, nil
}

func (store configStore) save(text, expected string) (config.Document, error) {
	if len(text) > MaxConfigBytes {
		return config.Document{}, fmt.Errorf("configuration exceeds %d bytes", MaxConfigBytes)
	}
	document, err := config.ParseDocument(text)
	if err != nil {
		return config.Document{}, err
	}
	root, err := store.open()
	if err != nil {
		return config.Document{}, err
	}
	defer root.Close()
	lock, err := root.OpenFile(".config.lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return config.Document{}, err
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || !store.trusted(lockInfo) {
		return config.Document{}, ErrUnsafe
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return config.Document{}, err
	}
	previous, info, err := store.read(root)
	if err != nil {
		return config.Document{}, err
	}
	if expected == "" || previous.Digest != expected {
		return config.Document{}, ErrChanged
	}
	if previous.Text == text {
		return document, nil
	}
	if err := writeAtomic(root, "config.toml.previous", []byte(previous.Text), info); err != nil {
		return config.Document{}, err
	}
	current, currentInfo, err := store.read(root)
	if err != nil {
		return config.Document{}, err
	}
	if current.Digest != expected || !os.SameFile(info, currentInfo) || currentInfo.Mode() != info.Mode() {
		return config.Document{}, ErrChanged
	}
	if err := writeAtomic(root, "config.toml", []byte(text), info); err != nil {
		return config.Document{}, err
	}
	return document, nil
}

func writeAtomic(root *os.Root, name string, data []byte, metadata os.FileInfo) error {
	temporary := ".config-" + rand.Text()
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	stat := metadata.Sys().(*syscall.Stat_t)
	if os.Geteuid() == 0 {
		if err := file.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err := file.Chmod(metadata.Mode().Perm()); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(temporary, name); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
