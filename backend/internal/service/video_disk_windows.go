//go:build windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func videoDiskLock(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var o windows.Overlapped
	for {
		err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
		if err == nil {
			return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o); _ = f.Close() }, nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
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
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free uint64
	err = windows.GetDiskFreeSpaceEx(p, &free, nil, nil)
	return int64(free), err
}
