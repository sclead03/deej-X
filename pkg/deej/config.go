package deej

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"

	"github.com/sclead03/deej-x/pkg/deej/util"
)

// Encoder button gesture action IDs (firmware SET_GESTURE_CONFIG payload values).
// Actions 0–3 are executed by the firmware directly (0 triggers CMD_REQUEST_MASTER_MUTE_TOGGLE
// back to host; 1–3 are sent as Consumer Control HID reports). Actions 4–5 require
// firmware support for a new CMD_REQUEST_MIC_MUTE_ACTION (0x0A) device→host command —
// see display.go. Firmware must be updated to use action IDs 4–5.
const (
	GestureActionMasterMute  byte = 0
	GestureActionPlayPause   byte = 1
	GestureActionSkipForward byte = 2
	GestureActionSkipBack    byte = 3
	GestureActionMicMute     byte = 4
	GestureActionMicUnmute   byte = 5
)

var gestureActionByName = map[string]byte{
	"masterVol_mute": GestureActionMasterMute,
	"play_pause":   GestureActionPlayPause,
	"skip_forward": GestureActionSkipForward,
	"skip_back":    GestureActionSkipBack,
	"mute_mic":   GestureActionMicMute,
	"unmute_mic": GestureActionMicUnmute,
}

// GestureConfig maps each encoder button gesture to a firmware action ID.
type GestureConfig struct {
	SingleClick byte
	DoubleClick byte
	TripleClick byte
}

// MicMuteConfig specifies which capture devices are affected by mute/unmute button presses.
// Targets are case-insensitive friendly-name substrings (e.g. "Razer Microphone") or the
// sentinels micMuteSentinelAll ("mute.all") / micUnmuteSentinelAll ("unmute.all").
type MicMuteConfig struct {
	MuteAction   []string
	UnmuteAction []string
}

// CanonicalConfig provides application-wide access to configuration fields,
// as well as loading/file watching logic for deej's configuration file
type CanonicalConfig struct {
	SliderMapping *sliderMap

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	InvertSliders       bool
	NoiseReductionLevel string
	NumSliders          int
	DisplayGapPixels    int
	FaderOrder          []int
	MasterLabel         string
	ChannelNames        [numChannels]string
	IconDir             string
	ProcessGroups         map[string][]string
	MicMute               MicMuteConfig
	Gestures              GestureConfig
	RGBButtonAction       string
	D16ButtonAction       byte
	EncoderClickWindowMs  int

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	userConfig     *viper.Viper
	internalConfig *viper.Viper
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	userConfigName     = "config"
	internalConfigName = "preferences"

	userConfigPath = "."

	configType = "yaml"

	configKeySliderMapping       = "slider_mapping"
	configKeyInvertSliders       = "invert_sliders"
	configKeyCOMPort             = "com_port"
	configKeyBaudRate            = "baud_rate"
	configKeyNoiseReductionLevel = "noise_reduction"
	configKeyFaderOrder        = "fader_order"
	configKeyChannelNames      = "channel_names"
	configKeyIconDir           = "icon_dir"
	configKeyMicMuteMuteAction     = "mic_mute.mute_action"
	configKeyMicMuteUnmuteAct      = "mic_mute.unmute_action"
	configKeyGestureSingleClick    = "encoder_gestures.single_click"
	configKeyGestureDoubleClick    = "encoder_gestures.double_click"
	configKeyGestureTripleClick    = "encoder_gestures.triple_click"
	configKeyEncoderClickWindow    = "encoder_click_window_ms"
	configKeyRGBButtonAction       = "rgb_button.action"
	configKeyD16ButtonAction       = "d16_button.action"
	configKeyNumSliders            = "num_sliders"
	configKeyDisplayGapPixels      = "display_gap_pixels"

	groupsDir = "groups"

	defaultNumSliders       = 5
	minNumSliders           = 0
	maxNumSliders           = 5
	defaultDisplayGapPixels = 0
	minDisplayGapPixels     = 0
	maxDisplayGapPixels     = 100

	defaultEncoderClickWindowMs = 250
	minEncoderClickWindowMs     = 50
	maxEncoderClickWindowMs     = 1000

	// rgbButtonActionDefault is the default RGB button action — toggle mic mute,
	// preserving the original hardcoded behaviour.
	rgbButtonActionDefault = "mic_mute_toggle"

	defaultCOMPort  = "COM4"
	defaultBaudRate = 115200
	defaultIconDir  = "icons"
)

// has to be defined as a non-constant because we're using path.Join
var internalConfigPath = path.Join(".", logDirectory)

var defaultSliderMapping = func() *sliderMap {
	emptyMap := newSliderMap()
	emptyMap.set(0, []string{masterSessionName})

	return emptyMap
}()

// NewConfig creates a config instance for the deej object and sets up viper instances for deej's config files
func NewConfig(logger *zap.SugaredLogger, notifier Notifier) (*CanonicalConfig, error) {
	logger = logger.Named("config")

	cc := &CanonicalConfig{
		logger:             logger,
		notifier:           notifier,
		reloadConsumers:    []chan bool{},
		stopWatcherChannel: make(chan bool),
	}

	// distinguish between the user-provided config (config.yaml) and the internal config (logs/preferences.yaml)
	userConfig := viper.New()
	userConfig.SetConfigName(userConfigName)
	userConfig.SetConfigType(configType)
	userConfig.AddConfigPath(userConfigPath)

	userConfig.SetDefault(configKeySliderMapping, map[string][]string{})
	userConfig.SetDefault(configKeyInvertSliders, false)
	userConfig.SetDefault(configKeyCOMPort, defaultCOMPort)
	userConfig.SetDefault(configKeyBaudRate, defaultBaudRate)
	userConfig.SetDefault(configKeyIconDir, defaultIconDir)

	internalConfig := viper.New()
	internalConfig.SetConfigName(internalConfigName)
	internalConfig.SetConfigType(configType)
	internalConfig.AddConfigPath(internalConfigPath)

	cc.userConfig = userConfig
	cc.internalConfig = internalConfig

	logger.Debug("Created config instance")

	return cc, nil
}

// Load reads deej's config files from disk and tries to parse them
func (cc *CanonicalConfig) Load() error {
	cc.logger.Debugw("Loading config", "path", userConfigFilepath)

	// make sure it exists
	if !util.FileExists(userConfigFilepath) {
		cc.logger.Warnw("Config file not found", "path", userConfigFilepath)
		cc.notifier.Notify("Can't find configuration!",
			fmt.Sprintf("%s must be in the same directory as deej. Please re-launch", userConfigFilepath))

		return fmt.Errorf("config file doesn't exist: %s", userConfigFilepath)
	}

	// load the user config
	if err := cc.userConfig.ReadInConfig(); err != nil {
		cc.logger.Warnw("Viper failed to read user config", "error", err)

		// if the error is yaml-format-related, show a sensible error. otherwise, show 'em to the logs
		if strings.Contains(err.Error(), "yaml:") {
			cc.notifier.Notify("Invalid configuration!",
				fmt.Sprintf("Please make sure %s is in a valid YAML format.", userConfigFilepath))
		} else {
			cc.notifier.Notify("Error loading configuration!", "Please check deej's logs for more details.")
		}

		return fmt.Errorf("read user config: %w", err)
	}

	// load the internal config - this doesn't have to exist, so it can error
	if err := cc.internalConfig.ReadInConfig(); err != nil {
		cc.logger.Debugw("Viper failed to read internal config", "error", err, "reminder", "this is fine")
	}

	// canonize the configuration with viper's helpers
	if err := cc.populateFromVipers(); err != nil {
		cc.logger.Warnw("Failed to populate config fields", "error", err)
		return fmt.Errorf("populate config fields: %w", err)
	}

	cc.loadProcessGroups()

	cc.logger.Info("Loaded config successfully")
	cc.logger.Infow("Config values",
		"sliderMapping", cc.SliderMapping,
		"connectionInfo", cc.ConnectionInfo,
		"invertSliders", cc.InvertSliders,
		"masterLabel", cc.MasterLabel,
		"channelNames", cc.ChannelNames,
		"iconDir", cc.IconDir,
		"processGroups", cc.ProcessGroups)

	return nil
}

// SubscribeToChanges allows external components to receive updates when the config is reloaded
func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

// WatchConfigFileChanges starts watching for configuration file changes
// and attempts reloading the config when they happen
func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)

	const (
		minTimeBetweenReloadAttempts = time.Millisecond * 500
		delayBetweenEventAndReload   = time.Millisecond * 50
	)

	lastAttemptedReload := time.Now()

	// establish watch using viper as opposed to doing it ourselves, though our internal cooldown is still required
	cc.userConfig.WatchConfig()
	cc.userConfig.OnConfigChange(func(event fsnotify.Event) {

		// when we get a write event...
		if event.Op&fsnotify.Write == fsnotify.Write {

			now := time.Now()

			// ... check if it's not a duplicate (many editors will write to a file twice)
			if lastAttemptedReload.Add(minTimeBetweenReloadAttempts).Before(now) {

				// and attempt reload if appropriate
				cc.logger.Debugw("Config file modified, attempting reload", "event", event)

				// wait a bit to let the editor actually flush the new file contents to disk
				<-time.After(delayBetweenEventAndReload)

				if err := cc.Load(); err != nil {
					cc.logger.Warnw("Failed to reload config file", "error", err)
				} else {
					cc.logger.Info("Reloaded config successfully")
					cc.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")

					cc.onConfigReloaded()
				}

				// don't forget to update the time
				lastAttemptedReload = now
			}
		}
	})

	// wait till they stop us
	<-cc.stopWatcherChannel
	cc.logger.Debug("Stopping user config file watcher")
	cc.userConfig.OnConfigChange(nil)
}

// StopWatchingConfigFile signals our filesystem watcher to stop
func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) populateFromVipers() error {

	// merge the slider mappings from the user and internal configs
	cc.SliderMapping = sliderMapFromConfigs(
		cc.userConfig.GetStringMapStringSlice(configKeySliderMapping),
		cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping),
	)

	// get the rest of the config fields - viper saves us a lot of effort here
	cc.ConnectionInfo.COMPort = cc.userConfig.GetString(configKeyCOMPort)

	cc.ConnectionInfo.BaudRate = cc.userConfig.GetInt(configKeyBaudRate)
	if cc.ConnectionInfo.BaudRate <= 0 {
		cc.logger.Warnw("Invalid baud rate specified, using default value",
			"key", configKeyBaudRate,
			"invalidValue", cc.ConnectionInfo.BaudRate,
			"defaultValue", defaultBaudRate)

		cc.ConnectionInfo.BaudRate = defaultBaudRate
	}

	cc.InvertSliders = cc.userConfig.GetBool(configKeyInvertSliders)
	cc.NoiseReductionLevel = cc.userConfig.GetString(configKeyNoiseReductionLevel)

	var numSliders int
	if !cc.userConfig.IsSet(configKeyNumSliders) {
		numSliders = defaultNumSliders
	} else {
		numSliders = cc.userConfig.GetInt(configKeyNumSliders)
		if numSliders < minNumSliders || numSliders > maxNumSliders {
			cc.logger.Warnw("num_sliders out of range, using default",
				"value", numSliders,
				"min", minNumSliders,
				"max", maxNumSliders,
				"default", defaultNumSliders)
			numSliders = defaultNumSliders
		}
	}
	cc.NumSliders = numSliders

	displayGap := cc.userConfig.GetInt(configKeyDisplayGapPixels)
	if displayGap < minDisplayGapPixels || displayGap > maxDisplayGapPixels {
		cc.logger.Warnw("display_gap_pixels out of range, using default",
			"value", displayGap,
			"min", minDisplayGapPixels,
			"max", maxDisplayGapPixels,
			"default", defaultDisplayGapPixels)
		displayGap = defaultDisplayGapPixels
	}
	cc.DisplayGapPixels = displayGap

	rawOrder := cc.userConfig.GetIntSlice(configKeyFaderOrder)
	if len(rawOrder) > 0 {
		seen := make(map[int]bool, len(rawOrder))
		valid := true
		for _, v := range rawOrder {
			if v < 0 || v >= len(rawOrder) || seen[v] {
				valid = false
				break
			}
			seen[v] = true
		}
		if valid {
			cc.FaderOrder = rawOrder
		} else {
			cc.logger.Warnw("fader_order is not a valid permutation, using physical order", "value", rawOrder)
			cc.FaderOrder = nil
		}
	} else {
		cc.FaderOrder = nil
	}

	// channel_names is a map: key "0" = master OLED label, keys "1"–"5" = fader OLED names
	namesMap := cc.userConfig.GetStringMapString(configKeyChannelNames)
	if label, ok := namesMap["0"]; ok && label != "" {
		cc.MasterLabel = label
	} else {
		cc.MasterLabel = "MASTER"
	}
	cc.ChannelNames = [numChannels]string{}
	for i := 1; i <= numChannels; i++ {
		if name, ok := namesMap[strconv.Itoa(i)]; ok {
			cc.ChannelNames[i-1] = name
		}
	}

	cc.IconDir = cc.userConfig.GetString(configKeyIconDir)

	cc.MicMute.MuteAction = cc.userConfig.GetStringSlice(configKeyMicMuteMuteAction)
	if len(cc.MicMute.MuteAction) == 0 {
		cc.MicMute.MuteAction = []string{micMuteSentinelAll}
	}
	cc.MicMute.UnmuteAction = cc.userConfig.GetStringSlice(configKeyMicMuteUnmuteAct)
	if len(cc.MicMute.UnmuteAction) == 0 {
		cc.MicMute.UnmuteAction = []string{micUnmuteSentinelAll}
	}

	cc.Gestures.SingleClick = parseGestureAction(cc.userConfig.GetString(configKeyGestureSingleClick), GestureActionMasterMute, cc.logger)
	cc.Gestures.DoubleClick = parseGestureAction(cc.userConfig.GetString(configKeyGestureDoubleClick), GestureActionPlayPause, cc.logger)
	cc.Gestures.TripleClick = parseGestureAction(cc.userConfig.GetString(configKeyGestureTripleClick), GestureActionSkipForward, cc.logger)
	cc.D16ButtonAction = parseGestureAction(cc.userConfig.GetString(configKeyD16ButtonAction), GestureActionMasterMute, cc.logger)

	clickWindow := cc.userConfig.GetInt(configKeyEncoderClickWindow)
	if clickWindow == 0 {
		clickWindow = defaultEncoderClickWindowMs
	} else if clickWindow < minEncoderClickWindowMs || clickWindow > maxEncoderClickWindowMs {
		cc.logger.Warnw("encoder_click_window_ms out of range, using default",
			"value", clickWindow,
			"min", minEncoderClickWindowMs,
			"max", maxEncoderClickWindowMs,
			"default", defaultEncoderClickWindowMs)
		clickWindow = defaultEncoderClickWindowMs
	}
	cc.EncoderClickWindowMs = clickWindow

	action := strings.ToLower(strings.TrimSpace(cc.userConfig.GetString(configKeyRGBButtonAction)))
	if action == "" {
		action = rgbButtonActionDefault
	}
	switch action {
	case "mic_mute_toggle", "mute_mic", "unmute_mic", "masterVol_mute":
		cc.RGBButtonAction = action
	default:
		cc.logger.Warnw("Unknown rgb_button action, using default", "action", action)
		cc.RGBButtonAction = rgbButtonActionDefault
	}

	cc.logger.Debug("Populated config fields from vipers")

	return nil
}

func parseGestureAction(name string, defaultAction byte, logger *zap.SugaredLogger) byte {
	if name == "" {
		return defaultAction
	}
	if action, ok := gestureActionByName[strings.ToLower(name)]; ok {
		return action
	}
	logger.Warnw("Unknown gesture action, using default", "name", name)
	return defaultAction
}

// loadProcessGroups scans the groups/ directory and parses each *.yaml file as a
// flat list of process names. The filename (without .yaml) becomes the group name,
// usable as deej.<name> in slider_mapping. A missing groups/ directory is not an error.
func (cc *CanonicalConfig) loadProcessGroups() {
	cc.ProcessGroups = make(map[string][]string)

	entries, err := os.ReadDir(groupsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
			continue
		}

		groupName := strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".yaml"))

		data, err := os.ReadFile(filepath.Join(groupsDir, name))
		if err != nil {
			cc.logger.Warnw("Failed to read process group file", "file", name, "error", err)
			continue
		}

		var processes []string
		if err := yaml.Unmarshal(data, &processes); err != nil {
			cc.logger.Warnw("Failed to parse process group file", "file", name, "error", err)
			continue
		}

		for i, p := range processes {
			processes[i] = strings.ToLower(p)
		}

		cc.ProcessGroups[groupName] = processes
		cc.logger.Debugw("Loaded process group", "group", groupName, "members", processes)
	}
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}
