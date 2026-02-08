package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	charmlog "github.com/charmbracelet/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	mu     sync.RWMutex
	logger = charmlog.NewWithOptions(os.Stdout, charmlog.Options{
		Level:           charmlog.InfoLevel,
		ReportTimestamp: true,
		TimeFormat:      "2006-01-02 15:04:05",
	})
)

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
	l := charmlog.NewWithOptions(w, charmlog.Options{
		Level:           charmlog.InfoLevel,
		ReportTimestamp: true,
		TimeFormat:      "2006-01-02 15:04:05",
	})

	mu.Lock()
	logger = l
	mu.Unlock()

	logger.Info("logging initialized", "path", logPath)
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
