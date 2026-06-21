# deej-x — Project Context (SERENITY)

## Standing Rules for Claude

- **Hardware questions go to the firmware CLAUDE.md.** All hardware details (pin assignments, display wiring, EEPROM layout, encoder behavior, RGB button state, etc.) are documented in `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`. Do not assume hardware details not documented here or there.
- **TBD items are blockers.** Several protocol details are explicitly deferred (icon format, icon dimensions, bitmap bit order, HID report format). Do not guess or assume values for these — check the TBD section first and ask the user if a decision is needed before proceeding.
- **Windows is the primary target.** Linux is best-effort secondary. All features must work on Windows. Linux implementations follow the same OS-abstraction pattern already established in the codebase.
- **This is a personal fork, not an upstream PR.** Do not attempt to maintain compatibility with the vanilla deej serial protocol or configuration format beyond what is documented here.
- **Investigate build/vet errors before calling them pre-existing or unrelated.** See "Build & Verification Gotchas" below — it documents real, root-caused issues (and how to actually verify the Linux build via WSL) found by digging in, not by waving errors away.

---

## Overview

Fork of [omriharel/deej](https://github.com/omriharel/deej) (MIT license), a Go desktop app that reads slider values from a serial-connected microcontroller and maps them to Windows/Linux audio session volumes.

**Module path:** `github.com/sclead03/deej-x`

This fork is customized exclusively for the **SERENITY** hardware: a custom ATmega32U4-based audio mixer with 5 faders, 5 mute buttons/LEDs, a rotary encoder, an RGB LED button, and 6 SSD1306 OLED displays. The original deej host app works with SERENITY as-is for basic fader control; this fork adds bidirectional communication, OLED icon/name streaming, and system mic mute via HID.

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
| `icon_conversion` | ✓ implemented | Per-channel list: `"dither"` (Floyd-Steinberg) or `"threshold"`; scalar value applies to all channels |

---

## Feature Status

### ✓ 1. Bidirectional Serial Protocol — COMPLETE

All host → firmware commands use binary framing:

```
[0x00][CMD_ID][LEN_LO][LEN_HI][...payload bytes...]
```

- `0x00` (null byte) is the escape prefix — never appears in ASCII fader data, unambiguous
- `LEN` is payload length in bytes, little-endian 16-bit
- **Fire-and-forget** — no ACK, no retry; USB CDC serial reliability is sufficient

**Assigned CMD_IDs (host → firmware):**

| Name | CMD_ID | Payload | Description |
|---|---|---|---|
| `CMD_QUERY` | `0x01` | none | Host → firmware: request ready beacon |
| `SET_CHANNEL_NAME` | `0x02` | `[channel_idx][name\0]` | Push display name for channel N |
| `SET_CHANNEL_ICON` | `0x03` | `[channel_idx][bitmap bytes]` | Push icon bitmap for channel N |
| `SET_MASTER_VOLUME` | `0x04` | `[vol_lo][vol_hi]` | Raw 0–1023, same domain as firmware's own `masterVol`; host's current master volume on connect |
| `SET_MIC_MUTE_STATE` | `0x05` | `[muted]` | `0x00` unmuted / `0x01` muted; host's current system mic mute state on connect |

Implemented in `serial_writer.go`. `SerialWriter` is created by `SerialIO` on connect and exposed via `SerialIO.Writer()`.

**Assigned CMD_IDs (firmware → host):**

| Name | CMD_ID | Payload | Description |
|---|---|---|---|
| `CMD_REQUEST_ICON_REDRAW` | `0x06` | `[channel_idx][x_offset][y_offset]` | Firmware's channel screensaver tick asking the host to re-render and re-stream that channel's icon at a new bounce position, instead of centered |

CMD_ID `0x06` is unassigned in the host→firmware direction, so there's no ambiguity, but note the two directions are independent namespaces anyway — they're parsed by entirely separate programs/state machines sharing only the physical UART. Read by `SerialIO.readFrames()` in `serial.go` (the binary-frame branch of the byte-stream parser that also produces fader-data lines), dispatched via `SerialIO.SubscribeToDeviceCommands()`, handled in `display.go`'s `handleIconRedrawRequest`.

### ✓ 2. Connection Handshake and Push Trigger — COMPLETE

Both connection scenarios are handled:

**Device connects while host is already running:**
- `hotplug_windows.go` registers a `WM_DEVICECHANGE` / `DBT_DEVICEARRIVAL` listener via a message-only window and `RegisterDeviceNotificationW` (GUID_DEVINTERFACE_COMPORT)
- On arrival, waits 500ms for the CDC driver to settle, then opens the serial port
- Connect event fires → `CMD_QUERY` sent → `SERENITY\r\n` beacon → push triggered

**Host launches with device already connected:**
- `display.go` receives the connect event, sends `CMD_QUERY` immediately
- Firmware responds with `SERENITY\r\n`
- Beacon received → push triggered

**Device unplugged and replugged while host is running:**
- `readFrames` goroutine closes its channel on read error
- `Start()` goroutine detects closed channel → calls `close()` → spawns `reconnect()` goroutine
- `reconnect()` calls `waitForSerialDevice()` (same hotplug path) then retries `Start()`

**Manual trigger:** "Push display icons" tray menu item calls `display.TriggerPush()`, skipping unchanged channels.

**Host-side change detection:** `DisplayManager` tracks `lastSentNames` and `lastSentIcons` per channel. Connection events force-push all channels; manual pushes skip unchanged ones.

Implemented in `display.go`, `serial.go`, and `hotplug_windows.go`.

### ✓ 3. Channel Name Streaming — COMPLETE

Names are pushed via `SET_CHANNEL_NAME` on every connection event and on manual push.

- Source: `channel_names` list in `config.yaml`, read into `CanonicalConfig.ChannelNames [5]string`
- `MaxChannelNameLength = 15` (constant in `serial_writer.go`; revisit when firmware font size is finalized)
- Config reload automatically picks up new names on the next manual push

### ✓ 5. Channel Icon Streaming — COMPLETE

Icons are pushed via `SET_CHANNEL_ICON` on every connection event and on manual push.

- Source: PNG files in `icon_dir` (config key), named after the process with `.exe` stripped (`chrome.png`, `spotify.png`)
- `deej.unmapped` maps to `unmapped.png`; `system` maps to `system.png`; `master` slot is skipped
- Conversion: per-channel, configurable via `icon_conversion` list — `"dither"` (Floyd-Steinberg) or `"threshold"`; a scalar value in config.yaml applies to all channels
- Pipeline (transparent PNG): detect alpha → box-filter resize alpha channel to 36×36 → use alpha as content mask (transparent=off, opaque=on); apply dither or threshold to alpha values for edge softening
- Pipeline (opaque PNG): box-filter resize RGB to 36×36 → grayscale → threshold or Floyd-Steinberg dither → 1-bit
- Output: 768-byte SSD1306 page-order frame; 36×36 icon placed at a given offset within the 128×48 blue area (46px horizontal / 6px vertical padding when centered)
- Implemented in `pkg/deej/icon/channel_icon.go`: `loadMono()` does decode/resize/dither, `packSSD1306(mono, leftPad, topPad)` packs at an arbitrary offset, `Load()` (centered) and `LoadAt()` (arbitrary offset) are thin wrappers. `Load` is wired into `display.go` `pushAll()`; `LoadAt` is used by `handleIconRedrawRequest` for screensaver bounce repositioning (see Feature 1's `CMD_REQUEST_ICON_REDRAW`).
- Missing icon files are logged at debug level and skipped gracefully (no crash)
- `lastSentIcons` change tracking prevents redundant re-sends on manual push; screensaver redraws also update `lastSentIcons` so a later centered push correctly notices the position changed

### ✓ 4. System Mic Mute via HID — COMPLETE (report validation pending TBD)

**Device identification:**
- USB VID: `0x1209`, USB PID: `0x0001`
- SERENITY enumerates as a composite USB device: CDC serial + HID

**HID implementation (pure Go, no CGO):**
- Windows: enumerates HID devices via `setupapi.dll` and `hid.dll` using `syscall.NewLazyDLL` — no C compiler required
- Device matched by VID/PID string in the device path, opened with `CreateFile`, read with `ReadFile`
- `MicMuter` interface with `_windows.go` / `_linux.go` implementations
- Windows mic mute: WASAPI/MMDeviceAPI (`go-wca`, already a dependency) to toggle default recording device mute
- Linux mic mute: `pactl set-source-mute @DEFAULT_SOURCE@ toggle` (best-effort)
- Linux HID enumeration: not yet implemented (`openSERENITY` returns an error; HID manager retries silently)

**Pending:** `handleReport` in `hid.go` currently triggers mute on any received report. Once the firmware HID descriptor is finalized, add a report format check there.

### ✓ 6. Master State Sync on Connect — master volume HARDWARE-VERIFIED; mic mute pending

Resolves the "master volume boots at 50%" issue: firmware's `masterVol` is hard-coded to 512 on power-on because it has no way to know the host's actual current state.

- On every beacon (`display.go` beacon handler, before `pushAll`), `DisplayManager.pushMasterState` sends `SET_MASTER_VOLUME` with the current master output volume and `SET_MIC_MUTE_STATE` with the current system mic mute state
- Master volume source: `sessionMap.getMasterVolume()` reads the `"master"` session's `GetVolume()` (0.0–1.0 scalar), converted to raw `0–1023` (`uint16(vol*1023 + 0.5)`) to match the firmware's native domain
- Mic mute source: `HIDManager.IsMicMuted()` → `MicMuter.IsMuted()` (Windows: `IAudioEndpointVolume.GetMute` on the default capture endpoint; Linux: `pactl get-source-mute @DEFAULT_SOURCE@`)
- If the master session or mic state isn't available (e.g. session map not yet populated), the corresponding push is skipped rather than guessed
- **Firmware side** — `processCmd` in `main.cpp` handles `0x04` (assigns `masterVol`, forces a bar redraw) and `0x05` (assigns `masterMuted`, forces an icon redraw + `applyRgbToHardware()`).
- **Master volume: hardware-verified 2026-06-19.** Required a host-side bug fix (see below) in addition to the firmware handler — the firmware side alone was not sufficient.
- **Mic mute: still pending hardware verification** — no test method established yet for toggling/observing Windows system mic mute during a bench session.

**Bug found and fixed (2026-06-19):** `serial.go`'s `handleLine` primes `currentSliderPercentValues` to `-1.0` whenever the detected slider count changes, which is "significantly different" from anything and forces a `SliderMoveEvent` on the next read for every slider — including slider 0 (`slider_mapping: 0: master`). `session_map.go` then unconditionally calls `SetVolume()` for that event, which overwrote the real Windows master volume with whatever `masterVol` the firmware happened to boot with (hardcoded 512), racing against and clobbering `pushMasterState`'s sync-down value. Unlike faders 1–5 (a physical position that *should* snap app volumes on connect), slider 0 has no physical position — it's the encoder's last state, which is meaningless before the host has told it anything. Fix: slider 0's first reading after a slider-count change is now primed silently (baseline recorded in `currentSliderPercentValues[0]`, no move event emitted); faders 1–5 keep the original priming behavior.

**Live tracking while connected — implemented 2026-06-20, pending hardware verification.** In addition to the connect-time sync above, `sessionMap` now watches for *external* master volume changes (Windows volume mixer, media keys, another app) while SERENITY stays connected, and pushes them down via the same `SET_MASTER_VOLUME` command. This is push-based, not polled, on both platforms:
- **Windows:** a hand-rolled `IAudioEndpointVolumeCallback` COM object is registered via `IAudioEndpointVolume.RegisterControlChangeNotify` (go-wca's own wrapper for this call is stubbed to `E_NOTIMPL`, so `session_finder_windows.go` calls the real vtable slot directly via `syscall.Syscall` — see `registerMasterVolumeChangeCallback`/`masterVolumeNotifyCallback`). The callback fires synchronously on the audio engine's own thread for every master volume/mute change and is filtered by comparing `guidEventContext` against deej's own `eventCtx` GUID, so deej's own writes (the SERENITY encoder) are recognized precisely, not just by a time heuristic.
- **Linux:** `session_finder_linux.go` subscribes to PulseAudio's native event mechanism (`proto.Subscribe{Mask: paSubscriptionMaskSink}` + `client.Callback`), re-reading the default sink's volume only when a real sink-change event arrives.
- Both implementations satisfy a shared `MasterVolumeWatcher` interface (`session_finder.go`); `sessionMap.setupMasterVolumeWatcher` forwards changes to `DisplayManager` only if they weren't just caused by deej itself (`sessionMap.markMasterVolumeSetByDeej`/`masterVolumeRecentlySetByDeej`, a 500ms window — the Linux watcher has no per-event context to compare against, so it relies on this generic backstop; Windows uses both the precise GUID check and this backstop).
- **Do not implement this as a polling loop.** A prior attempt used a `time.Ticker` polling `getMasterVolume()` every 250ms; this was explicitly rejected as an unacceptable approach for a never-ending host-resident loop. The push-based mechanisms above were built specifically to avoid that.
- Not yet exercised against real hardware — needs a bench test confirming the OLED follows a Windows-side volume change while SERENITY stays connected.

---

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

This machine has WSL (Ubuntu 24.04) installed, with a real Go toolchain, gcc, and the GTK3/appindicator/webkit2gtk dev headers `systray` needs for its native cgo build (`golang-go`, `build-essential`, `pkg-config`, `libgtk-3-dev`, `libappindicator3-dev`, `libwebkit2gtk-4.1-dev` — installed 2026-06-20). Use it instead of cross-compiling or declaring Linux "unverifiable from here":

```
wsl -d Ubuntu -- bash -lc "cd '/mnt/c/Users/Steven/Documents/Solid Models/deej/Deej-X/Deej-X' && PKG_CONFIG_PATH=\$HOME/pkgconfig-shim go build ./... 2>&1"
wsl -d Ubuntu -- bash -lc "cd '/mnt/c/Users/Steven/Documents/Solid Models/deej/Deej-X/Deej-X' && PKG_CONFIG_PATH=\$HOME/pkgconfig-shim go vet ./... 2>&1"
```

- Ubuntu 24.04 only ships `webkit2gtk-4.1`, but the pinned 2020-era `systray` version hardcodes a `webkit2gtk-4.0` pkg-config lookup. Fixed with a no-sudo shim at `~/pkgconfig-shim/webkit2gtk-4.0.pc` *inside WSL* that redirects to the installed 4.1 package — always pass `PKG_CONFIG_PATH=$HOME/pkgconfig-shim` on build/vet commands there.
- Installing/removing apt packages needs `sudo`, which requires an interactive password Claude doesn't have. Ask the user to run the `apt-get install` command themselves (the `!` prefix, or a one-line `wsl -d Ubuntu -- sudo ...` for a separate cmd/PowerShell window) rather than attempting to bypass this.
- A harmless linker warning (`missing .note.GNU-stack section implies executable stack`) is normal on this cgo build and not a real issue.

### `signal.Notify` with an unbuffered channel — fixed 2026-06-20

`pkg/deej/util/util.go`'s `SetupCloseHandler` used to create an **unbuffered** `chan os.Signal` passed to `signal.Notify`. `signal.Notify` does a non-blocking send to registered channels, so an unbuffered channel can silently drop the OS interrupt signal if nothing happens to be receiving at that exact instant. Fixed by buffering the channel (`make(chan os.Signal, 1)`). If `go vet` flags this pattern again elsewhere, apply the same fix — don't dismiss it as a pre-existing warning without checking.

### `go vet`'s `unsafeptr` check on Win32/COM callback structs

When writing a hand-rolled COM callback (mirroring the `IMMNotificationClient` pattern already in `session_finder_windows.go`), declare pointer-typed callback parameters as their real pointer type (e.g. `pNotify *audioVolumeNotificationData`), **not** `uintptr` plus a manual `unsafe.Pointer(uintptr)` cast inside the function body. `syscall.NewCallback` marshals typed pointer arguments directly — see the existing `this *wca.IMMNotificationClient` parameter on `defaultDeviceChangedCallback`. Converting a `uintptr` to `unsafe.Pointer` after the fact is exactly the pattern `go vet`'s `unsafeptr` check flags (fabricating a pointer from an arbitrary integer), even though it happens to be safe in practice here (the memory belongs to the OS/COM caller, not the Go GC). Use the typed-parameter form so vet stays clean instead of suppressing or excusing the warning.

---

## Remaining Work

### HID Report Validation

One-line fill-in once firmware HID descriptor is known. See `handleReport` in `hid.go`.

### Linux HID Enumeration

Best-effort. Implement `openSERENITY()` in `hid_linux.go` by enumerating `/dev/hidraw*` and matching VID/PID via `/sys/class/hidraw/<dev>/device/uevent`.

### Screensaver Hardware Verification

`CMD_REQUEST_ICON_REDRAW` handling, the 36×36 icon resize, and `readFrames()` all build and the existing unit-level logic is unchanged for normal (centered) pushes, but none of this has been exercised against real hardware yet — needs a bench test of the full idle → screensaver → wake cycle once the firmware side is flashed. See firmware CLAUDE.md "Current State → Implemented, pending hardware verification."

### Process Group Channels (e.g. `deej.steam`)

**Idea (not yet designed in detail):** a slider should be able to target a *named group* of processes defined in a separate file — e.g. a `SteamGames` group listing `cyberpunk2077.exe`, `thelastofus.exe`, `mahjong.exe`, etc. — instead of listing every process individually in `slider_mapping`. One slider would control the volume of whichever of those processes happens to be running, and the channel OLED would show a single representative icon (e.g. Steam's) rather than per-game icons.

**Priority rule:** if a process is both (a) listed in the group file and (b) explicitly assigned to its own separate channel in `slider_mapping`, the explicit per-channel assignment wins — that process is excluded from the group for volume-control purposes (it shouldn't be controlled by two sliders at once).

**Where this likely plugs in**, based on the existing special-target mechanism in `session_map.go`:
- `specialTargetTransformPrefix` ("deej.") already dispatches to `applyTargetTransform()`, which currently only handles `deej.current` and `deej.unmapped`. A new case (`deej.steam`, or a generic `deej.group:<name>` if multiple groups are wanted) would read the group file, return all matching session keys as `resolvedTargets`, minus any process name that's also explicitly mapped to a *different* slider elsewhere in `SliderMapping` (the override case above) — `sessionMapped()` already walks the full mapping table, so the exclusion check can reuse that pattern.
- The group file itself: format TBD ("doesn't need to be a .yaml" per discussion) — could be a new top-level config key (a path, like `icon_dir`) or a section inside `config.yaml`. Needs a decision before implementation.
- Icon side: `display.go`'s `pushAll()` currently loads an icon by treating `targets[0]` as a literal process name (`icon.Load(processName, ...)`). A group-targeted channel would need a special case (similar to the existing `processName == "master"` skip) that loads a fixed group icon (e.g. `steam.png`) instead of trying to resolve one of the many underlying game executables.

### Decouple Icon Selection from Process Name — DISCUSS FURTHER BEFORE IMPLEMENTATION

**Current behavior:** icon association has nothing to do with `channel_names` (the OLED display label) — it's keyed entirely off `slider_mapping`. `pushAll()` in `display.go` takes `targets[0]` for a channel's slider mapping (e.g. `firefox.exe`) and `icon.Load()` lowercases it, strips `.exe`, and looks for that exact filename in `icon_dir` (`firefox.png`). Renaming the channel label to "Browser" has zero effect on icon lookup. Also: if a slider maps to multiple processes, only `targets[0]` is used for the icon — the rest are ignored for icon purposes.

**Idea:** add an explicit, optional icon key per channel/slider (defaulting to the current process-name-derived behavior if unset, so existing configs don't break), so a channel labeled "Browser" mapped to `firefox.exe` could explicitly declare `icon: firefox` (or similar) without relying on the process name matching a filename. This also gives the process-group feature above (`deej.steam`) a clean way to declare its own representative icon explicitly instead of needing another special case.

**Open questions to resolve before building this:** exact config shape (per-slider field vs. a separate icon-mapping section), precedence if both an explicit icon key and a same-named PNG exist, and whether this should land before or after the process-group feature since they overlap (a group's icon is a more general case of "icon not derived from process name").

### Remove Dithering Support

**Decided — remove.** Floyd-Steinberg dithering hasn't shown a visible benefit on icon edges at 36×36; for flat-color app logos, edge aliasing happens either way and dithering tends to scatter stray pixels near edges rather than smooth them. Removing it simplifies the pipeline and the user-facing config surface (one less thing to configure/explain).

**What changes:** in `pkg/deej/icon/channel_icon.go`, collapse `loadMono` to always threshold (drop the `applyFloydSteinberg` / `applyFloydSteinbergAlpha` functions and the `switch conversion` branches in both the transparency and opaque paths). Remove the `icon_conversion` config key from `config.go`/`config.yaml` and `IconConversion` plumbing in `display.go`'s `pushAll()`/`handleIconRedrawRequest`. Update the "Conversion" row in this file's Icon Protocol — Decided table and the Config keys table.

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
| Displayed icon size | 36×36 pixels (reduced from 48×48 to leave bounce room for the channel screensaver — see firmware CLAUDE.md Display Design) |
| Wire format | 768 bytes — full 128×48 blue area in SSD1306 page order; icon at a given offset (46px horizontal / 6px vertical padding when centered) |
| Bit order | SSD1306 native: each byte = one column of 8 vertical pixels; bit 0 = topmost pixel of page |
| Conversion | Configurable: `dither` (Floyd-Steinberg) or `threshold`; set via `icon_conversion` in `config.yaml` |
| `master` slot (index 0) | Skip icon push — master OLED is encoder-controlled, not a channel display |
| `deej.unmapped` slot | Use bundled default icon; user can override by placing `unmapped.png` in `icon_dir` |
| `system` slot | Use bundled default icon; user can override by placing `system.png` in `icon_dir` |

**TODO:** Design and bundle default icons for `deej.unmapped` and `system` slots. These ship with the package as fallback; user can drop their own file in `icon_dir` to override.

## TBD — Do Not Assume These

| Item | Blocked on |
|---|---|
| Custom HID report format (mic mute) | RGB button hardware replacement + firmware HID descriptor |

---

## Reference

- Firmware repo and hardware ground truth: `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`
- Upstream deej: https://github.com/omriharel/deej
