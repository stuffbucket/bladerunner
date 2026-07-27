package logging

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// RotateOptions configure a RotatingFile.
type RotateOptions struct {
	// MaxSize is the max size in MB before a rotation.
	MaxSize int
	// MaxBackups is the max number of rotated files to retain.
	MaxBackups int
	// MaxAge is the max age in days for old log files.
	MaxAge int
	// Compress enables gzip compression of rotated files.
	Compress bool
}

// RotatingFile bridges a writable *os.File (suitable for handing to APIs
// that demand a real file descriptor, such as Virtualization.framework's
// VZFileHandleSerialPortAttachment) to a lumberjack-backed rotating log.
//
// Bytes written to File() flow through an internal pipe and are copied
// by a goroutine into the lumberjack rotator. Close() shuts the writer
// pipe end, waits for the pump goroutine to drain, then closes the
// rotator (which closes the current log file).
type RotatingFile struct {
	file    *os.File // pipe write end exposed to callers
	pipeR   *os.File
	rotator *lumberjack.Logger

	wg       sync.WaitGroup
	closeOne sync.Once
	closeErr error
}

// NewRotatingFile opens path with rotation. The returned *os.File via
// File() must be passed to the consumer; the consumer's writes will be
// rotated. Callers MUST call Close() to flush and release resources.
func NewRotatingFile(path string, opts RotateOptions) (*RotatingFile, error) {
	if path == "" {
		return nil, fmt.Errorf("rotating file path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create rotating file directory: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create pipe for rotating file: %w", err)
	}

	rot := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    opts.MaxSize,
		MaxBackups: opts.MaxBackups,
		MaxAge:     opts.MaxAge,
		Compress:   opts.Compress,
	}

	rf := &RotatingFile{
		file:    pw,
		pipeR:   pr,
		rotator: rot,
	}

	rf.wg.Add(1)
	go rf.pump()

	return rf, nil
}

func (r *RotatingFile) pump() {
	defer r.wg.Done()
	// io.Copy returns when the pipe write end is closed.
	_, _ = io.Copy(r.rotator, r.pipeR)
}

// File returns the writable end of the pipe. Pass this to APIs that
// require a real *os.File. Do not close it directly — call Close()
// on the RotatingFile instead.
func (r *RotatingFile) File() *os.File {
	return r.file
}

// Rotate forces lumberjack to close the current log file and start a new
// one, even if MaxSize has not been reached. Callers use this to guarantee
// that a long-running operation gets its own log file (e.g. `br start`
// rotates console.log so the new boot's serial output isn't mixed with
// the previous shutdown's). Returns any error from the rotator.
func (r *RotatingFile) Rotate() error {
	if r.rotator == nil {
		return nil
	}
	return r.rotator.Rotate()
}

// Close shuts the writer, waits for the pump to finish, and closes
// the rotator. Safe to call multiple times.
func (r *RotatingFile) Close() error {
	r.closeOne.Do(func() {
		// Closing the write end signals EOF to the pump.
		if err := r.file.Close(); err != nil {
			r.closeErr = err
		}
		r.wg.Wait()
		if err := r.pipeR.Close(); err != nil && r.closeErr == nil {
			r.closeErr = err
		}
		if err := r.rotator.Close(); err != nil && r.closeErr == nil {
			r.closeErr = err
		}
	})
	return r.closeErr
}

// bytesPerMB converts RotateOptions.MaxSize (megabytes, lumberjack's unit) into
// bytes.
const bytesPerMB = 1024 * 1024

// The names of rotated files follow lumberjack, so one log rotated by both
// RotateIfLarger and a lumberjack rotator still has a single series of
// generations that either side recognizes.
const (
	// backupTimeFormat is the UTC timestamp that separates the name of a
	// rotated file from its extension.
	backupTimeFormat = "2006-01-02T15-04-05.000"
	// compressSuffix marks a rotated file that is gzipped.
	compressSuffix = ".gz"
	// oneDay converts RotateOptions.MaxAge (days) into a duration.
	oneDay = 24 * time.Hour
)

// RotateIfLarger rotates the log at path when it has already grown past
// opts.MaxSize megabytes, then returns — leaving no file open.
//
// It exists for logs that are written by a DIFFERENT process than the one that
// opens them: a detached child inherits a plain file descriptor, so there is
// nobody on this side to notice it growing. RotatingFile cannot serve that case
// at all, because its pump goroutine lives in the process that created it — a
// short-lived spawner would exit and leave the child writing into a pipe with
// no reader. Checking the size at open time is the part that can be done from
// here, and it is what stops the file growing without bound across spawns.
//
// Every part of the rotation is FINISHED when this function returns, and that
// is why it does not hand the file to lumberjack the way RotatingFile and Init
// do. Lumberjack prunes and compresses on a background goroutine that Close()
// does not wait for. The caller here is a spawner that exits seconds later, so
// that goroutine is killed part way through: the backups are never pruned (the
// unbounded file comes back one level up, as an unbounded set of backups) and a
// half-written .gz is left on disk. A caller that outlives its own rotation is
// also the only kind that a test can observe.
//
// A path that does not exist yet is not an error: there is nothing to rotate.
func RotateIfLarger(path string, opts RotateOptions) error {
	if path == "" || opts.MaxSize <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat log %s: %w", path, err)
	}
	if info.Size() < int64(opts.MaxSize)*bytesPerMB {
		return nil
	}

	now := time.Now()
	if err := rotateNow(path, info.Mode().Perm(), now); err != nil {
		return err
	}
	return retainBackups(path, opts, now)
}

// rotateNow moves path aside under a timestamped name and puts an empty file
// with the same permissions back in its place. The caller opens that empty file
// for itself: this side keeps no descriptor.
func rotateNow(path string, mode os.FileMode, now time.Time) error {
	if err := os.Rename(path, backupName(path, now)); err != nil {
		return fmt.Errorf("rotate log %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open log %s after rotation: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close log %s after rotation: %w", path, err)
	}
	return nil
}

// backupName is the name the rotated generation of path takes.
func backupName(path string, now time.Time) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "-" + now.UTC().Format(backupTimeFormat) + ext
}

// backup is one rotated generation of a log.
type backup struct {
	path       string
	stamp      time.Time
	compressed bool
}

// retainBackups applies MaxBackups and MaxAge to the rotated generations of
// path, then gzips the generations that it kept. It does inline what lumberjack
// does on its mill goroutine.
func retainBackups(path string, opts RotateOptions, now time.Time) error {
	if opts.MaxBackups <= 0 && opts.MaxAge <= 0 && !opts.Compress {
		return nil
	}
	kept, err := listBackups(path)
	if err != nil {
		return err
	}
	if opts.MaxBackups > 0 && len(kept) > opts.MaxBackups {
		if err := removeBackups(kept[opts.MaxBackups:]); err != nil {
			return err
		}
		kept = kept[:opts.MaxBackups]
	}
	if opts.MaxAge > 0 {
		if kept, err = dropExpired(kept, now.UTC().Add(-time.Duration(opts.MaxAge)*oneDay)); err != nil {
			return err
		}
	}
	if !opts.Compress {
		return nil
	}
	for _, b := range kept {
		if b.compressed {
			continue
		}
		if err := compressBackup(b.path); err != nil {
			return err
		}
	}
	return nil
}

// dropExpired removes every backup older than cutoff and returns the rest.
func dropExpired(backups []backup, cutoff time.Time) ([]backup, error) {
	fresh := make([]backup, 0, len(backups))
	stale := make([]backup, 0, len(backups))
	for _, b := range backups {
		if b.stamp.Before(cutoff) {
			stale = append(stale, b)
			continue
		}
		fresh = append(fresh, b)
	}
	if err := removeBackups(stale); err != nil {
		return nil, err
	}
	return fresh, nil
}

// removeBackups deletes the generations that fell outside the retention policy.
// A file that something else already deleted is not an error.
func removeBackups(backups []backup) error {
	for _, b := range backups {
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove rotated log %s: %w", b.path, err)
		}
	}
	return nil
}

// listBackups returns the rotated generations of path, newest first. A file
// that does not carry the backup naming of THIS log is not one of them, so
// neither an unrelated file nor another instance's log in the same directory
// can be deleted or compressed here.
func listBackups(path string) ([]backup, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read log directory %s: %w", dir, err)
	}
	backups := make([]backup, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stamp, compressed, ok := parseBackupName(e.Name(), prefix, ext)
		if !ok {
			continue
		}
		backups = append(backups, backup{
			path:       filepath.Join(dir, e.Name()),
			stamp:      stamp,
			compressed: compressed,
		})
	}
	slices.SortFunc(backups, func(a, b backup) int { return b.stamp.Compare(a.stamp) })
	return backups, nil
}

// parseBackupName reads the timestamp out of the name of a rotated file. ok is
// false when the name is not a generation of the log that prefix and ext
// describe.
func parseBackupName(name, prefix, ext string) (stamp time.Time, compressed, ok bool) {
	if !strings.HasPrefix(name, prefix) {
		return time.Time{}, false, false
	}
	rest := strings.TrimPrefix(name, prefix)
	compressed = strings.HasSuffix(rest, compressSuffix)
	rest = strings.TrimSuffix(rest, compressSuffix)
	if !strings.HasSuffix(rest, ext) {
		return time.Time{}, false, false
	}
	parsed, err := time.Parse(backupTimeFormat, strings.TrimSuffix(rest, ext))
	if err != nil {
		return time.Time{}, false, false
	}
	return parsed, compressed, true
}

// compressBackup gzips one rotated file and deletes the original. A failure
// leaves no partial .gz behind: the caller must be able to try again.
func compressBackup(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat rotated log %s: %w", path, err)
	}
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open rotated log %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()

	gzPath := path + compressSuffix
	dst, err := os.OpenFile(gzPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create compressed log %s: %w", gzPath, err)
	}
	if err := writeGzip(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(gzPath)
		return fmt.Errorf("compress rotated log %s: %w", path, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(gzPath)
		return fmt.Errorf("close compressed log %s: %w", gzPath, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove rotated log %s after compression: %w", path, err)
	}
	return nil
}

// writeGzip copies src into dst through a gzip writer and closes that writer: a
// gzip stream is incomplete until its writer is closed.
func writeGzip(dst io.Writer, src io.Reader) error {
	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}
