package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	charmlog "github.com/charmbracelet/log"
	"golang.org/x/term"
	"gopkg.in/natefinch/lumberjack.v2"
)

// LogLevelEnvVar is the environment variable used to set the log level.
const LogLevelEnvVar = "BLADERUNNER_LOG_LEVEL"

var (
	mu     sync.RWMutex
	logger = charmlog.NewWithOptions(os.Stdout, charmlog.Options{
		Level:           levelFromEnv(),
		ReportTimestamp: true,
		TimeFormat:      "2006-01-02 15:04:05",
	})

	// fileWriter holds the rotator-only writer set by Init so SetQuiet can
	// swap between (terminal + file) and (file only) at runtime.
	fileWriter io.Writer
)

// parseLevel maps a string (case-insensitive) to a charmlog.Level. Accepted
// values are "debug", "info", "warn"/"warning", and "error". Unknown or empty
// values fall back to InfoLevel.
func parseLevel(s string) charmlog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return charmlog.DebugLevel
	case "info":
		return charmlog.InfoLevel
	case "warn", "warning":
		return charmlog.WarnLevel
	case "error":
		return charmlog.ErrorLevel
	default:
		return charmlog.InfoLevel
	}
}

// levelFromEnv reads BLADERUNNER_LOG_LEVEL and returns the parsed level.
func levelFromEnv() charmlog.Level {
	return parseLevel(os.Getenv(LogLevelEnvVar))
}

// Init configures the structured logger. When stdout is a terminal the
// logger writes to the rotating log file only — the assumption is that an
// interactive caller has its own UI (e.g. the boot board) owning the
// screen, and slog spam would interfere with it. SetQuiet flips this at
// runtime if a caller needs the inverse.
//
// In non-TTY environments (CI, log capture) the logger writes to both the
// file and stdout so existing scrapers keep working.
func Init(logPath string) error {
	if logPath == "" {
		return fmt.Errorf("log path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    25, // MB
		MaxBackups: 5,
		MaxAge:     14, // days
		Compress:   true,
	}

	level := levelFromEnv()
	mu.Lock()
	fileWriter = rotator
	mu.Unlock()
	w := writerForTTY(isStdoutTTY())
	l := charmlog.NewWithOptions(w, charmlog.Options{
		Level:           level,
		ReportTimestamp: true,
		TimeFormat:      "2006-01-02 15:04:05",
	})

	mu.Lock()
	logger = l
	mu.Unlock()

	logger.Info("logging initialized", "path", logPath, "level", level)
	return nil
}

// SetQuiet routes log output to the rotating file only (quiet=true) or to
// both the file and stdout (quiet=false). No-op until Init has been called.
// Used by interactive callers to silence slog while a TUI owns the screen.
func SetQuiet(quiet bool) {
	mu.Lock()
	defer mu.Unlock()
	if fileWriter == nil {
		return
	}
	logger.SetOutput(writerLocked(quiet))
}

// writerForTTY returns the writer to install given whether stdout is a TTY.
// Caller must ensure fileWriter is populated.
func writerForTTY(tty bool) io.Writer {
	if tty {
		return fileWriter
	}
	return io.MultiWriter(os.Stdout, fileWriter)
}

// writerLocked returns the appropriate writer for the given quiet flag.
// Caller must hold mu.
func writerLocked(quiet bool) io.Writer {
	if quiet {
		return fileWriter
	}
	return io.MultiWriter(os.Stdout, fileWriter)
}

func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func SetLevel(level charmlog.Level) {
	mu.Lock()
	defer mu.Unlock()
	logger.SetLevel(level)
}

func L() *charmlog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return logger
}
