package logging

import (
    "log/slog"
    "os"
    "sync"
)

var (
	levelVar   = new(slog.LevelVar)
	loggerOnce sync.Once
	baseLogger *slog.Logger
)

func init() {
	levelVar.Set(slog.LevelInfo)
	initLogger()
}

func initLogger() {
	loggerOnce.Do(func() {
		baseLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     levelVar,
			AddSource: false,
		}))
	})
}

func Logger() *slog.Logger {
    initLogger()
    return baseLogger
}

func SetDebug(enabled bool) {
    if enabled {
        levelVar.Set(slog.LevelDebug)
    } else {
        levelVar.Set(slog.LevelInfo)
    }
}
