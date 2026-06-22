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
| `SET_MIC_MUTE_STATE` | `0x05` | `[muted]` | `0x00` unmuted / `0x01` muted; host's current system mic mute state on connect, plus live updates (RGB button and external Windows changes) |
| `SET_MASTER_MUTE_STATE` | `0x07` | `[muted]` | `0x00` unmuted / `0x01` muted; master output's real WASAPI mute state (volume mixer mute button, media keys), pushed on connect and live. Distinct from SERENITY's own local `volMuted` toggle (encoder click), which never needed this command since it already round-trips via the existing fader/ASCII channel — see "Master + Mic Mute Live Sync" below |

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

### ✓ 4. System Mic Mute via HID — COMPLETE

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

**Report ID filtering implemented 2026-06-21.** Previously `handleReport` in `hid.go` triggered mute on *any* received report — this was more than a TODO, it was an active bug: the encoder's double-click sends a Consumer Control Play/Pause report (report ID 3, `kConsumerDesc` in `main.cpp`) on the *same shared HID interface*, so every double-click also fired a spurious mute toggle, and reliably crashed deej-x outright (see "Encoder double-click crash" below). Firmware now sends the RGB button's mic-mute signal as its own dedicated report — report ID 4, vendor-defined usage page 0xFF00 (`kMicMuteDesc`/`sendMicMuteButtonReport()` in `main.cpp`) — and `handleReport` checks `report[0] == micMuteReportID` before doing anything, ignoring the Play/Pause report entirely. After a successful toggle, it also reads back the real resulting state via `IsMuted()` and pushes it down with `SerialWriter.SendMicMuteState` (the same `SET_MIC_MUTE_STATE`/`0x05` command already verified for connect-time sync), correcting firmware's optimistic local toggle if it guessed wrong.

**Encoder double-click crash — root-caused and fixed 2026-06-21.** Every double-click reliably crashed deej-x with no toast, no dialog, and no crash log. Root cause was two-fold:
1. The "any report triggers mute" bug above meant the Play/Pause report from double-click was hitting `windowsMicMuter.ToggleMute()` at all, which it was never supposed to.
2. `ToggleMute`'s COM call chain (`withCaptureVolume` in `hid_windows.go`) runs on `HIDManager`'s read-loop goroutine, which was never pinned with `runtime.LockOSThread()`. Confirmed via a console build (`go run`) that the crash is a Go runtime **`fatal error: fault`** (a corrupted-heap abort that dumps every goroutine, not a normal single-stack `panic:`) — `recover()` cannot catch this class of error by design, which is exactly why `panic.go`'s crash-log/toast mechanism never fired. This is the same structural hazard as the master-volume-callback bug above (hand-rolled `syscall.Syscall`-based COM bindings, unpinned goroutine), just surfacing as heap corruption here instead of a silently-dead callback. Fixed the same way: `withCaptureVolume` now calls `runtime.LockOSThread()`/`defer runtime.UnlockOSThread()` for the duration of each call (unlocked afterward, unlike the master-volume fix, since nothing here needs to outlive the call).
3. A misleading detail hit while debugging: the original crash's panic-style trace pointed at `m.logger.Debugw(...)` with what looked like a nil/garbage receiver. Verified via temporary diagnostic logging that `m`/`m.logger` are never nil — that frame's printed values were an artifact of the optimized build's stack unwinding, not the real bug. Don't trust argument values in an optimized-build crash trace at face value; the `fatal error: fault` + full goroutine dump was the real signal.

**Bench-verified 2026-06-21 after reflash.** Double-click no longer crashes or affects mic mute. Hit two more real bugs surfaced only by hardware testing, both fixed:
- **Wrong HID collection opened.** Adding the second top-level collection (mic-mute) made Windows split SERENITY's HID interface into two separate device paths (`HID\VID_1209&PID_0001&MI_02&COL01` = Consumer Control, `&COL02` = vendor-defined mic-mute), confirmed via `Get-CimInstance Win32_PnPEntity`. `openSERENITY()` in `hid_windows.go` matched on VID/PID substring alone and returned the *first* match it enumerated (COL01, Play/Pause) — it never reached COL02, so the mic-mute report never arrived at all. Fixed by adding `matchesMicMuteCollection()`, which opens each VID/PID match and checks its actual HID usage via `HidD_GetPreparsedData`/`HidP_GetCaps` (usage page `0xFF00`, usage `1`, matching firmware's `kMicMuteDesc`), skipping non-matching collections instead of returning the first hit.
- **Second crash, different mechanism, after the fix above.** Once the report reached `ToggleMute()`, the real WASAPI toggle succeeded every time, but the process then crashed immediately after — same call site as the double-click crash (`m.logger.Debugw(...)` inside `ToggleMute`'s closure, right after `aev.SetMute` returned), but this time a clean recoverable `panic: invalid memory address or nil pointer dereference` rather than the earlier `fatal error: fault`. The `runtime.LockOSThread()` fix (added for the double-click crash) did *not* fix this — it kept recurring at the identical line even with that fix in place, which in hindsight means the double-click "fix" likely only worked because report-ID filtering stopped Play/Pause from ever reaching `ToggleMute` again, not because `LockOSThread` actually fixed the underlying issue. The one consistent pattern across every repro: `IsMuted()`'s closure (which only touches the local `aev`/`muted`, never the `m` receiver) never once crashed, while `ToggleMute()`'s closure (which called `m.logger.Debugw(...)`, referencing the receiver, from inside the closure passed across the COM call boundary) crashed every time. Fixed by restructuring `ToggleMute` to only touch `aev` and a plain local inside the closure, moving the `m.logger.Debugw` call to *after* `withCaptureVolume` returns, fully outside the COM/syscall chain. Bench-confirmed clean across ~15 consecutive button presses (mute/unmute both directions) with no crash. The exact mechanism (why touching the receiver from inside a closure spanning a hand-rolled syscall COM call corrupts something) was not fully root-caused — the fix was validated empirically by elimination, not by a confirmed theory. If something similar resurfaces in other hand-rolled COM closures in this codebase (`session_finder_windows.go`'s callbacks), try this same restructuring (no receiver/captured-state access inside the closure) before assuming it's the apartment/threading class of bug again.

RGB LED color feedback still not testable (common-anode button replacement still on order) — everything else in this feature is done.

### ✓ 6. Master State Sync on Connect — COMPLETE, all hardware-verified

Resolves the "master volume boots at 50%" issue: firmware's `masterVol` is hard-coded to 512 on power-on because it has no way to know the host's actual current state.

- On every beacon (`display.go` beacon handler, before `pushAll`), `DisplayManager.pushMasterState` sends `SET_MASTER_VOLUME` with the current master output volume and `SET_MIC_MUTE_STATE` with the current system mic mute state
- Master volume source: `sessionMap.getMasterVolume()` reads the `"master"` session's `GetVolume()` (0.0–1.0 scalar), converted to raw `0–1023` (`uint16(vol*1023 + 0.5)`) to match the firmware's native domain
- Mic mute source: `HIDManager.IsMicMuted()` → `MicMuter.IsMuted()` (Windows: `IAudioEndpointVolume.GetMute` on the default capture endpoint; Linux: `pactl get-source-mute @DEFAULT_SOURCE@`)
- If the master session or mic state isn't available (e.g. session map not yet populated), the corresponding push is skipped rather than guessed
- **Firmware side** — `processCmd` in `main.cpp` handles `0x04` (assigns `masterVol`, forces a bar redraw) and `0x05` (assigns `masterMuted`, forces an icon redraw + `applyRgbToHardware()`).
- **Master volume: hardware-verified 2026-06-19.** Required a host-side bug fix (see below) in addition to the firmware handler — the firmware side alone was not sufficient.
- **Mic mute: hardware-verified 2026-06-21**, alongside the live mute-tracking work below.

**Bug found and fixed (2026-06-19):** `serial.go`'s `handleLine` primes `currentSliderPercentValues` to `-1.0` whenever the detected slider count changes, which is "significantly different" from anything and forces a `SliderMoveEvent` on the next read for every slider — including slider 0 (`slider_mapping: 0: master`). `session_map.go` then unconditionally calls `SetVolume()` for that event, which overwrote the real Windows master volume with whatever `masterVol` the firmware happened to boot with (hardcoded 512), racing against and clobbering `pushMasterState`'s sync-down value. Unlike faders 1–5 (a physical position that *should* snap app volumes on connect), slider 0 has no physical position — it's the encoder's last state, which is meaningless before the host has told it anything. Fix: slider 0's first reading after a slider-count change is now primed silently (baseline recorded in `currentSliderPercentValues[0]`, no move event emitted); faders 1–5 keep the original priming behavior.

**Live tracking while connected — implemented 2026-06-20, bench-tested 2026-06-21 (confirmed NOT functional), root-caused and fixed 2026-06-21.** In addition to the connect-time sync above, `sessionMap` watches for *external* master volume changes (Windows volume mixer, media keys, another app) while SERENITY stays connected, and pushes them down via the same `SET_MASTER_VOLUME` command. This is push-based, not polled, on both platforms:
- **Windows:** a hand-rolled `IAudioEndpointVolumeCallback` COM object is registered via `IAudioEndpointVolume.RegisterControlChangeNotify` (go-wca's own wrapper for this call is stubbed to `E_NOTIMPL`, so `session_finder_windows.go` calls the real vtable slot directly via `syscall.Syscall` — see `registerMasterVolumeChangeCallback`/`masterVolumeNotifyCallback`). The callback fires synchronously on the audio engine's own thread for every master volume/mute change and is filtered by comparing `guidEventContext` against deej's own `eventCtx` GUID, so deej's own writes (the SERENITY encoder) are recognized precisely, not just by a time heuristic.
- **Linux:** `session_finder_linux.go` subscribes to PulseAudio's native event mechanism (`proto.Subscribe{Mask: paSubscriptionMaskSink}` + `client.Callback`), re-reading the default sink's volume only when a real sink-change event arrives.
- Both implementations satisfy a shared `MasterVolumeWatcher` interface (`session_finder.go`); `sessionMap.setupMasterVolumeWatcher` forwards changes to `DisplayManager` only if they weren't just caused by deej itself (`sessionMap.markMasterVolumeSetByDeej`/`masterVolumeRecentlySetByDeej`, a 500ms window — the Linux watcher has no per-event context to compare against, so it relies on this generic backstop; Windows uses both the precise GUID check and this backstop).
- **Do not implement this as a polling loop.** A prior attempt used a `time.Ticker` polling `getMasterVolume()` every 250ms; this was explicitly rejected as an unacceptable approach for a never-ending host-resident loop. The push-based mechanisms above were built specifically to avoid that — this constraint still holds; do not fall back to polling.
- **Root cause found and fixed (2026-06-21):** `GetAllSessions()` in `session_finder_windows.go` called `ole.CoInitializeEx` then `defer ole.CoUninitialize()` around its entire body — including `registerDefaultDeviceChangeCallback()` and `registerMasterVolumeChangeCallback()`. Both register long-lived notification sinks against the STA apartment created by that `CoInitializeEx` call, but the deferred `CoUninitialize()` tore that exact apartment down the instant `GetAllSessions()` returned — before the audio engine ever got a chance to dispatch a callback into it. Registration itself succeeded (HRESULT 0), so nothing was ever logged as failing; the callback just silently never fired, matching the bench symptom exactly. Additionally, the calling goroutine was never pinned with `runtime.LockOSThread()`, so even within a single call the apartment's owning OS thread wasn't guaranteed stable. Fix: `GetAllSessions()` now calls `runtime.LockOSThread()` on entry (idempotent/refcounted, never unlocked — mirrors the existing pattern in `hotplug_windows.go`) and no longer calls `CoUninitialize()` at all; COM stays initialized on whichever OS threads end up calling this function for the life of the process, which is the standard pattern for a long-running background service and costs nothing meaningful for a tray app.
- **Bench-verified 2026-06-21 with SERENITY connected.** Initial bench test showed the master OLED updating but lagging/stepping behind rapid host-side scrolling, and occasionally settling 3-5% off from the host's true final value. Root cause: `sf.masterVolumeChanges` (capacity 8, drop-newest-on-full) and the per-consumer fan-out (unbuffered, blocking send) formed a FIFO queue under load — a fast scroll fires far more notifications than the 115200-baud serial link can drain, so the queue filled and **new** (including the final) notifications got dropped while stale ones were still being processed. Fixed by making both hops capacity-1, latest-value-wins (evict-and-replace instead of drop-newest) — see `masterVolumeNotifyCallback` and `sessionMap.forwardMasterVolumeChange`. This fixed live smoothness but a final-value mismatch could still occur if the very last live notification was itself the one evicted right as scrolling stopped.
- **Settle correction, added 2026-06-21.** The latest-value-wins fix above fixed smoothness, but occasional 3-5% final-state mismatches remained (the very last live notification can still be the one evicted right as scrolling stops). `sessionMap.setupMasterVolumeWatcher` runs a `masterVolumeSettleDelay` (100ms) debounce timer, reset on every forwarded live change. When it fires (no further change for 100ms), it re-reads the volume directly via `getMasterVolume()` — bypassing the live/coalescing path entirely — and forwards it as a `MasterVolumeUpdate{ForceSync: true}`. `DisplayManager.handleExternalMasterVolumeChange` always sends a `ForceSync` update, skipping the usual noise-threshold dedup. This alone wasn't sufficient — see the serial echo feedback loop below, found during the same bench session, which was actually starving the settle mechanism along with everything else.
- **Serial echo feedback loop — found and fixed 2026-06-21, the real cause of "gets stuck and never catches up."** Bench testing (with the fixes above in place) showed the master OLED still lagging/stepping behind fast scrolling and, critically, never catching up at all if the user stopped scrolling mid-lag — which the 100ms settle correction should have prevented. Root cause, confirmed via debug logs: slot 0 ("master") in the device→host ASCII telemetry line (`masterVol|fader0|...`) was designed for a SERENITY encoder turn (real user input, should mirror to Windows) but SERENITY also echoes back whatever `masterVol` it currently holds on every regular line — including values *we* just pushed via `SET_MASTER_VOLUME`. `serial.go`'s `handleLine` couldn't tell the difference, so it read every echo as a fresh encoder move and called `session.SetVolume()` on it again. That re-write is correctly recognized as deej's own write by the GUID check, but it also re-arms `markMasterVolumeSetByDeej`'s 500ms suppression window (`session_map.go`) — and since the echo loop kept retriggering roughly every cycle during continuous scrolling, the window effectively never got a chance to expire, silently swallowing genuinely external Windows-side changes for the entire duration of the scroll. Fixed in two parts:
  - `SerialWriter` (`serial_writer.go`) now tracks the last raw value sent via `SendMasterVolume` (`LastSentMasterVolumeRaw`) and when it was sent (`TimeSinceLastSentMasterVolume`).
  - `handleLine` (`serial.go`) recognizes a slot-0 reading as an echo (and skips emitting a `SliderMoveEvent` for it) if it exactly matches the last-sent raw value, **or** if it arrives within `masterVolumeSerialEchoWindow` (200ms) of the last send. The exact-match check alone wasn't sufficient: a fast scroll can have multiple `SET_MASTER_VOLUME` pushes in flight before their echoes return, so an echo of an older, already-superseded push wouldn't match the *latest* sent value anymore, fell through as a "real" encoder move, and reapplied a stale value to Windows — the audible/visible "step backwards" the user described while scrolling. The 200ms window catches every in-flight echo regardless of how many pushes are queued ahead of it.
  - This also explained the residual 1-2% drift on otherwise-isolated single steps even before the feedback-loop was firing continuously: each misread echo forced a redundant round trip through `NormalizeScalar`'s truncate-to-2-decimals, very slightly degrading an otherwise-precise value.
- **Firmware rounding bug — found and fixed 2026-06-21, alongside the above.** Even with the feedback loop fixed, the firmware's master volume percentage display used integer-truncating division (`vol * 100 / 1023`) instead of rounding, biasing the displayed percentage down by up to ~1% versus Windows. Fixed in firmware (`main.cpp`'s `drawMasterNormal()`): `pct = (uint8_t)(((uint32_t)vol * 100 + 511) / 1023)` — round instead of truncate. See firmware CLAUDE.md for this side of the fix.
- **All three fixes bench-verified together 2026-06-21:** slow/steady changes land on the exact host percentage every time; fast continuous scrolling no longer bounces backward and lands on the same final number as the host when scrolling stops, including the previously-reproduced 64%-vs-54% stuck case.
- Added permanent debug-level logging at each stage of this pipeline (`registerMasterVolumeChangeCallback`, `masterVolumeNotifyCallback`, `sessionMap.setupMasterVolumeWatcher`) so any future regression here is immediately visible in `--verbose`/dev-build logs instead of failing silently again.
- **Bonus finding (2026-06-21):** the same Windows notification struct (`audioVolumeNotificationData`, mirroring `AUDIO_VOLUME_NOTIFICATION_DATA`) already carries the master mute bit (`BMuted`) alongside the volume level (`FMasterVolume`) — it arrives in `masterVolumeNotifyCallback` today but is discarded; only `FMasterVolume` is read. This became the basis for "Master + Mic Mute Live Sync" below.

**Master + Mic Mute Live Sync — implemented and bench-verified 2026-06-21.** Two-way mute tracking for both master volume and mic, following the same architecture as live master volume tracking above (push-based OS callbacks, not polling).

- **Master mute (`volMuted`):** no new registration needed — `BMuted` was already arriving in the existing `masterVolumeNotifyCallback`/`registerMasterVolumeChangeCallback` registration (see "Bonus finding" above), just discarded. `MasterVolumeWatcher.SubscribeToMasterVolumeChanges()` now returns `<-chan MasterVolumeNotification{Volume, Muted}` instead of a bare `float32` (both platforms updated, including Linux's `GetSinkInfoReply.Mute`/`GetSourceInfoReply.Mute` fields, which the jfreymuth/pulse library already exposes). `sessionMap.MasterVolumeUpdate` carries `Muted` alongside `Volume`/`ForceSync` through the same forwarding/consumer-channel plumbing. `DisplayManager.handleExternalMasterVolumeChange` now pushes volume and mute independently (separate dedupe state, `lastPushedVolMuted`/`havePushedVolMuted`) since either can change without the other — muting via the mixer's mute button alone leaves the volume level untouched. Pushed via the new `SET_MASTER_MUTE_STATE` (`0x07`) command, both on connect (`pushMasterState`, via new `masterSession.GetMuted()`/`sessionMap.getMasterMuted()`) and live.
- **Mic mute:** a *second*, independent `RegisterControlChangeNotify` registration (`registerMicMuteChangeCallback`/`micMuteNotifyCallback` in `session_finder_windows.go`), this time against `sf.masterIn.volume` (the default capture endpoint) instead of `sf.masterOut.volume`. New `MicMuteWatcher` interface (`SubscribeToMicMuteChanges() <-chan bool`), Windows-only — mirrors the rest of the mic-mute feature, which has no Linux implementation either. `sessionMap.setupMicMuteWatcher`/`forwardMicMuteChange` mirror the master volume watcher's consumer-fanout plumbing. Pushed via the existing `SET_MIC_MUTE_STATE` (`0x05`) — no new firmware command needed, since `processCmd`'s existing handler already does an absolute assignment regardless of *why* the state changed.
- **One nuance, not a bug:** SERENITY's `volMuted` is implemented as "zero the transmitted serial value," not a real WASAPI mute flag (see firmware CLAUDE.md). Pushing `SET_MASTER_MUTE_STATE(true)` therefore makes firmware's regular ASCII output zero out, which round-trips back through the existing slider-move path and forces Windows' *reported volume level* to 0% while muted via the mixer (instead of preserving the level with just a mute flag) — restoring correctly to the original level on unmute, since firmware's internal `masterVol` is never actually touched by this. This matches how the encoder's own local mute has always behaved; not a new side effect of this feature.
- **Crash found and fixed 2026-06-21: tagging the RGB button's `SetMute` call with a COM eventContext GUID.** To let `micMuteNotifyCallback` distinguish the RGB button's own writes from external ones (mirroring the master volume watcher's GUID filter), `windowsMicMuter.ToggleMute()` was changed to pass a real `*ole.GUID` (same value as `wcaSessionFinder.eventCtx`) instead of `nil` as `SetMute`'s second argument — even captured as a plain local *before* the closure, matching the existing "don't touch the receiver inside the closure" rule from the mic-mute crash history. **This crashed on the very first button press every time** (`Exception 0xc0000005`, access violation) — a console build's crash trace pointed exactly at the `aevSetMute` syscall itself (`iaudioendpointvolume_windows.go`), not at any of this codebase's Go logic. `SetMasterVolumeLevelScalar` uses an identical 3-arg syscall shape with a real (non-nil) GUID and has never crashed once across this entire session's live-volume testing — the only difference is the calling context: `ToggleMute` runs inside `withCaptureVolume`'s per-call `runtime.LockOSThread()`/fresh-`CoInitializeEx` cycle (the same fragile context already responsible for two earlier, structurally different crashes in this exact call path — see [[project-deejx-mic-mute-hid]]), not an ordinary goroutine. The precise mechanism was not root-caused (consistent with this codebase's established pattern for this specific area); fixed empirically by reverting `SetMute`'s eventContext argument to always `nil` and removing GUID-tagging from `windowsMicMuter` entirely. Self-triggered mic-mute writes are now recognized downstream by `sessionMap.markMicMuteSetByButton`/`micMuteRecentlySetByButton` — a plain time window (500ms, `micMuteEchoSuppressWindow`), exactly mirroring `markMasterVolumeSetByDeej`'s backstop, with **no COM/syscall involvement in the filtering itself.**
- **A second, smaller bug found in the same bench session: a mark-after-toggle ordering race.** `hid.go`'s `handleReport` originally called `markMicMuteSetByButton()` *after* `h.muter.ToggleMute()` returned. But `SetMute`'s registered notification callback can fire synchronously, on the same call stack, *before* `ToggleMute` even returns — so the live watcher's suppression check could run before the mark was recorded, occasionally letting a button-triggered change through `setupMicMuteWatcher` as if it were external. Harmless in practice (both the button's direct push and the watcher's "external" push agreed on the same value, so it only meant an occasional redundant duplicate send, confirmed via debug logs), but not the intended single clean push. Fixed by moving `markMicMuteSetByButton()` to *before* `ToggleMute()` is called.
- `micMuteNotifyCallback` does no work inline beyond copying the notification by value and spawning a goroutine (`handleMicMuteNotification`) to do the actual logging/filtering/channel-send — kept deliberately minimal given this callback can be reentered into Go-callback space while the triggering call (`ToggleMute`) is still inside its own locked-thread/syscall chain. Not proven necessary by itself (the actual crash was the GUID argument, not reentrancy), but cheap insurance given how fragile this exact code path has proven to be across three separate incidents now.
- Bench-verified 2026-06-21: master mute and mic mute both work in both directions (SERENITY-initiated and Windows-initiated) with no crashes across repeated toggles, and no double-pushing once the ordering race was fixed.

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

### NEXT PRIORITY — Master volume mute broken/inconsistent, reported 2026-06-21 — NOT YET INVESTIGATED

Reported right after the master+mic mute live-sync work above shipped; very likely a regression from it, but **not yet investigated or fixed — do not start until explicitly told to.** Symptoms as described (exact repro not yet pinned down):
- Hardware-initiated mute (SERENITY's encoder click) no longer functions at all while deej-X is connected.
- Host-initiated mute (external Windows-side mute, pushed via `SET_MASTER_MUTE_STATE`) correctly zeros the volume, but unmute clears the mute icon/flag on SERENITY while leaving the volume at 0 instead of restoring it.

Most likely place to look first, once investigation starts: the interaction between firmware's local `volMuted` zero-spoof (which also drives the regular fader/ASCII output) and the new `SET_MASTER_MUTE_STATE` push touching that same flag — see firmware CLAUDE.md's matching TODO entry for the firmware-side half of this.

**Open design question to revisit as part of this, not a fix to make right now:** why is master mute "zero the transmitted serial value" (firmware-local, round-trips through the regular slider-move path) instead of a real WASAPI mute (`IAudioEndpointVolume.SetMute`/`GetMute` on the master output endpoint — the same mechanism mic mute already uses)? The zero-spoof approach is the suspected source of this bug class; real WASAPI mute would preserve the underlying volume level independent of the mute flag, which seems more robust. Worth reconsidering the whole approach instead of just patching this specific symptom.

### Global Mic Mute (mute all inputs, unmute one) — NOT DESIGNED IN DETAIL, discussion only (2026-06-21)

Today's mic mute (`windowsMicMuter`, Feature 4 above) only ever touches **the OS default capture device** — `withCaptureVolume` calls `GetDefaultAudioEndpoint(ECapture, EConsole, ...)`, a single device, every time. There's no per-device targeting for mic mute at all today (the friendly-name device targeting that exists for `slider_mapping` is a volume-only mechanism, unrelated).

**Desired behavior, discussed 2026-06-21:**
- **Mute** (RGB button) → mute **every active input device**, not just the default. Mechanism: enumerate all active capture endpoints (Windows: `IMMDeviceEnumerator.EnumAudioEndpoints(ECapture, DEVICE_STATE_ACTIVE, ...)`, mirroring the existing output-device enumeration already in `enumerateAndAddSessions` — same pattern, `ERender` → `ECapture`; Linux: enumerate all PulseAudio sources instead of just `@DEFAULT_SOURCE@`) and call mute on each.
- **Unmute** → only **one specific configured device** (by friendly name), leaving every other input device muted. Asymmetric by design.
- **Config shape (proposed, not finalized):**
  ```yaml
  mic_mute:
    mute_target: input.global          # sentinel meaning "every active input device"
    unmute_target: "USB Microphone"    # friendly name, exactly one device
  ```
- **Host must track an explicit "intended" global state**, not just react to queried device states — so a newly-connected input device (hotplug) inherits the current intended state on arrival: mute it immediately if the intended state is "muted," leave it alone if "unmuted."
- **Live mic-mute tracking for the single default capture device is done** (see "Master + Mic Mute Live Sync" above) — but that mechanism only watches *one* device's mute bit. If the mute-all/unmute-one design above is built, this watcher will need to expand to track the multi-device aggregate state, not just one device's `BMuted` flag.
- **Third "partial" master-OLED icon state — flagged for further discussion before implementation, not specced.** Idea floated: if observed device states diverge from the intended target (mic + slash + exclamation mark icon, distinct from today's two-state slash/no-slash). Unresolved before this can be designed: exact definition of "partial" — since the asymmetric unmute-one-device design means *every* routine unmute leaves other devices still muted, "not all devices share state" can't be the trigger (that'd fire constantly); a tighter definition like "observed device states don't match what the last button action should have produced" was proposed but not confirmed. Also unresolved: what the RGB LED should show during that state (firmware side — see firmware CLAUDE.md "RGB button mic mute"). **Do not implement any part of the partial-state icon until this is revisited.**
- Entirely host-side except the partial-icon piece, which is firmware-side (tri-state `masterMuted`, new icon bitmap, `SET_MIC_MUTE_STATE` payload needs a third value).

### Per-Channel Mute from Host — maybe, depending on firmware flash availability; not a true todo

Idea: if a user mutes a specific app's session directly in the OS volume mixer, reflect that on the channel's physical mute LED/state on SERENITY. Deprioritized 2026-06-21 — judged an unlikely scenario, not worth the cost right now. Would need: a new device-bound protocol command (none exists — firmware's `processCmd` has no per-channel mute case, and adding one costs flash); host-side per-session mute support, which doesn't exist either — `session.go`'s `Session` interface only has a commented-out `// TODO: future mute support` (`GetMute()`/`SetMute()`), never implemented.

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

### Soft Takeover — Move to Host, and Extend to Connect-Time Sync — NOT DESIGNED, discussion only

**Current state:** soft takeover on per-channel mute/unmute lives entirely in firmware (see `kFaderOrder`-indexed `takeoverPending`/`takeoverTarget`/`takeoverSide` arrays and the freeze-until-crossed logic in `main.cpp`'s `updateMuteButtons()` and the serial send loop — see firmware CLAUDE.md). Separately, on host connect/power-up, faders 1–5 currently snap-jump app volume to the physical slider position instantly (see `serial.go`'s `handleLine` priming logic) — no takeover behavior at connect time.

**Idea discussed (2026-06-21), not yet designed:** consolidate the takeover decision logic (capture target, track which side the live value is on, freeze output until crossed) into the host alone, as a single shared implementation reusable for both (a) per-channel mute/unmute and (b) a new connect-time case: if a slider's physical position differs from the app's current volume on connect, freeze that channel's effective volume at the app's setpoint until the physical slider is moved through it, instead of snap-jumping.

**Why this looked appealing:** measured the actual flash cost of the firmware-side takeover logic by building a stripped copy — only ~176 bytes (0.6% of the 32KB budget), so flash savings is not the motivation. The real case for consolidating is avoiding two parallel implementations of the same crossing algorithm in two languages/places (firmware C++ for mute-unmute, host Go for connect-time) — one implementation is easier to reason about, test, and iterate on (host restart vs. ISP re-flash). The "firmware mute is autonomous without the host" objection does not hold — the device has no local audio path; nothing it does has any effect without the host translating serial data into Windows volume API calls, same as the faders. So host-only mute is not a functional regression.

**Real remaining cost, not yet resolved:** state sync across connect/reconnect. If the host becomes sole authority for mute/takeover state, the firmware's local `muted[]`/LED state and the host's notion of "is this channel muted" need to agree on every connect — the same class of problem already solved once for master volume (`pushMasterState`, see Feature 6 above and its "boots at 50%" bug writeup). Likely needs an equivalent "host pushes/learns per-channel mute state on connect" step, plus a decision on whether the mute LED stays instant/local (button press → LED, no serial round trip) even though the actual volume effect becomes host-mediated.

**Open questions, not addressed:** exact protocol change needed (new device→host "mute toggled" command; firmware would need to stop zeroing its own serial output and always send raw `cur[i]`, deferring the zero/freeze decision to host); how a slider mapped to multiple sessions (process groups) picks a single reference target volume to gate crossing on; whether this should be gated behind a config option or replace the snap-jump behavior outright.

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
| *(none currently)* | Custom HID report format (mic mute) was the only entry here — decided 2026-06-21: report ID 4, vendor-defined usage page 0xFF00, 1-byte payload (`kMicMuteDesc` in firmware `main.cpp`); see "System Mic Mute via HID" above |

---

## Reference

- Firmware repo and hardware ground truth: `C:\Users\Steven\Documents\Solid Models\deej\SERENITY Firmware\SERENITY-Firmware\CLAUDE.md`
- Upstream deej: https://github.com/omriharel/deej
