package store

import (
	"os"
	"path/filepath"
	"strconv"
)

func uintToString(v uint64) string { return strconv.FormatUint(v, 10) }

// atomicWrite writes data to tmp, fsyncs it and renames over target.
// The parent directory is fsynced as well so the rename itself is
// durable across power loss; callers that create a fresh shard
// directory use atomicMkdirFirst to sync that directory exactly once.
func atomicWrite(target, tmp string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	return syncDir(filepath.Dir(target))
}

// atomicWriteNoDirfsync is used for files in a directory whose
// existence and rename durability was established by an earlier sync
// (e.g. block shards within blocks/). The file's own fsync still
// guarantees its contents are durable; skipping the repeated directory
// fsync avoids one syscall per block during steady-state ingestion.
func atomicWriteNoDirfsync(target, tmp string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
