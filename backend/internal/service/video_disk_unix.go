//go:build !windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func videoDiskLock(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if err != unix.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func videoDiskFree(dir string) (int64, error) {
	var s unix.Statfs_t
	err := unix.Statfs(dir, &s)
	return int64(s.Bavail) * int64(s.Bsize), err
}
