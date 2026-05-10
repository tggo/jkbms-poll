# CLAUDE.md

Context file for Claude / other coding assistants working in this repo.
Mirrors what a human contributor would want to know up front.

## What this repo is

`jkbms-poll` — a Linux Go binary that connects to a JK-BMS (battery management
system) over Bluetooth Low Energy, decodes one cell-info frame, writes JSON,
exits. See `README.md` for the user-facing description.

Designed to run on a Raspberry Pi (or any Linux host with BlueZ) close to
the BMS, fired off periodically by cron / systemd, JSON consumed by Home
Assistant or similar.

## Layout

```
main.go         BLE: scan → connect → discover → notify → reassemble → JSON.
                Build-tagged //go:build linux because tinygo.org/x/bluetooth's
                Address struct only has the fields we touch on the BlueZ backend.
parse.go        Pure JK02_32S frame decoder. No bluetooth import. Runs anywhere.
parse_test.go   Tests against an upstream fixture (frames_jk02_32s_v15.h from
                syssi/esphome-jk-bms) — Go output is held to the same numbers
                their C++ tests expect.
.env.example    Documents the env vars that .env should set. Real .env is
                gitignored.
Makefile        test / build / build-host / deploy / run / clean.
```

## Build & test

```sh
go test ./...                                # works on any OS (parse only)
go build -o jkbms-poll ./...                 # native (Linux only)
GOOS=linux GOARCH=arm64 go build ./...       # cross-compile from a Mac for a Pi
```

Tests must stay platform-independent. Don't import `tinygo.org/x/bluetooth`
from `parse.go` or `parse_test.go`.

## Editing rules of thumb

- **Parser changes need a test.** Every field decoded from a fixed offset is
  a place where firmware drift can silently break things. If you add or move
  a field, extend `parse_test.go` so the upstream fixture catches it.
- **`raw_frame_hex` stays in the output.** That's the escape hatch for
  re-parsing historical data when offsets change.
- **No control writes.** The binary intentionally stays read-only. Don't add
  a `-set-balance` flag or similar without a clear discussion — toggling a
  MOSFET on a real BMS over flaky BLE is a way to brick a battery.
- **No daemon mode.** Single-shot is on purpose; it sidesteps an entire class
  of "BLE stack got wedged after 14 hours" bugs that long-running BLE clients
  invariably hit. Cron is a feature.
- **Logging is `slog`-based.** `log.Info / log.Debug / log.Warn` with key=value
  pairs. Don't go back to `fmt.Fprintf(os.Stderr, …)` — the structured fields
  (RSSI, attempt counts, byte counters) are the whole point.

## Common gotchas

- **`le-connection-abort-by-local` after a successful scan.** Almost always
  signal-strength, not a bug. Below ~-85 dBm BlueZ gives up the handshake.
  Move the host or use an ESP32 BLE proxy.
- **`Method "Get" with signature "ss" on interface DBus.Properties doesn't exist`.**
  Means BlueZ didn't have a recent advert for the device path. Re-scan first
  — the code already does this; if you see it, the scan window was too short
  or the device went silent.
- **First frame arrives, but `frame_type != 0x02`.** The BMS streams several
  frame types (`0x02` cell info, `0x03` device info, `0x01` settings). The
  loop already filters on `0x02`. Don't change that without understanding
  the JK02 protocol — non-cell frames have completely different layouts.
- **Cell count mismatch.** `-cells N` controls how many of the 32 voltage /
  resistance slots end up in the JSON. The BMS always writes 32 slots; the
  trailing ones are zero on smaller packs. If your delta_v looks negative,
  you set `-cells` higher than the actual pack and got zeroed slots in the
  min calculation.

## Reference

JK02_32S protocol layout: see comments at the top of `parse.go`, ultimately
sourced from <https://github.com/syssi/esphome-jk-bms> —
`components/jk_bms_ble/jk_bms_ble.cpp` (`decode_jk02_cell_info_`).
