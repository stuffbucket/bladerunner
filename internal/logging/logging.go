package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	charmlog "github.com/charmbracelet/log"
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

// Init configures a logger that writes to stdout and a rotating file.
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

	w := io.MultiWriter(os.Stdout, rotator)
	level := levelFromEnv()
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
