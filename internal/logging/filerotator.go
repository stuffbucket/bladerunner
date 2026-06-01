package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

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
// that a long-running operation gets its own log file (e.g. `runner start`
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
