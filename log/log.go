package log

import (
	"context"

	"github.com/NixM0nk3y/dynamodb-clone/version"
	"go.uber.org/zap"
)

// https://blog.gopheracademy.com/advent-2016/context-logging/
type correlationIDType int

const (
	requestIDKey correlationIDType = iota
	sessionIDKey
)

// Default logger of the system.
var logger *zap.Logger

func init() {

	buildVersion := version.Version
	buildHash := version.BuildHash
	buildDate := version.BuildDate

	defaultLogger, LoggerErr := zap.NewProduction()
	if LoggerErr != nil {
		panic("failed to initilize logger: " + LoggerErr.Error())
	}
	defer defaultLogger.Sync()
	logger = defaultLogger.With(zap.String("v", buildVersion), zap.String("bh", buildHash), zap.String("bd", buildDate))
}

// WithRqID returns a context which knows its request ID
func WithRqID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

// Logger returns a zap logger with as much context as possible
func Logger(ctx context.Context) *zap.Logger {

	newLogger := logger

	if ctx == nil {
		return newLogger
	}

	if ctxRqID, ok := ctx.Value(requestIDKey).(string); ok {
		newLogger = newLogger.With(zap.String("rqID", ctxRqID))
	}

	return newLogger
}
