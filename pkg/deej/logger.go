package deej

import (
	"fmt"
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
)

// NewLogger provides a logger instance for the whole program
func NewLogger(buildType string) (*zap.SugaredLogger, error) {
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

	case buildTypeDebug:
		// debug and above, log to file (no color — file-safe)
		if err := util.EnsureDirExists(logDirectory); err != nil {
			return nil, fmt.Errorf("ensure log directory exists: %w", err)
		}
		loggerConfig = zap.NewDevelopmentConfig()
		loggerConfig.OutputPaths = []string{filepath.Join(logDirectory, fmt.Sprintf("deej-debug-%s.log", ts))}
		loggerConfig.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

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
