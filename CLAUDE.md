# deej-x — Project Context (SERENITY)

## Standing Rules for Claude

- **Always build `deej-x.exe` with the exact command in "Building deej-x.exe" below.** Do not improvise `go build`/`ldflags` invocations. Omitting `-H=windowsgui` is a recurring mistake that leaves a visible blank console window open every time the tray app runs — this has happened more than once. If a new ldflag or build step is ever needed, update that section first, then use it from then on.
- **Hardware questions go to the firmware CLAUDE.md.** All hardware details (pin assignments, display wiring, EEPROM layout, encoder behavior, RGB button state, etc.) are documented in `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`. Do not assume hardware details not documented here or there.
- **TBD items are blockers.** Several protocol details are explicitly deferred (icon format, icon dimensions, bitmap bit order, HID report format). Do not guess or assume values for these — check the TBD section first and ask the user if a decision is needed before proceeding.
- **Windows is the primary target.** Linux is best-effort secondary. All features must work on Windows. Linux implementations follow the same OS-abstraction pattern already established in the codebase.
- **This is a personal fork, not an upstream PR.** Do not attempt to maintain compatibility with the vanilla deej serial protocol or configuration format beyond what is documented here.
- **Investigate build/vet errors before calling them pre-existing or unrelated.** See "Build & Verification Gotchas" below — it documents real, root-caused issues (and how to actually verify the Linux build via WSL) found by digging in, not by waving errors away.

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
| `icon_conversion` | pending removal | See "Remove Dithering Support" in Remaining Work |

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
| `SET_CLICK_WINDOW` | `0x0B` | `[ms_lo][ms_hi]` | Encoder click-window duration, uint16 LE, ms. Default 250; host enforces 50–1000. Pushed on every beacon. |

**Firmware → host CMD_IDs:**

| Name | CMD_ID | Payload | Description |
|---|---|---|---|
| `CMD_REQUEST_ICON_REDRAW` | `0x06` | `[channel_idx][x_offset][y_offset]` | Screensaver: re-render icon at bounce offset instead of centered |
| `CMD_REQUEST_MASTER_MUTE_TOGGLE` | `0x08` | none | Encoder single-click: perform OS-level master mute toggle |
| `CMD_REQUEST_MIC_MUTE_ACTION` | `0x0A` | `[desired_state]` | Gesture-mapped mic mute/unmute. `0x00`=mute, `0x01`=unmute. Not yet implemented in firmware. |

The two directions are independent namespaces. CMD_IDs `0x06`, `0x08`, `0x0A` are unassigned host→firmware; `0x09`, `0x0B` are unassigned firmware→host.

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
  icon/channel_icon.go         — OLED icon pipeline: Load(), box resize, threshold/dither, packSSD1306()
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

### Master Volume Mute Redesign (Windows) — compiles clean, NOT YET bench-tested

Real WASAPI mute on the master output, mirroring how mic mute works. Implemented:
- `masterSession.SetMuted(bool) error` (`session_windows.go`) — `s.volume.SetMute(muted, s.eventCtx)`. GUID echo-filtering works via existing `masterVolumeNotifyCallback` check; no time-window fallback needed for the output endpoint.
- `sessionMap.toggleMasterMuted() (bool, error)` (`session_map.go`) — reads current mute, flips it, calls `markMasterVolumeSetByDeej()`.
- `DisplayManager.handleMasterMuteToggleRequest()` (`display.go`) — handler for CMD_REQUEST_MASTER_MUTE_TOGGLE (0x08): calls `toggleMasterMuted()`, pushes result via `SendMasterMuteState`/0x07, updates `lastPushedVolMuted`/`havePushedVolMuted`.
- Linux `SetMuted`/`toggleMasterMuted` not yet implemented (`session_linux.go`).

**Still needed:** bench verify encoder mute/unmute restores the real level, external Windows-mixer mute/unmute still works, and rapid repeated clicks don't desync `volMuted` from WASAPI state.

### Global Mic Mute (mute all inputs, unmute one) — NOT DESIGNED

Current mic mute only touches the OS default capture device. Desired behavior:
- **Mute** → every active input device (`IMMDeviceEnumerator.EnumAudioEndpoints(ECapture, DEVICE_STATE_ACTIVE, ...)`)
- **Unmute** → one specific configured device by friendly name only (asymmetric by design)
- Host must track an explicit "intended" state so hotplugged devices inherit it
- **Do not implement the partial-state icon** (mic+slash+exclamation) until the definition of "partial" is resolved — see firmware CLAUDE.md "RGB button mic mute"
- Config shape (proposed, not finalized):
  ```yaml
  mic_mute:
    mute_target: input.global
    unmute_target: "USB Microphone"
  ```

### Linux HID Enumeration

Implement `openSERENITY()` in `hid_linux.go` by enumerating `/dev/hidraw*` and matching VID/PID via `/sys/class/hidraw/<dev>/device/uevent`.

### Screensaver Hardware Verification

`CMD_REQUEST_ICON_REDRAW` / `LoadAt()` / screensaver bounce not yet exercised on real hardware. Needs a bench test of the full idle → screensaver → wake cycle once the firmware side is flashed.

### Process Group Channels (e.g. `deej.steam`) — NOT DESIGNED IN DETAIL

A slider targets a named group of processes (from a separate file) instead of listing them individually. Explicit per-channel `slider_mapping` assignments win over group membership.

Likely plugs into `applyTargetTransform()` in `session_map.go` alongside existing `deej.current`/`deej.unmapped`. Group file format TBD. Icon: `pushAll()` would need a special case to load a representative group icon rather than deriving from `targets[0]`.

### Decouple Icon Selection from Process Name — DISCUSS BEFORE IMPLEMENTING

Currently icon lookup is keyed off `targets[0]` from `slider_mapping` (lowercased, `.exe` stripped) — channel label has no effect. Idea: optional explicit icon key per channel, defaulting to current behavior. Overlaps with process group feature. Open questions: config shape, precedence, ordering relative to process groups.

### Remove Dithering Support — DECIDED, not yet implemented

In `channel_icon.go`: drop `applyFloydSteinberg`/`applyFloydSteinbergAlpha`, collapse `switch conversion` branches to threshold-only. Remove `icon_conversion` from `config.go`/`config.yaml` and `IconConversion` plumbing in `display.go`'s `pushAll()`/`handleIconRedrawRequest`.

### Soft Takeover — NOT DESIGNED

Move takeover logic to host (currently firmware-only for per-channel mute) and extend to connect-time: faders 1–5 currently snap-jump on connect; could freeze at app setpoint until the physical fader crosses it. Open: exact protocol changes needed, multi-session slider target resolution, whether this replaces snap-jump outright or is config-gated.

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
| Conversion | Always threshold (dithering removed — see "Remove Dithering Support") |
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
