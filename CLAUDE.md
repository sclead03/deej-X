# deej-x — Project Context (SERENITY)

## Standing Rules for Claude

- **Always build `deej-x.exe` with the exact command in "Building deej-x.exe" below.** Do not improvise `go build`/`ldflags` invocations. Omitting `-H=windowsgui` is a recurring mistake that leaves a visible blank console window open every time the tray app runs — this has happened more than once. If a new ldflag or build step is ever needed, update that section first, then use it from then on.
- **Hardware questions go to the firmware CLAUDE.md.** All hardware details (pin assignments, display wiring, EEPROM layout, encoder behavior, RGB button state, etc.) are documented in `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`. Do not assume hardware details not documented here or there.
- **TBD items are blockers.** Several protocol details are explicitly deferred (icon format, icon dimensions, bitmap bit order, HID report format). Do not guess or assume values for these — check the TBD section first and ask the user if a decision is needed before proceeding.
- **Windows is the primary target.** Linux is best-effort secondary. All features must work on Windows. Linux implementations follow the same OS-abstraction pattern already established in the codebase.
- **This is a personal fork, not an upstream PR.** Do not attempt to maintain compatibility with the vanilla deej serial protocol or configuration format beyond what is documented here.
- **Investigate build/vet errors before calling them pre-existing or unrelated.** See "Build & Verification Gotchas" below — it documents real, root-caused issues (and how to actually verify the Linux build via WSL) found by digging in, not by waving errors away.
- **Check "Testing & Verification Status" before suggesting a test.** Don't propose re-testing a ✅ item unless the current diff touches one of its listed files — that section exists specifically so already-verified functionality doesn't get re-suggested every session.

---

## Testing & Verification Status

Legend: ✅ Verified — don't re-suggest unless listed files changed · ⬜ Not yet verified — fair game to test next · 🚫 Blocked — no hardware or not implemented, don't suggest testing it

### ✅ Verified — retest only if listed files change

- **Master volume mute (Windows, encoder single-click)** — Covered: encoder toggle+restore exact level, external Windows Volume Mixer sync (both directions), rapid-click gesture disambiguation, reconnect/beacon resync while muted, independence from mic mute. Files: `session_windows.go` (`SetMuted`), `session_map.go` (`toggleMasterMuted`), `display.go` (`handleMasterMuteToggleRequest`).
- **Single-device mic mute (Windows)** — Files: `hid_windows.go` (`applyToDevices`, `IsMuted`, `queryCaptureAllMuted`), `display.go` (`handleExternalMicMuteChange`, single-device path only — multi-device path is the open bug below).

### ⬜ Not yet verified

- **Multi-device mic mute** — active bug, see "Active Bugs" below for full debug plan.
- **`enable_logging` config gate** (commit `507603e`) — implemented, not bench-tested: does `false` actually suppress the log file write, does `Fatal`/`Fatalw` still terminate the process.
- **Screensaver disable delay** (commit `4d4212d`) — implemented, not bench-tested on hardware.
- **Linear/log fader curve, dB floor, deadzone config** (commit `b2ee470`) — implemented, not bench-tested.
- **Screensaver hardware verification** (icon redraw/bounce, `CMD_REQUEST_ICON_REDRAW`/`LoadAt()`) — full idle → screensaver → wake cycle not yet exercised.

### 🚫 Blocked — don't suggest testing

- **D16 button gestures** — no D16 hardware on this unit; untestable until a unit with that pin exists.
- **Linux HID enumeration / mic mute** — `openSERENITY()` not implemented.
- **Linux master mute** (`SetMuted`/`toggleMasterMuted` in `session_linux.go`) — not implemented.

---

## Overview

Fork of [omriharel/deej](https://github.com/omriharel/deej) (MIT license), a Go desktop app that reads slider values from a serial-connected microcontroller and maps them to Windows/Linux audio session volumes.

**Module path:** `github.com/sclead03/deej-x`

This fork is customized exclusively for the **SERENITY** hardware: a custom ATmega32U4-based audio mixer with 5 faders, 5 mute buttons/LEDs, a rotary encoder, an RGB LED button, and 6 SSD1306 OLED displays. This fork adds bidirectional communication, OLED icon/name streaming, and system mic mute via HID.

---

## Differences from Upstream deej

| | Upstream deej | This fork |
|---|---|---|
| Baud rate | 9600 | 115200 |
| Serial output | Continuous stream | Event-driven (on change only) |
| Channel count | Variable | 6 (index 0 = master vol encoder, 1–5 = faders) |
| Serial direction | Device → host only | Bidirectional |
| HID handling | None | Mic mute via custom HID report |
| Display support | None | Icon + name streaming to 5 channel OLEDs |

### Serial input format (device → host)

```
masterVol|fader0|fader1|fader2|fader3|fader4\r\n
```

Six pipe-delimited values, 0–1023 each. Index 0 is the master volume encoder; indices 1–5 are the analog faders. The existing `expectedLinePattern` regex in `serial.go` handles variable channel counts and requires no changes. The firmware only transmits on value changes — the host must not assume a regular update rate.

Index 0 maps in `config.yaml` like any other channel (`slider_mapping: 0: master`). The host has no special awareness that it is encoder-sourced vs. ADC-sourced.

### Config keys

| Key | Status | Notes |
|---|---|---|
| `com_port` | ✓ existing | |
| `baud_rate` | ✓ updated | Default changed to 115200 |
| `slider_mapping` | ✓ existing | 6-channel SERENITY layout in default config |
| `channel_names` | ✓ implemented | List of 5 strings for channel OLEDs 1–5 |
| `icon_dir` | ✓ implemented | Directory containing PNG icon files; relative or absolute path |
| `d16_button.single_click` / `.double_click` / `.triple_click` | ✓ implemented | D16 (PF7) button gesture actions — identical single/double/triple-click handling to `encoder_gestures` (same shared click window, same action IDs: `masterVol_mute`, `play_pause`, `skip_forward`, `skip_back`, `mute_mic`, `unmute_mic`). Defaults: single `masterVol_mute`, double `play_pause`, triple `skip_forward`. Pushed on every beacon via `SET_D16_ACTION` (0x0E, 3-byte payload). Replaces the old single-value `d16_button.action` key. |
| `screensaver_timeout_s` | ✓ implemented | Idle time in seconds before SERENITY's OLED screensaver engages. Range 30–1800, default 180. Pushed on every beacon via `SET_SCREENSAVER_TIMEOUT` (0x0F). |
| `newInput_behavior` | pending | `mute` or `unmute`. On startup and whenever a new input device connects, immediately enforce this state on all connected input devices. Not yet implemented. |
| `enable_logging` | ✓ implemented | `true`/`false`, default `false`. Gates whether the **release build** (`deej-x.exe`) writes a log file to `logs/`. Read directly out of `config.yaml` in `main.go` before the logger exists (the full viper-backed config system needs a logger first). When `false`, `NewLogger` returns `zap.NewNop().Sugar()` — every call site is a no-op short-circuited at the `Enabled()` check, so nothing is formatted or written to disk; `Fatal`/`Fatalw` still terminate the process (confirmed via zap source: `WriteThenFatal` fires unconditionally in `CheckedEntry.Write`, independent of the core). Does not affect `deej-x_debug.exe`, which always logs regardless of this setting — see "Building deej-x.exe" below. |

---

## Protocol & Architecture

### Bidirectional Serial

All host → firmware commands use binary framing:

```
[0x00][CMD_ID][LEN_LO][LEN_HI][...payload bytes...]
```

- `0x00` (null byte) is the escape prefix — never appears in ASCII fader data, unambiguous
- `LEN` is payload length in bytes, little-endian 16-bit
- **Fire-and-forget** — no ACK, no retry; USB CDC serial reliability is sufficient

Implemented in `serial_writer.go`. `SerialWriter` is created by `SerialIO` on connect and exposed via `SerialIO.Writer()`. Received frames are parsed by `SerialIO.readFrames()` in `serial.go`, dispatched via `SerialIO.SubscribeToDeviceCommands()`, handled in `display.go`.

**Host → firmware CMD_IDs:**

| Name | CMD_ID | Payload | Description |
|---|---|---|---|
| `CMD_QUERY` | `0x01` | none | Request ready beacon |
| `SET_CHANNEL_NAME` | `0x02` | `[channel_idx][name\0]` | Push display name for channel N |
| `SET_CHANNEL_ICON` | `0x03` | `[channel_idx][bitmap bytes]` | Push icon bitmap for channel N |
| `SET_MASTER_VOLUME` | `0x04` | `[vol_lo][vol_hi]` | Raw 0–1023; host's current master volume |
| `SET_MIC_MUTE_STATE` | `0x05` | `[muted]` | `0x00` unmuted / `0x01` muted |
| `SET_MASTER_MUTE_STATE` | `0x07` | `[muted]` | `0x00` unmuted / `0x01` muted; master output WASAPI mute state |
| `SET_GESTURE_CONFIG` | `0x09` | `[single][double][triple]` | Encoder gesture → action mapping; action IDs: 0=MasterVolMute 1=PlayPause 2=SkipForward 3=SkipBack 4=MicMute 5=MicUnmute. Pushed on every beacon. |
| `SET_CLICK_WINDOW` | `0x0B` | `[ms_lo][ms_hi]` | Click-window duration, uint16 LE, ms — shared by encoder and D16 button gesture disambiguation. Default 250; host enforces 50–1000. Pushed on every beacon. |
| `SET_SLIDER_COUNT` | `0x0C` | `[count]` | Active channel count, uint8, 0–5. Firmware gates all channel interactions to this number. Pushed on every beacon. |
| `SET_DISPLAY_GAP` | `0x0D` | `[gap]` | Inter-display dead pixel count, uint8, 0–100. Firmware stores in EEPROM. Pushed on every beacon. |
| `SET_D16_ACTION` | `0x0E` | `[single][double][triple]` | D16 button (PF7) gesture → action mapping, mirrors SET_GESTURE_CONFIG's payload shape and action IDs. Defaults: MasterVolMute/PlayPause/SkipForward. Pushed on every beacon. |
| `SET_SCREENSAVER_TIMEOUT` | `0x0F` | `[s_lo][s_hi]` | Idle timeout before screensaver engages, uint16 LE, seconds. Default 180; host enforces 30–1800. Persisted to firmware EEPROM. Pushed on every beacon. |

**Firmware → host CMD_IDs:**

| Name | CMD_ID | Payload | Description |
|---|---|---|---|
| `CMD_REQUEST_ICON_REDRAW` | `0x06` | `[channel_idx][x_offset][y_offset]` | Screensaver: re-render icon at bounce offset instead of centered |
| `CMD_REQUEST_MASTER_MUTE_TOGGLE` | `0x08` | none | Encoder single-click: perform OS-level master mute toggle |
| `CMD_REQUEST_MIC_MUTE_ACTION` | `0x0A` | `[desired_state]` | Gesture-mapped mic mute/unmute. `0x00`=mute, `0x01`=unmute. |

The two directions are independent namespaces. CMD_IDs `0x06`, `0x08`, `0x0A` are unassigned host→firmware; `0x09`, `0x0B`, `0x0C`, `0x0D`, `0x0E` are unassigned firmware→host.

### Connection & Push

- **Hotplug (device arrives while host running):** `hotplug_windows.go` — WM_DEVICECHANGE/DBT_DEVICEARRIVAL via message-only window and RegisterDeviceNotificationW (GUID_DEVINTERFACE_COMPORT). 500ms settle delay, then open port.
- **Host launches with device connected:** connect event → CMD_QUERY immediately.
- **Reconnect:** readFrames goroutine closes channel on read error → Start() detects → close() → reconnect() goroutine → waitForSerialDevice() → retry Start().
- **On beacon (`SERENITY\r\n`):** pushMasterState → pushAll.
- **Manual trigger:** "Push display icons" tray item → display.TriggerPush(), skipping unchanged channels.
- **Change tracking:** DisplayManager tracks lastSentNames and lastSentIcons per channel. Connection events force-push all; manual pushes skip unchanged.

### Channel Names

Pushed via SET_CHANNEL_NAME on every connection and manual push. Source: `channel_names` in config.yaml → `CanonicalConfig.ChannelNames [5]string`. `MaxChannelNameLength = 15` (serial_writer.go). Config reload picks up changes on next push.

### Channel Icons

Pushed via SET_CHANNEL_ICON on connection and manual push. Source: PNG files in `icon_dir`, named after the process with `.exe` stripped. `deej.unmapped` → `unmapped.png`; `system` → `system.png`; master slot skipped.

Pipeline (transparent PNG): box-filter resize alpha to 36×36 → use alpha as content mask → threshold.
Pipeline (opaque PNG): box-filter resize RGB to 36×36 → grayscale → threshold → 1-bit.
Output: 768-byte SSD1306 page-order frame; 36×36 icon at given offset (46px horizontal / 6px vertical padding when centered).

Implemented in `pkg/deej/icon/channel_icon.go`: `loadMono()`, `packSSD1306(mono, leftPad, topPad)`, `Load()` (centered), `LoadAt()` (arbitrary offset for screensaver bounce). Missing icons logged at debug level and skipped gracefully. `lastSentIcons` tracks change state; screensaver redraws update it so a later centered push correctly detects the position changed.

### HID Mic Mute

Device: USB VID `0x1209`, PID `0x0001` (composite: CDC serial + HID).

- Pure Go, no CGO: Win32 HID via `setupapi.dll`/`hid.dll` using `syscall.NewLazyDLL`
- Mic-mute report: ID 4, usage page `0xFF00`, usage `1` (`kMicMuteDesc` in firmware)
- SERENITY's HID interface splits into two Windows device paths (COL01 = Consumer Control/Play-Pause; COL02 = vendor mic-mute). VID/PID substring match alone isn't sufficient — `matchesMicMuteCollection()` opens each match and checks usage via `HidD_GetPreparsedData`/`HidP_GetCaps`, skipping non-matching collections.
- `handleReport` filters on `report[0] == micMuteReportID`; ignores Play/Pause (report ID 3).
- After toggle: reads back real state via `IsMuted()` and pushes via `SerialWriter.SendMicMuteState` (SET_MIC_MUTE_STATE/0x05).
- Linux: `openSERENITY` not yet implemented; HID manager retries silently. Mic mute via `pactl`.

**Rules for `withCaptureVolume` closures in `hid_windows.go` — apply to any new COM closure here:**
- Never access the receiver (`m`) or captured state inside the closure — only `aev` and plain locals. Move logging/receiver access to after `withCaptureVolume` returns. Violations cause crashes (empirically confirmed across multiple incidents; exact mechanism not fully understood).
- Always pass `nil` for `SetMute`'s eventContext on the capture endpoint — a real GUID crashes. Self-triggered writes are suppressed via `markMicMuteSetByButton` time window (500ms, `micMuteEchoSuppressWindow`) instead.
- `markMicMuteSetByButton()` must be called **before** `ToggleMute()` — the WASAPI callback can fire synchronously before ToggleMute returns.
- `micMuteNotifyCallback`: copy notification by value, spawn goroutine for all actual work. No COM calls or receiver access inline.

### Master State Sync & Live Tracking

On every beacon (before pushAll), `DisplayManager.pushMasterState` sends:
- `SET_MASTER_VOLUME` (0x04): `getMasterVolume()` × 1023, rounded → uint16 LE
- `SET_MIC_MUTE_STATE` (0x05): `HIDManager.IsMicMuted()`
- `SET_MASTER_MUTE_STATE` (0x07): `sessionMap.getMasterMuted()`

Skips any push where the value isn't available (session map not yet populated).

Live master volume/mute changes are tracked via push-based OS callbacks — **do not change this to polling.**

- **Windows:** `IAudioEndpointVolumeCallback` registered via `registerMasterVolumeChangeCallback` (direct vtable call — go-wca's wrapper is stubbed to E_NOTIMPL). Filtered by `guidEventContext` vs. deej's `eventCtx` GUID. `GetAllSessions()` calls `runtime.LockOSThread()` on entry and never calls `CoUninitialize()` — COM stays initialized for the life of the process.
- **Linux:** PulseAudio `proto.Subscribe{Mask: paSubscriptionMaskSink}` sink events.
- Both satisfy `MasterVolumeWatcher` (`session_finder.go`). `SubscribeToMasterVolumeChanges()` returns `<-chan MasterVolumeNotification{Volume, Muted}`. Volume and mute are pushed independently via separate dedupe state — either can change without the other.
- Channel capacity: capacity-1, latest-value-wins (evict-and-replace) on both the source channel and per-consumer fan-out.
- Settle: 100ms debounce timer in `setupMasterVolumeWatcher`; on expiry re-reads via `getMasterVolume()` and forwards as `MasterVolumeUpdate{ForceSync: true}`, bypassing noise-threshold dedup.

**Slider 0 on connect:** First reading after a slider-count change is silently baselined (no `SliderMoveEvent`) — faders 1–5 still snap to physical position. Slider 0 has no meaningful physical position before the host has synced down the real value.

**Echo suppression (slot 0):** `handleLine` skips `SliderMoveEvent` for slot-0 readings that exactly match `SerialWriter.LastSentMasterVolumeRaw`, or arrive within `masterVolumeSerialEchoWindow` (200ms) of the last SET_MASTER_VOLUME send. The time window (not just exact match) is necessary because multiple pushes can be in flight simultaneously.

Live mic mute: separate `RegisterControlChangeNotify` on `sf.masterIn.volume` (default capture endpoint). `MicMuteWatcher` interface with `SubscribeToMicMuteChanges() <-chan bool`. Windows-only. Pushed via existing SET_MIC_MUTE_STATE (0x05).

---

## Building deej-x.exe

**Canonical command — always use exactly this, do not substitute or drop flags:**

```
go build -ldflags "-H=windowsgui -X main.buildType=release" -o deej-x.exe ./pkg/deej/cmd
```

- `-H=windowsgui`: **required.** Without it, the binary links as a console subsystem app and a blank console window opens every time `deej-x.exe` is launched (it's a tray-only app with no console UI). This has been forgotten more than once — always include it.
- `-X main.buildType=release`: sets the build-time variable in `cmd/main.go` that switches logging to file-only, production mode (see `logger.go`). Without it, `buildType` is empty and the binary runs in dev-logging mode instead.
- Before rebuilding, stop any running `deej-x.exe` process first (file is locked while running).
- For local debugging where you want live stderr output instead of the release log file, use `go run ./pkg/deej/cmd` (optionally with `-v`/`--verbose`) instead of building — don't pass `-H=windowsgui` for that case, since you want the console.

**Debug build (`deej-x_debug.exe`):**

`deej-x_debug.exe` is pre-built and present in the project directory — do not rebuild it at the start of a debug session unless code has changed. To rebuild:

```
go build -ldflags "-X main.buildType=debug" -o deej-x_debug.exe ./pkg/deej/cmd
```

Logs at DEBUG level to both `logs/deej-debug-<timestamp>.log` and a visible console window (this build intentionally omits `-H=windowsgui` so a console exists; Ctrl+C in that window terminates the session). The two sinks aren't identical: `logger.go`'s `newDebugLogger()` tees a file core (untruncated) with a console core wrapped by `truncatingCore`, which caps long string fields (e.g. `serial_writer.go`'s hex-dumped TX/RX payloads — an icon push alone is 1500+ hex chars) at `consoleFieldTruncateLen` (120 chars) so the terminal doesn't get flooded; the log file always has the full value. Controlled by `debug.yaml` in the working directory:
- `run_duration_ms: 0` — run until manually terminated (required for manual test sessions; set to 0 before debugging)
- `run_duration_ms: N` — auto-exit after N ms (useful for quick smoke tests)

## Codebase Structure

```
pkg/deej/
  cmd/main.go                  — entry point
  deej.go                      — main Deej struct, lifecycle
  config.go                    — config loading (viper)
  serial.go                    — serial I/O: readFrames() byte-stream parser (ASCII lines + binary device->host command frames), fader parsing, connect/beacon/device-command events
  serial_writer.go             — host → firmware command framing (binary protocol)
  display.go                   — handshake, master state sync, name push sequencing, change tracking
  hid.go                       — HIDManager, MicMuter interface (toggle + query mute state), read loop
  hid_windows.go               — Win32 HID enumeration + WASAPI mic mute
  hid_linux.go                 — Linux stubs (HID enumeration TBD, mic mute via pactl)
  hotplug_windows.go           — WM_DEVICECHANGE COM port arrival listener (message-only window)
  hotplug_linux.go             — Linux stub (2s delay fallback)
  session.go                   — audio session abstraction
  session_map.go               — slider → session mapping
  session_finder.go            — interface
  session_finder_windows.go
  session_finder_linux.go
  tray.go                      — system tray (includes "Push display icons" item)
  notify.go                    — desktop notifications
  logger.go
  panic.go                     — crash handler
  util/util.go
  util/util_windows.go
  util/util_linux.go
  icon/icon.go                 — tray/notification icon data (generated, do not edit)
  icon/channel_icon.go         — OLED icon pipeline: Load(), box resize, threshold, packSSD1306()
```

---

## Build & Verification Gotchas

Real, root-caused issues found while verifying builds/vet on this machine. Check here before calling an error "pre-existing" or "unrelated" — that determination must be backed by actual investigation, not assumed.

### Cross-compiling `GOOS=linux` from this Windows box will always fail — expected, not a bug

`go build`/`go vet` with `GOOS=linux` run from this Windows host fails with `undefined: nativeLoop` (and similar) inside `github.com/getlantern/systray`. Cause: `systray_linux.go` uses `import "C"` (cgo: GTK3 + libappindicator + webkit2gtk), and cross-compiling from Windows defaults `CGO_ENABLED=0`, so that file is silently skipped while `systray.go` still calls the functions it defines. **This is not a defect in this project's code** and isn't fixable by editing our Go files — don't strip the tray dependency or add build tags to "fix" it. To get a real answer about the Linux build, build it natively (see below) instead of cross-compiling.

### Verifying the Linux build for real: use WSL

This machine has WSL (Ubuntu 24.04) installed, with a real Go toolchain, gcc, and the GTK3/appindicator/webkit2gtk dev headers `systray` needs for its native cgo build. Use it instead of cross-compiling or declaring Linux "unverifiable from here":

```
wsl -d Ubuntu -- bash -lc "cd '/mnt/c/Users/Steven/Documents/Solid Models/deej/Deej-X/Deej-X' && PKG_CONFIG_PATH=\$HOME/pkgconfig-shim go build ./... 2>&1"
wsl -d Ubuntu -- bash -lc "cd '/mnt/c/Users/Steven/Documents/Solid Models/deej/Deej-X/Deej-X' && PKG_CONFIG_PATH=\$HOME/pkgconfig-shim go vet ./... 2>&1"
```

- Ubuntu 24.04 only ships `webkit2gtk-4.1`, but the pinned 2020-era `systray` version hardcodes a `webkit2gtk-4.0` pkg-config lookup. Fixed with a no-sudo shim at `~/pkgconfig-shim/webkit2gtk-4.0.pc` *inside WSL* that redirects to the installed 4.1 package — always pass `PKG_CONFIG_PATH=$HOME/pkgconfig-shim` on build/vet commands there.
- Installing/removing apt packages needs `sudo`, which requires an interactive password Claude doesn't have. Ask the user to run the `apt-get install` command themselves (the `!` prefix, or a one-line `wsl -d Ubuntu -- sudo ...` for a separate cmd/PowerShell window) rather than attempting to bypass this.
- A harmless linker warning (`missing .note.GNU-stack section implies executable stack`) is normal on this cgo build and not a real issue.

### `signal.Notify` requires a buffered channel

`pkg/deej/util/util.go`'s `SetupCloseHandler` uses `make(chan os.Signal, 1)` — `signal.Notify` does a non-blocking send and silently drops signals on unbuffered channels. If `go vet` flags this pattern elsewhere, apply the same fix.

### `go vet`'s `unsafeptr` check on Win32/COM callback structs

When writing a hand-rolled COM callback, declare pointer-typed callback parameters as their real pointer type (e.g. `pNotify *audioVolumeNotificationData`), **not** `uintptr` plus a manual `unsafe.Pointer(uintptr)` cast inside the function body. `syscall.NewCallback` marshals typed pointer arguments directly — see the existing `this *wca.IMMNotificationClient` parameter on `defaultDeviceChangedCallback`. Converting a `uintptr` to `unsafe.Pointer` after the fact is exactly the pattern `go vet`'s `unsafeptr` check flags.

---

## Remaining Work

### Revision Stamping — DEFERRED until final development version

Upstream deej's build scripts stamped `main.gitCommit`/`main.versionTag` via ldflags (`-X main.gitCommit=... -X main.versionTag=...`) and had a `prepare-release.bat` pipeline to tag, build, and stage release binaries under `releases/vX.Y.Z/`. Those scripts were removed 2026-07-08 as dead weight (this fork builds exclusively via the manual command in "Building deej-x.exe" below, and no tagged-release workflow was in use). `main.go` still has the `gitCommit`/`versionTag` vars wired up and logs them if set — only the scripts that populated them are gone.

**TODO:** once this fork reaches a final/stable development version, reintroduce revision stamping (either restore an updated version of the old scripts, matching the canonical `-H=windowsgui -X main.buildType=release` invocation, or fold `-X main.gitCommit=... -X main.versionTag=...` directly into the canonical build command in "Building deej-x.exe"). Not needed while still in active development.

### Master Volume Mute Redesign — Linux remaining

Real WASAPI mute on the master output, mirroring how mic mute works. Windows side implemented and covered by "Testing & Verification Status" ✅:
- `masterSession.SetMuted(bool) error` (`session_windows.go`) — `s.volume.SetMute(muted, s.eventCtx)`. GUID echo-filtering works via existing `masterVolumeNotifyCallback` check; no time-window fallback needed for the output endpoint (contrast with mic mute's 500ms echo-suppress window, needed because the capture-endpoint `SetMute` call can't pass a real GUID context — see "HID Mic Mute" rules).
- `sessionMap.toggleMasterMuted() (bool, error)` (`session_map.go`) — reads current mute, flips it, calls `markMasterVolumeSetByDeej()`.
- `DisplayManager.handleMasterMuteToggleRequest()` (`display.go`) — handler for CMD_REQUEST_MASTER_MUTE_TOGGLE (0x08): calls `toggleMasterMuted()`, pushes result via `SendMasterMuteState`/0x07, updates `lastPushedVolMuted`/`havePushedVolMuted`.

Linux `SetMuted`/`toggleMasterMuted` not yet implemented (`session_linux.go`).

### Global Mic Mute — IMPLEMENTED, BUG UNDER INVESTIGATION

Multi-device mute infrastructure is implemented but nonfunctional in practice (see "Active Bugs").

**Canonical behavior spec (source of truth):**
- `mute.all` must enforce mute on every connected input device, regardless of each device's prior state — no reliance on stored/assumed state.
- `unmute.all` must enforce unmute on every connected input device, regardless of prior state.
- SERENITY shows **muted** only when ALL active input devices are muted.
- SERENITY shows **unmuted** if ANY active input device is unmuted; not all need to be unmuted simultaneously.
- State changes are pushed to SERENITY only when the aggregate (all-muted) status changes. Individual device changes that do not change the aggregate must not generate a push. (Currently `handleExternalMicMuteChange` in `display.go` has no dedup — it pushes on every notification regardless of whether the aggregate actually changed. Needs a last-pushed cache, same pattern as master volume.)
- `newInput_behavior` config key (`mute`/`unmute`, not yet implemented): on startup, apply this state to all currently connected input devices; whenever a new input device connects, immediately apply it.

**What is implemented:**
- `windowsMicMuter.applyToDevices()` (`hid_windows.go`) enumerates all active capture devices via a fresh `IMMDeviceEnumerator` each call and calls `SetMute` unconditionally on each matching target — satisfies the "regardless of previous state" requirement.
- Default `MuteAction`/`UnmuteAction` is `["mute.all"]`/`["unmute.all"]` — sentinels that hit every device.
- `queryCaptureAllMuted()` (`hid_windows.go` + `session_finder_windows.go`) returns true only if every active device is muted — satisfies the aggregate logic. Short-circuits on the first unmuted device: once any device is found unmuted, the aggregate is definitively false and remaining devices are not polled. This is intentional — continuing would be redundant.
- `deviceStateChangedCallback` → `handleDeviceStateChanged()` (`session_finder_windows.go`) fires on device state transitions; rebuilds `captureAevs`, re-registers `micMuteCallback` on all current active devices, pushes updated aggregate to `micMuteChanges`.
- `HIDManager.currentMuted` cache is updated by `handleExternalMicMuteChange()` so the next button press toggles from the correct state.

**What is NOT YET IMPLEMENTED (separate from the bug):**
- `newInput_behavior` config key (startup + hotplug enforcement).
- Aggregate push dedup in `handleExternalMicMuteChange` (individual device changes should not send redundant pushes to SERENITY).
- Do not implement the partial-state icon until "partial" definition is resolved — see firmware CLAUDE.md "RGB button mic mute".

### Linux HID Enumeration

Implement `openSERENITY()` in `hid_linux.go` by enumerating `/dev/hidraw*` and matching VID/PID via `/sys/class/hidraw/<dev>/device/uevent`.

### Screensaver Hardware Verification

`CMD_REQUEST_ICON_REDRAW` / `LoadAt()` / screensaver bounce not yet exercised on real hardware. Needs a bench test of the full idle → screensaver → wake cycle once the firmware side is flashed.

### Process Group Channels (e.g. `deej.steam`) — NOT DESIGNED IN DETAIL

A slider targets a named group of processes (from a separate file) instead of listing them individually. Explicit per-channel `slider_mapping` assignments win over group membership.

Likely plugs into `applyTargetTransform()` in `session_map.go` alongside existing `deej.current`/`deej.unmapped`. Group file format TBD. Icon: `pushAll()` would need a special case to load a representative group icon rather than deriving from `targets[0]`.

### Decouple Icon Selection from Process Name — DISCUSS BEFORE IMPLEMENTING

Currently icon lookup is keyed off `targets[0]` from `slider_mapping` (lowercased, `.exe` stripped) — channel label has no effect. Idea: optional explicit icon key per channel, defaulting to current behavior. Overlaps with process group feature. Open questions: config shape, precedence, ordering relative to process groups.

### Soft Takeover — NOT DESIGNED

Move takeover logic to host (currently firmware-only for per-channel mute) and extend to connect-time: faders 1–5 currently snap-jump on connect; could freeze at app setpoint until the physical fader crosses it. Open: exact protocol changes needed, multi-session slider target resolution, whether this replaces snap-jump outright or is config-gated.

---

## Active Bugs

### Multi-device mic mute nonfunctional

**Symptom:** Mic mute behavior breaks when more than one input device is connected. Original failure mode not yet reproduced — being run down via the layered test plan below, one layer at a time. **Partial characterization found along the way:** hotplug transitions double-fire the device-state-changed callback with a transiently stale device count on the first firing (see `deviceStateChangedCallback` in "Code paths under investigation" below) — hasn't yet produced an observed incorrect aggregate push, but is a live candidate mechanism if one turns up.

**Code paths under investigation** (check git blame if refactored):
- `hid_windows.go:318` `applyToDevices()` — fresh enumerator, iterates active capture devices, pre-checks each device's current mute state and **skips `SetMute` if already correct** (driver-quirk workaround). Logs: `"applyToDevices start"` (L367), `"SetMute skipped, already in desired state"` (L410) or `"SetMute succeeded"` (L415), `"applyToDevices done"` (L429).
- `hid_windows.go:444` `IsMuted()` — delegates to `queryCaptureAllMuted()`; logs `"IsMuted aggregate result"` (L472).
- `hid_windows.go:235` `queryCaptureAllMuted()` — logs `"device count"` (L248); **short-circuits at L302-304** on the first unmuted device, so per-device `GetMute` logs stop there — with 3+ devices, later devices never get logged even though the aggregate value itself is still correct.
- `session_finder_windows.go:766` `registerMicMuteChangeCallback()` — (re)builds `captureAevs` under `captureAevsMu`; logs `"Registered mic mute change callbacks on capture devices"` (L837).
- `session_finder_windows.go:935` `deviceStateChangedCallback()` — **does not inspect `dwNewState`**, fires identically for arrival, disable, and unplug; logs `"Device state changed callback fired"` (L940). **No debounce** on this path (unlike the 100ms-debounced default-device-change callback). **Confirmed (not just plausible):** every single hotplug transition — arrival or removal, no flapping required — fires this callback twice, each firing running its own independent rebuild→query→push cycle through `handleDeviceStateChanged()` below. On removal, the 1st cycle's rebuild can return a transiently stale device count (the removed device hadn't dropped out of enumeration yet) and push an aggregate computed from that stale state; the 2nd cycle's rebuild is correct. Observed pushing the same (correct) value both times so far only because the removed device wasn't the one determining the short-circuit outcome in `queryCaptureAllMuted()` — if it had been, the 1st push could plausibly go out wrong. Root cause of the double-firing itself not yet identified (Windows notifying twice per transition vs. two separate registered callback instances vs. something else) — next investigation step if pursued further.
- `session_finder_windows.go:953` `handleDeviceStateChanged()` — fully rebuilds `captureAevs` (only currently-`DEVICE_STATE_ACTIVE` devices survive); logs `"Rebuilt captureAevs after device state change"` (L1024), then `"Device state changed, pushing updated aggregate"` (L1034). See confirmed double-firing finding above.
- `session_finder_windows.go:886` `micMuteNotifyCallback()` → `session_finder_windows.go:897` `handleMicMuteNotification()` — logs `"Mic mute notify callback fired"` (L898). Checks `micMuteSuppressCheck` **before** querying the aggregate at all — if suppressed, logs `"Mic mute notify: suppressing echo of button press, skipping aggregate query"` (L906) and returns without querying (not just a push-level dedup); otherwise logs `"Mic mute aggregate computed"` (L916).
- `session_map.go:707` `micMuteRecentlySetByButton()` — pure 500ms time window (`micMuteEchoSuppressWindow`, `session_map.go:82`) since the last button-triggered write; not per-device or aggregate-value aware, so it can drop a legitimate second-device notification that lands inside the window.
- `display.go:219` `handleExternalMicMuteChange()` — pushes `SendMicMuteState` and logs `"Pushed live mic mute update"` (L234) unconditionally on every call, regardless of whether the aggregate value actually changed. **Known implementation gap, not a new bug** — don't re-flag it as one.
- No `*_test.go` files exist anywhere in the repo — everything below is manual/hardware-in-the-loop.
- `newInput_behavior` still unimplemented (only referenced in this doc) — out of scope for this bug hunt.

**Layered test plan** (run in order; scope is 2-device hardware for now, Layer 9 deferred until a 3rd device is available):

**Progress: Layers 0–4 done. Layer 5 is next.**

*Layer 0 — Preconditions:* Done. `deej-x_debug.exe` freshly built, `debug.yaml` has `run_duration_ms: 0`. Clear/note `logs/` before each layer so each session's log file is unambiguous. **Gotcha:** the release `deej-x.exe` may already be running in the tray and holding COM9 exclusively — the debug build will fail to connect (`Access is denied`) and immediately self-terminate if so. Kill any running `deej-x.exe` process first (`tasklist //FI "IMAGENAME eq deej-x*"` / `taskkill //PID <pid> //F`) before launching the debug build.

*Layer 1 — Single-device smoke check (not a full retest):* Done. Launched with exactly 1 mic device connected, confirmed `registered=1 total=1` at startup, `queryCaptureAllMuted` aggregate resolved correctly for the single device, full beacon/push sequence completed cleanly. Confirms the current build didn't regress the already-✅-verified single-device path (see "Testing & Verification Status") — don't re-run the full single-device bench suite.

*Layer 2 — Two devices connected before launch (steady-state aggregate):* Done. Both devices connected before startup, `registered=2 total=2` confirmed. Mute press: `applyToDevices start total=2`, one device `SetMute succeeded`, the other `skipped, already in desired state` — `applied=2 total=2` held even with a skip, then `allMuted=true`. Unmute press: both `SetMute succeeded`, `applied=2 total=2`, `allMuted=false`. Both devices always ended in the same state as each other and as the button action.

*Layer 3 — Hotplug arrival (1 → 2 while running):* Done. Started with 1 device, plugged in the 2nd while running. `"Device state changed callback fired"` → `"Rebuilt captureAevs after device state change"` → `"Device state changed, pushing updated aggregate"` all fired and landed on `registered=2`, aggregate correct. **Observed detail:** arrival fired the callback twice (both `newState=1`) before settling at `registered=2` — the first rebuild stayed at `registered=1`. End state was correct, so not a failure, but worth keeping in mind given `deviceStateChangedCallback` has no debounce (see "Code paths under investigation" above, and Layer 4 for the fuller double-push-cycle analysis on the removal side). Mute press afterward correctly targeted `applyToDevices start total=2` (not stale `total=1`) — this was the original bug report's core suspected failure point, and it passed.

*Layer 4 — Hotplug removal (2 → 1 while running):* Done, both starting states. With 2 devices connected and muted, unplugged one: rebuild/push sequence fired (removal isn't filtered out per the `dwNewState` finding above), landed on `registered=1`, `allMuted=true` correctly reflecting the remaining muted device; subsequent button press correctly targeted only the remaining device (`applyToDevices total=1`, `applied=1 total=1`). Repeated starting from both-unmuted: same end result correct, but with a more precise finding — **each hotplug transition (arrival or removal) fires `deviceStateChangedCallback` twice, and each firing runs its own independent rebuild→query→push cycle**, not just a cosmetic double rebuild. On removal specifically: the 1st cycle's rebuild returned a stale `registered=2` (the removed device hadn't dropped out of enumeration yet), queried and short-circuited on the still-present device, and pushed an aggregate computed from stale state; the 2nd cycle's rebuild correctly settled on `registered=1` and pushed again. Both pushes happened to land on the correct value in this run only because the removed device wasn't the one determining the short-circuit outcome — if it had been, the 1st push could plausibly go out wrong. Confirms the no-debounce race noted in "Code paths under investigation" occurs on a routine single hotplug, not just rapid flapping — keep this in mind for Layer 8. Subsequent button press again correctly targeted only the remaining device (`total=1`, `applied=1 total=1`).

*Layer 5 — Mixed starting state at button press:* **Setup:** exactly 2 input devices must be physically connected. `deej-x_debug.exe` must be running; if it isn't, launch it per Layer 0's preconditions (check for a release `deej-x.exe` holding COM9 first). Confirm `registered=2 total=2` at startup/reconnect before proceeding. **Test:** before pressing the SERENITY button, use Windows Sound settings to put the two devices into different mute states (one muted, one not — either device can be in either state). Press the button: confirm `mute.all`/`unmute.all` forces both to the same target state, and specifically confirm the skip-if-already-correct path doesn't drop the skipped device from the applied count — `applied=2 total=2` should hold even when one device needed no change.

*Layer 6 — External per-device changes via OS Sound settings:* **Setup:** same as Layer 5 (2 devices physically connected, `deej-x_debug.exe` running, `registered=2 total=2` confirmed — see Layer 0 for launch preconditions if the exe isn't already running). **Test:** mute/unmute one device at a time from Windows' native UI (not the SERENITY button). Confirm the aggregate *value* SERENITY ends up showing is always correct (muted only when both are muted). **Known gap, not a failure:** expect a push (`"Pushed live mic mute update"`) on every individual device change even when the aggregate didn't change — log the push count for later comparison once dedup is implemented, but don't treat the redundant pushes as this bug.

*Layer 7 — Suppression window interaction (best-effort/exploratory):* **Setup:** same as Layer 5 (2 devices connected, exe running, `registered=2 total=2` confirmed). **Test:** press the SERENITY mute button, then within 500ms externally change the *other* device via Windows Sound settings. Watch for `"suppressing echo of button press, skipping aggregate query"` and check whether the second device's legitimate external change gets dropped. Hard to hit deterministically — a few attempts are enough to characterize, not exhaustively reproduce.

*Layer 8 — Rapid flap / race exploration (best-effort/exploratory):* **Setup:** same as Layer 5 (2 devices connected, exe running, `registered=2 total=2` confirmed). Given the confirmed double-firing finding from Layer 4, expect to see it here too. **Test:** quickly unplug and replug one device (within a couple seconds). No debounce exists on `handleDeviceStateChanged`, so this is the most likely place to catch a genuine race (`captureAevs` rebuilding against a device the OS hasn't finished re-enumerating). Note any `total=` count that looks wrong, or any crash/panic.

*Layer 9 — DEFERRED: 3+ device aggregate:* Requires a 3rd input device (not currently available); run once Layers 2–8 are fully resolved. **Setup when a 3rd device is available:** all 3 physically connected, exe running, `registered=3 total=3` confirmed. **Test:** specifically exercises the `queryCaptureAllMuted` short-circuit: with 3 devices where the 2nd is unmuted, confirm the log goes dark for the 3rd device (expected, not a bug) while `allMuted=false` is still correct.

**After each layer passes:** move it into "Testing & Verification Status" ✅ with the files it covers (per the verification-scope memory), so future sessions don't re-suggest retesting it. Any layer that reveals a genuine failure replaces the Symptom line above with a precise, reproduced failure mode.

---

## OS Support

| Feature | Windows | Linux |
|---|---|---|
| Fader/serial reading | ✓ | ✓ |
| Bidirectional serial | ✓ | ✓ |
| Channel name streaming | ✓ | ✓ |
| Icon streaming | ✓ | ✓ |
| Serial hotplug (device arrives after host) | ✓ (WM_DEVICECHANGE) | fallback (2s retry) |
| Serial reconnect (device unplugged/replugged) | ✓ | ✓ |
| HID device reading | ✓ | stub (retries silently) |
| Mic mute toggle | ✓ (WASAPI) | best-effort (pactl) |
| Master volume / mic mute state query (for connect sync) | ✓ (WASAPI) | ✓ (pactl) |

---

## Icon Protocol — Decided

| Item | Decision |
|---|---|
| Source file format | PNG, any resolution — host resizes at runtime |
| File naming | Process name from `slider_mapping` with `.exe` stripped — `firefox.png`, `spotify.png` |
| Displayed icon size | 36×36 pixels (reduced from 48×48 to leave bounce room for screensaver) |
| Wire format | 768 bytes — full 128×48 blue area in SSD1306 page order; icon at a given offset (46px horizontal / 6px vertical padding when centered) |
| Bit order | SSD1306 native: each byte = one column of 8 vertical pixels; bit 0 = topmost pixel of page |
| Conversion | Always threshold (dithering removed) |
| `master` slot (index 0) | Skip icon push — master OLED is encoder-controlled, not a channel display |
| `deej.unmapped` slot | Use bundled default icon; user can override with `unmapped.png` in `icon_dir` |
| `system` slot | Use bundled default icon; user can override with `system.png` in `icon_dir` |

**TODO:** Design and bundle default icons for `deej.unmapped` and `system` slots.

## TBD — Do Not Assume These

| Item | Blocked on |
|---|---|
| *(none currently)* | |

---

## Reference

- Firmware repo and hardware ground truth: `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`
- Upstream deej: https://github.com/omriharel/deej
