# jkbms-poll

A small Go program that connects to a JK-BMS over Bluetooth Low Energy, reads
one cell-info frame, and writes it as JSON to a file. One-shot — connect,
read, exit.

Built for embedding in a polling cron / systemd timer on a Linux host that
sits near the BMS (Raspberry Pi, OpenWRT box, ESP-running-Linux, …) and
having the JSON consumed by Home Assistant, Grafana, scripts, whatever.

## What's supported

- **Protocol:** JK02_32S (modern firmware, 4S–32S, including JK-B2A8S20P,
  JK-B2A16S15P, JK-PB2A16S20P, etc.)
- **Connection:** BlueZ on Linux, via [`tinygo.org/x/bluetooth`].
- **Output:** one JSON file per invocation, with parsed fields and the
  full raw 300-byte frame in hex (so re-parsing later, after firmware
  layout changes, stays possible).

[`tinygo.org/x/bluetooth`]: https://github.com/tinygo-org/bluetooth

## Build

```sh
# native
go build -o jkbms-poll ./...

# cross-compile for a Pi (arm64)
GOOS=linux GOARCH=arm64 go build -o jkbms-poll-linux-arm64 ./...

# or
make build                  # default linux/arm64
make build GOARCH=amd64     # x86_64 Linux
make build-host             # whatever you're on
```

Linux only — the `tinygo.org/x/bluetooth` BlueZ backend uses fields that
don't exist on the Darwin/Windows backends, so `main.go` is gated with
`//go:build linux`. Tests are platform-independent.

## Run

```sh
./jkbms-poll -mac AA:BB:CC:DD:EE:FF -cells 8 -out /tmp/jkbms.json
```

`-mac` is required (no built-in default). Find it with `bluetoothctl scan le`
on the host while you're near the BMS, or in the JK BMS phone app.

Useful flags:

| Flag | Default | Notes |
|------|---------|-------|
| `-mac` | (required) | BMS BLE address. Also `JKBMS_MAC` env var. |
| `-cells` | `8` | Number of cells in your pack (1..32). |
| `-out` | `/tmp/jkbms.json` | Output JSON path. Also `JKBMS_OUT`. |
| `-timeout` | `90s` | Total budget for scan + connect + first frame. |
| `-log` | `info` | `debug` / `info` / `warn` / `error`. |
| `-log-json` | off | Emit logs as JSON (slog `JSONHandler`). |

The program exits 0 once the JSON is written, non-zero on any error
(scan timeout, connect failure, parse failure, …). Wire it into systemd
or cron for periodic polling.

### Sample run

```
$ jkbms-poll -mac C8:47:80:14:CC:CC -log debug
INFO  starting pid=17926 timeout=1m30s mac=C8:47:80:14:CC:CC cells=8
DEBUG adapter enabled
INFO  scan starting budget=1m10s
DEBUG scan advert (new device) addr=C8:47:80:14:CC:CC rssi=-65 name=Second_24v
INFO  scan locked target rssi=-65
INFO  connect ok attempt=1 took=420ms best_rssi=-65
INFO  services discovered count=4
INFO  FFE1 locked
INFO  frame complete type=0x02 counter=0xac crc_byte=0xd8 leftover=0
INFO  parsed pack v=53.224 a=31.881 w=1696.8 soc=25 t1=13.4 t2=12.8 mos=12.9
wrote /tmp/jkbms.json (V=53.224 I=31.881A SOC=25% Δ=0.017V crc_ok=true)
```

## Output schema

```jsonc
{
  "timestamp_unix": 1715300000,
  "timestamp_iso":  "2026-05-10T00:13:20Z",
  "bms_address":    "AA:BB:CC:DD:EE:FF",
  "frame_type":     2,
  "frame_counter":  172,
  "crc_ok":         true,
  "num_cells":      8,
  "cell_voltages_v":      [3.333, 3.326, ...],
  "cell_resistances_mohm":[0.064, 0.061, ...],
  "enabled_cell_mask":    255,
  "cell_avg_v":  3.328,
  "cell_min_v":  3.320,
  "cell_max_v":  3.337,
  "cell_delta_v":0.017,
  "cell_min_num":13,    // 1-based
  "cell_max_num":16,    // 1-based
  "battery_voltage_v":  53.224,
  "battery_current_a":  31.881,   // + = charge, - = discharge
  "battery_power_w":    1696.8,
  "power_tube_temp_c":  12.9,
  "t1_c":               13.4,
  "t2_c":               12.8,
  "balance_current_a":  0.0,
  "balance_status":     0,        // 0=off, 1=charging-bal, 2=discharging-bal
  "error_bitmask":      0,
  "soc_percent":        25,
  "soh_percent":        100,
  "remaining_capacity_ah":49.286,
  "nominal_capacity_ah": 200.000,
  "cycle_count":         9,
  "cycle_capacity_ah":   1.853,
  "total_runtime_s":     24530060,
  "charging":            true,
  "discharging":         true,
  "raw_frame_hex":       "55aaeb9002ac…"
}
```

## How it works

1. **Scan** until BlueZ has seen an advert from the target MAC (BlueZ won't
   let you connect to a device path it hasn't observed recently).
2. **Connect** with retries (BLE at the edge of range often aborts the
   first handshake locally).
3. **Discover** GATT services, lock characteristic `0xFFE1` (the JK BMS
   uses an HM-10 / Bluetrum UART-over-BLE module: write+notify on `FFE1`
   inside service `FFE0`).
4. **Subscribe** to notifications and **write** a 20-byte `REQUEST_CELL_INFO`
   command (`AA 55 90 EB 96 00…00 + sum_mod_256`).
5. **Reassemble** notifications (BLE chunks ~20 B each) into a 300-byte
   frame keyed by the `55 AA EB 90` start sequence.
6. **Parse** the JK02_32S layout into the schema above and write JSON.
7. **Exit.**

## Parser

Field offsets are mirrored from
[`syssi/esphome-jk-bms`](https://github.com/syssi/esphome-jk-bms)
(`components/jk_bms_ble/jk_bms_ble.cpp`, `decode_jk02_cell_info_`).

`parse_test.go` ships a real captured frame (from the upstream test
fixtures, JK_PB2A16S15P running fw 15.38) and asserts the parsed
fields against upstream's known-good values — voltages, current,
temperatures, SOC, capacity, cycle count, runtime, MOSFET state.
Run with:

```sh
go test ./...
```

If you hit a frame that doesn't parse cleanly on different firmware,
the `raw_frame_hex` in the output is enough to add a new fixture and
extend the parser.

## Limitations / known nots

- **JK04 firmware is not supported** — only JK02_32S. JK04 is a different
  layout and isn't on my BMS to test against.
- **Read-only** — the program never sends control commands (no MOSFET
  toggles, no settings writes). Adding writes is straightforward (the
  `buildRequest` builder already produces a valid command frame) but
  intentionally not exposed.
- **Single shot** — there's no built-in daemon mode. Run it on a timer.
  This keeps the BLE stack on the BMS side rested between polls and
  recovers cleanly from disconnects.
- **Range-bound by BlueZ** — at RSSI below roughly -85 dBm, BlueZ tends
  to abort connections locally (`le-connection-abort-by-local`). If the
  host can't be moved closer to the BMS, an ESP32 running ESPHome's
  `bluetooth_proxy` is a far better solution than a stronger Pi antenna.

## License

MIT. See [`LICENSE`](LICENSE).

## Acknowledgements

- The JK02_32S protocol layout, reverse-engineered and maintained by the
  [`syssi/esphome-jk-bms`](https://github.com/syssi/esphome-jk-bms)
  contributors. This project's parser is a Go port of theirs and uses
  their test fixtures as ground truth.
- [`tinygo.org/x/bluetooth`](https://github.com/tinygo-org/bluetooth) for
  the BLE plumbing.
