// Package logger configures the process-wide structured logger.
package logger

import (
	"log/slog"
	"os"
)

// New returns a JSON structured logger. Using slog (stdlib since Go 1.21)
// keeps this dependency-free while still giving us leveled, structured logs
// suitable for shipping to any log aggregator.
func New(env string) *slog.Logger {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: env == "development",
	})

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
