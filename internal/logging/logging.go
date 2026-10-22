// Package logging builds the agent's logger: readable text for journald by default, or the
// control plane's JSON line shape (docs/logging.md) with LOG_FORMAT=json.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns the logger for the given LOG_FORMAT and LOG_LEVEL values.
func New(w io.Writer, format, level, version string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if !strings.EqualFold(format, "json") {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	// Lowercase levels, the same keys the Node services write.
	opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.LevelKey {
			return slog.String(slog.LevelKey, strings.ToLower(a.Value.String()))
		}
		if a.Value.Kind() == slog.KindAny {
			if err, ok := a.Value.Any().(error); ok {
				return slog.String(a.Key, err.Error())
			}
		}
		return a
	}
	return slog.New(slog.NewJSONHandler(w, opts)).With("service", "agent", "version", version)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug", "trace":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
