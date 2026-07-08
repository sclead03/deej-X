package deej

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sclead03/deej-x/pkg/deej/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	buildTypeNone    = ""
	buildTypeDev     = "dev"
	buildTypeDebug   = "debug"
	buildTypeRelease = "release"

	logDirectory = "logs"

	// consoleFieldTruncateLen caps how much of any single string field (e.g. a
	// hex-dumped serial payload) the debug build prints to its console window.
	// The log file always gets the untruncated value.
	consoleFieldTruncateLen = 120
)

// NewLogger provides a logger instance for the whole program
func NewLogger(buildType string) (*zap.SugaredLogger, error) {
	if buildType == buildTypeDebug {
		return newDebugLogger()
	}

	var loggerConfig zap.Config

	ts := time.Now().Format("2006-01-02_15-04-05")

	switch buildType {
	case buildTypeRelease:
		// info and above, log to file only
		if err := util.EnsureDirExists(logDirectory); err != nil {
			return nil, fmt.Errorf("ensure log directory exists: %w", err)
		}
		loggerConfig = zap.NewProductionConfig()
		loggerConfig.OutputPaths = []string{filepath.Join(logDirectory, fmt.Sprintf("deej-%s.log", ts))}
		loggerConfig.Encoding = "console"

	default:
		// development: debug and above, log to stderr, colorful
		loggerConfig = zap.NewDevelopmentConfig()
		loggerConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	// all build types: make it readable
	loggerConfig.EncoderConfig.EncodeCaller = nil
	loggerConfig.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format("2006-01-02 15:04:05.000"))
	}

	loggerConfig.EncoderConfig.EncodeName = func(s string, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(fmt.Sprintf("%-27s", s))
	}

	logger, err := loggerConfig.Build()
	if err != nil {
		return nil, fmt.Errorf("create zap logger: %w", err)
	}

	// no reason not to use the sugared logger - it's fast enough for anything we're gonna do
	sugar := logger.Sugar()

	return sugar, nil
}

// newDebugLogger builds the debug build's logger: debug level and above, to
// both the log file and a console window (no `-H=windowsgui`, so the console
// exists and Ctrl+C in it terminates the session). The two sinks are teed
// together but not identical: the file core gets every field untruncated,
// while the console core truncates long string fields (e.g. the serial
// writer's hex-dumped TX/RX payloads, which can run to over a thousand
// characters for a single icon push) via truncatingCore so the terminal
// doesn't get flooded - the full value is always still in the log file.
func newDebugLogger() (*zap.SugaredLogger, error) {
	if err := util.EnsureDirExists(logDirectory); err != nil {
		return nil, fmt.Errorf("ensure log directory exists: %w", err)
	}

	encoderConfig := zap.NewDevelopmentEncoderConfig()
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	encoderConfig.EncodeCaller = nil
	encoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format("2006-01-02 15:04:05.000"))
	}
	encoderConfig.EncodeName = func(s string, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(fmt.Sprintf("%-27s", s))
	}
	encoder := zapcore.NewConsoleEncoder(encoderConfig)

	ts := time.Now().Format("2006-01-02_15-04-05")
	logPath := filepath.Join(logDirectory, fmt.Sprintf("deej-debug-%s.log", ts))
	fileSink, _, err := zap.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open debug log file: %w", err)
	}

	fileCore := zapcore.NewCore(encoder, fileSink, zapcore.DebugLevel)
	consoleCore := &truncatingCore{
		Core:        zapcore.NewCore(encoder, zapcore.Lock(os.Stderr), zapcore.DebugLevel),
		maxFieldLen: consoleFieldTruncateLen,
	}

	logger := zap.New(zapcore.NewTee(fileCore, consoleCore), zap.Development())

	return logger.Sugar(), nil
}

// truncatingCore wraps another zapcore.Core and truncates long string field
// values before delegating to it - see newDebugLogger.
type truncatingCore struct {
	zapcore.Core
	maxFieldLen int
}

func (c *truncatingCore) With(fields []zapcore.Field) zapcore.Core {
	return &truncatingCore{Core: c.Core.With(c.truncate(fields)), maxFieldLen: c.maxFieldLen}
}

func (c *truncatingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *truncatingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(ent, c.truncate(fields))
}

func (c *truncatingCore) truncate(fields []zapcore.Field) []zapcore.Field {
	out := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		if f.Type == zapcore.StringType && len(f.String) > c.maxFieldLen {
			f.String = fmt.Sprintf("%s...(%d chars total, see log file)", f.String[:c.maxFieldLen], len(f.String))
		}
		out[i] = f
	}
	return out
}
