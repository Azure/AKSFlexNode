package bootstrapper

import (
	"log/slog"

	"github.com/sirupsen/logrus"
)

// toLogrus returns the logrus logger associated with the slog.Logger, or
// creates a new default logrus.Logger. This bridges the gap between the
// shared agent library (slog) and existing FlexNode code (logrus).
//
// TODO: migrate FlexNode to slog throughout, then remove this adapter.
func toLogrus(log *slog.Logger) *logrus.Logger {
	_ = log // not used yet; we return the standard logrus instance
	return logrus.StandardLogger()
}
