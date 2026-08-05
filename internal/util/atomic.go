package util

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// tmpSuffix is the os.CreateTemp pattern appended to the destination file name
// to form the staging file. The per-destination prefix keeps concurrent writers
// of DIFFERENT files apart, and the random suffix CreateTemp substitutes for
// the star keeps concurrent writers of the SAME file apart.
const tmpSuffix = ".tmp-*"

// WriteFileAtomic writes data to path atomically and durably.
//
// The data is written to a temp file in the destination's own directory (so the
// rename never crosses a filesystem boundary), flushed to stable storage,
// renamed over path, and the directory itself is then fsynced. A concurrent
// reader therefore observes either the previous contents or the new ones, never
// a partial file, and the rename survives a host crash.
//
// The directory fsync is the part a naive temp+rename omits: without it the
// rename can still be lost after a power failure even though the file contents
// were synced. It is best effort — see SyncDir.
//
// A failed write leaves the destination untouched and removes the temp file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+tmpSuffix)
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if err := writeSyncClose(tmp, data, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish %s: %w", path, err)
	}
	SyncDir(dir)
	return nil
}

// PublishFileAtomic makes an already-written file src the contents of dst, in
// one step.
//
// It is the large-file counterpart of WriteFileAtomic, for bytes some other
// tool produced for us (hdiutil converting a cartridge, qemu-img materializing
// a disk): the data never passes through this process, but the publish is still
// a single rename, so a reader of dst sees either the whole old file or the
// whole new one. src is flushed to stable storage first and the directory is
// fsynced after, so the swap survives a host crash.
//
// src and dst MUST live on the same filesystem, because os.Rename cannot cross
// one — build the temporary in the destination's own directory.
//
// A failed publish leaves dst BYTE-FOR-BYTE UNTOUCHED and leaves src in place;
// removing or salvaging it is the caller's decision, because for a large
// artifact it may be the only copy of work worth keeping.
func PublishFileAtomic(src, dst string) error {
	// O_RDWR rather than O_RDONLY: fsync on a read-only descriptor is not
	// portable, and this file is one we wrote, so write access is ours to take.
	f, err := os.OpenFile(src, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flush %s: %w", src, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", src, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("publish %s over %s: %w", src, dst, err)
	}
	SyncDir(filepath.Dir(dst))
	return nil
}

// writeSyncClose writes data to f with mode perm, flushes it to stable storage
// and closes it. f is closed exactly once on every path.
func writeSyncClose(f *os.File, data []byte, perm fs.FileMode) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	// os.CreateTemp always creates 0600; widen (or narrow) to the caller's mode
	// before the rename so the file is never briefly visible at the wrong mode.
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// SyncDir fsyncs a directory so a rename into it is durable.
//
// It is the half of an atomic publish that a naive temp-and-rename omits:
// without it the rename can still be lost after a power failure even though the
// file contents were synced. Callers that do their own renames — the updater
// swaps a whole .app bundle rather than a file — need it directly.
//
// Best effort: some filesystems refuse to open a directory for sync, and a
// failure here only costs durability across a host crash, never correctness.
// There is therefore nothing for a caller to handle, so nothing is returned.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
