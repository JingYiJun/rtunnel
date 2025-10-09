package logging

import (
	"context"
	"fmt"
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

func With(args ...any) *slog.Logger {
	return Logger().With(args...)
}

func SetDebug(enabled bool) {
	if enabled {
		levelVar.Set(slog.LevelDebug)
	} else {
		levelVar.Set(slog.LevelInfo)
	}
}

func Debugf(format string, args ...any) {
	log := Logger()
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.Debug(fmt.Sprintf(format, args...))
	}
}

func Infof(format string, args ...any) {
	Logger().Info(fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	Logger().Warn(fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	Logger().Error(fmt.Sprintf(format, args...))
}
