package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sclead03/deej-x/pkg/deej"
	"gopkg.in/yaml.v2"
)

var (
	gitCommit  string
	versionTag string
	buildType  string

	verbose bool
)

func init() {
	flag.BoolVar(&verbose, "verbose", false, "show verbose logs (useful for debugging serial)")
	flag.BoolVar(&verbose, "v", false, "shorthand for --verbose")
	flag.Parse()
}

func main() {

	// first we need a logger
	logger, err := deej.NewLogger(buildType)
	if err != nil {
		panic(fmt.Sprintf("Failed to create logger: %v", err))
	}

	named := logger.Named("main")
	named.Debug("Created logger")

	named.Infow("Version info",
		"gitCommit", gitCommit,
		"versionTag", versionTag,
		"buildType", buildType)

	// provide a fair warning if the user's running in verbose mode
	if verbose {
		named.Debug("Verbose flag provided, all log messages will be shown")
	}

	// create the deej instance
	d, err := deej.NewDeej(logger, verbose)
	if err != nil {
		named.Fatalw("Failed to create deej object", "error", err)
	}

	// if injected by build process, set version info to show up in the tray
	if buildType != "" && (versionTag != "" || gitCommit != "") {
		identifier := gitCommit
		if versionTag != "" {
			identifier = versionTag
		}

		versionString := fmt.Sprintf("Version %s-%s", buildType, identifier)
		d.SetVersion(versionString)
	}

	// Debug builds: read debug.yaml for run duration, default 100ms.
	// run_duration_ms: 0 means run until manually terminated.
	if buildType == "debug" {
		runMs := 100
		type debugConfig struct {
			RunDurationMs int `yaml:"run_duration_ms"`
		}
		if data, err := os.ReadFile("debug.yaml"); err == nil {
			var cfg debugConfig
			if err := yaml.Unmarshal(data, &cfg); err == nil {
				runMs = cfg.RunDurationMs
			} else {
				named.Warnw("Failed to parse debug.yaml, using default 100ms", "error", err)
			}
		}
		if runMs > 0 {
			go func() {
				time.Sleep(time.Duration(runMs) * time.Millisecond)
				named.Infof("Debug build: auto-exiting after %dms", runMs)
				logger.Sync()
				os.Exit(0)
			}()
		} else {
			named.Info("Debug build: run_duration_ms=0, running until terminated")
		}
	}

	// onwards, to glory
	if err = d.Initialize(); err != nil {
		named.Fatalw("Failed to initialize deej", "error", err)
	}
}
