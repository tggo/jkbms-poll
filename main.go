//go:build linux

// jkbms-poll: connect to a JK-BMS over BLE, fetch one cell-info frame,
// parse it, and write JSON to /tmp/second_battary_bms.json.
//
// Target: JK-B2A8S20P (8S), JK02_32S BLE protocol.
// Reference: https://github.com/syssi/esphome-jk-bms (jk_bms_ble.cpp parser)
//
// Build only targets Linux because tinygo.org/x/bluetooth's BlueZ backend
// has the right Address layout there. Cross-build from Mac with:
//   GOOS=linux GOARCH=arm64 go build -o jkbms-poll ./...
// Tests (parse_test.go) don't import bluetooth, so they run anywhere.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tinygo.org/x/bluetooth"
)

const (
	defaultOut   = "/tmp/jkbms.json"
	defaultCells = 8

	// JK BMS BLE GATT — HM-10 style UART
	// Service: 0xFFE0, char 0xFFE1 (write + notify)
	charFFE1Short = 0xFFE1
)

// envOr returns the value of the named env var or the fallback.
func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

var (
	flagMAC      = flag.String("mac", envOr("JKBMS_MAC", ""), "BMS MAC address (or set JKBMS_MAC); REQUIRED")
	flagOut      = flag.String("out", envOr("JKBMS_OUT", defaultOut), "output JSON path (or JKBMS_OUT)")
	flagCells    = flag.Int("cells", defaultCells, "number of cells in this BMS (1..32)")
	flagAdapter  = flag.String("adapter", "", "Bluetooth adapter (default = system default)")
	flagTimeout  = flag.Duration("timeout", 90*time.Second, "overall timeout")
	flagLogLevel = flag.String("log", "info", "log level: debug | info | warn | error")
	flagLogJSON  = flag.Bool("log-json", false, "emit logs as JSON instead of text")
)

var log *slog.Logger

func setupLogger() {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(*flagLogLevel))); err != nil {
		fmt.Fprintf(os.Stderr, "bad -log %q: %v\n", *flagLogLevel, err)
		os.Exit(2)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if *flagLogJSON {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	log = slog.New(h)
}

// hexPreview returns up to first n bytes as space-separated hex.
func hexPreview(b []byte, n int) string {
	if n > len(b) {
		n = len(b)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%02x", b[i])
	}
	return strings.Join(parts, " ")
}

// JK02 "request all info" frame (20 bytes, last byte = sum mod 256).
// CMD 0x96 = read cell info; the BMS will start streaming 0x02 frames.
func buildRequest(cmd byte) []byte {
	f := make([]byte, 20)
	f[0], f[1], f[2], f[3] = 0xAA, 0x55, 0x90, 0xEB
	f[4] = cmd
	var sum byte
	for i := 0; i < 19; i++ {
		sum += f[i]
	}
	f[19] = sum
	return f
}

func main() {
	flag.Parse()
	setupLogger()

	if *flagMAC == "" {
		fmt.Fprintln(os.Stderr, "error: -mac is required (or set JKBMS_MAC env var)")
		flag.Usage()
		os.Exit(2)
	}

	log.Info("starting",
		"pid", os.Getpid(),
		"go", runtime.Version(),
		"goos", runtime.GOOS,
		"goarch", runtime.GOARCH,
		"timeout", *flagTimeout,
		"mac", *flagMAC,
		"cells", *flagCells,
		"out", *flagOut,
	)

	ctx, cancel := context.WithTimeout(context.Background(), *flagTimeout)
	defer cancel()

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		fail("adapter enable", "err", err)
	}
	log.Debug("adapter enabled")

	mac, err := bluetooth.ParseMAC(*flagMAC)
	if err != nil {
		fail("bad mac", "mac", *flagMAC, "err", err)
	}
	target := bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac}}

	// --- SCAN ---
	scanBudget := *flagTimeout - 20*time.Second
	if scanBudget < 15*time.Second {
		scanBudget = 15 * time.Second
	}
	scanCtx, scanCancel := context.WithTimeout(ctx, scanBudget)
	defer scanCancel()
	log.Info("scan starting", "budget", scanBudget, "target", target.String())

	var (
		seenTotal   uint64
		seenTarget  uint64
		seenUnique  sync.Map
		bestRSSI    int32 = -127
		lastTargetA       = time.Now()
	)
	found := make(chan bluetooth.ScanResult, 1)
	go func() {
		_ = adapter.Scan(func(a *bluetooth.Adapter, sr bluetooth.ScanResult) {
			atomic.AddUint64(&seenTotal, 1)
			addr := sr.Address.String()
			if _, dup := seenUnique.LoadOrStore(addr, sr.RSSI); !dup {
				log.Debug("scan advert (new device)",
					"addr", addr, "rssi", sr.RSSI, "name", sr.LocalName())
			}
			if strings.EqualFold(addr, target.String()) {
				atomic.AddUint64(&seenTarget, 1)
				if int32(sr.RSSI) > atomic.LoadInt32(&bestRSSI) {
					atomic.StoreInt32(&bestRSSI, int32(sr.RSSI))
				}
				if time.Since(lastTargetA) > 1500*time.Millisecond {
					log.Debug("scan target advert",
						"n", atomic.LoadUint64(&seenTarget),
						"rssi", sr.RSSI,
						"best_rssi", atomic.LoadInt32(&bestRSSI))
					lastTargetA = time.Now()
				}
				select {
				case found <- sr:
					_ = a.StopScan()
				default:
				}
			}
		})
	}()

	select {
	case sr := <-found:
		log.Info("scan locked target",
			"addr", sr.Address.String(),
			"rssi", sr.RSSI,
			"adverts_total", atomic.LoadUint64(&seenTotal),
			"adverts_target", atomic.LoadUint64(&seenTarget))
	case <-scanCtx.Done():
		_ = adapter.StopScan()
		fail("scan timeout, target not seen",
			"target", target.String(),
			"adverts_total", atomic.LoadUint64(&seenTotal),
			"unique_devices", countSyncMap(&seenUnique))
	}

	// --- CONNECT ---
	log.Info("connect starting", "max_attempts", 5, "per_attempt_timeout", "15s")
	var dev bluetooth.Device
	for attempt := 1; attempt <= 5; attempt++ {
		t0 := time.Now()
		dev, err = adapter.Connect(target, bluetooth.ConnectionParams{
			ConnectionTimeout: bluetooth.NewDuration(15 * time.Second),
		})
		dt := time.Since(t0).Round(time.Millisecond)
		if err == nil {
			log.Info("connect ok",
				"attempt", attempt, "took", dt, "best_rssi", atomic.LoadInt32(&bestRSSI))
			break
		}
		log.Warn("connect attempt failed", "attempt", attempt, "took", dt, "err", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		fail("connect after retries",
			"err", err,
			"best_rssi", atomic.LoadInt32(&bestRSSI),
			"hint", "RSSI too weak; move closer or use ESP32 BLE proxy")
	}
	defer func() {
		log.Debug("disconnecting")
		_ = dev.Disconnect()
	}()

	// --- DISCOVER ---
	log.Debug("discovering services")
	services, err := dev.DiscoverServices(nil)
	if err != nil {
		fail("discover services", "err", err)
	}
	log.Info("services discovered", "count", len(services))

	var ffe1 bluetooth.DeviceCharacteristic
	var foundChar bool
	for _, svc := range services {
		chars, err := svc.DiscoverCharacteristics(nil)
		if err != nil {
			log.Warn("discover chars failed", "service", svc.UUID().String(), "err", err)
			continue
		}
		uuids := make([]string, 0, len(chars))
		for _, c := range chars {
			uuids = append(uuids, c.UUID().String())
			if u := c.UUID(); u.Get16Bit() == charFFE1Short && !foundChar {
				ffe1 = c
				foundChar = true
			}
		}
		log.Debug("service",
			"uuid", svc.UUID().String(),
			"chars", strings.Join(uuids, ","),
			"char_count", len(chars))
	}
	if !foundChar {
		fail("FFE1 characteristic not found in any service")
	}
	log.Info("FFE1 locked", "char_uuid", ffe1.UUID().String())

	// --- NOTIFY + REASSEMBLY ---
	frameCh := make(chan []byte, 4)
	var (
		asm        []byte
		notifCount uint64
		byteCount  uint64
	)
	if err := ffe1.EnableNotifications(func(b []byte) {
		nc := atomic.AddUint64(&notifCount, 1)
		atomic.AddUint64(&byteCount, uint64(len(b)))
		// Verbose chunk-level log only at debug, throttled.
		if nc <= 8 || nc%10 == 0 {
			log.Debug("notif",
				"n", nc, "len", len(b), "asm_before", len(asm),
				"head", hexPreview(b, 12))
		}
		if len(b) >= 4 && b[0] == 0x55 && b[1] == 0xAA && b[2] == 0xEB && b[3] == 0x90 {
			if len(asm) > 0 {
				log.Debug("frame header mid-stream, discarding partial", "discard_len", len(asm))
			}
			asm = append(asm[:0], b...)
		} else {
			asm = append(asm, b...)
		}
		if len(asm) >= frameLen {
			cp := make([]byte, frameLen)
			copy(cp, asm[:frameLen])
			asm = asm[frameLen:]
			log.Info("frame complete",
				"type", fmt.Sprintf("0x%02x", cp[4]),
				"counter", fmt.Sprintf("0x%02x", cp[5]),
				"crc_byte", fmt.Sprintf("0x%02x", cp[frameLen-1]),
				"leftover", len(asm))
			select {
			case frameCh <- cp:
			default:
				log.Warn("frameCh full, dropping frame")
			}
		}
	}); err != nil {
		fail("enable notifications", "err", err)
	}
	log.Debug("notifications enabled")

	// --- REQUEST ---
	req := buildRequest(0x96)
	log.Debug("writing REQUEST_CELL_INFO", "cmd", "0x96", "frame", hex.EncodeToString(req))
	t0 := time.Now()
	if _, err := ffe1.WriteWithoutResponse(req); err != nil {
		log.Warn("WriteWithoutResponse failed, falling back to Write", "err", err)
		if _, err2 := ffe1.Write(req); err2 != nil {
			fail("write request",
				"WriteWithoutResponse_err", err, "Write_err", err2)
		}
	}
	log.Debug("request written", "took", time.Since(t0).Round(time.Millisecond))

	// --- WAIT FOR FRAME ---
	deadline := time.NewTimer(*flagTimeout)
	defer deadline.Stop()
	statusTick := time.NewTicker(2 * time.Second)
	defer statusTick.Stop()
	for {
		select {
		case f := <-frameCh:
			if f[4] != 0x02 {
				log.Debug("ignoring non-cell-info frame", "type", fmt.Sprintf("0x%02x", f[4]))
				continue
			}
			res, err := ParseJK02CellInfo(f, *flagCells, target.String())
			if err != nil {
				fail("parse", "err", err, "head", hexPreview(f, 32))
			}
			out, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				fail("marshal", "err", err)
			}
			if err := os.WriteFile(*flagOut, append(out, '\n'), 0o644); err != nil {
				fail("write output", "path", *flagOut, "err", err)
			}
			log.Info("parsed cells",
				"min_v", res.CellMinV, "min_num", res.CellMinNum,
				"max_v", res.CellMaxV, "max_num", res.CellMaxNum,
				"delta_v", res.CellDeltaV, "avg_v", res.CellAvgV,
				"voltages", res.CellVoltagesV)
			log.Info("parsed pack",
				"v", res.BatteryVoltageV, "a", res.BatteryCurrentA, "w", res.BatteryPowerW,
				"soc", res.SOCPercent, "soh", res.SOHPercent,
				"t1", res.T1C, "t2", res.T2C, "mos", res.PowerTubeTempC)
			log.Info("parsed capacity",
				"cap_rem_ah", res.RemainingAh, "nom_ah", res.NominalAh,
				"cycles", res.CycleCount, "cycle_cap_ah", res.CycleCapacityAh,
				"runtime_s", res.TotalRuntimeS,
				"charging", res.Charging, "discharging", res.Discharging,
				"err_bitmask", fmt.Sprintf("0x%04x", res.ErrorBitmask),
				"crc_ok", res.CRCOK)
			fmt.Printf("wrote %s (V=%.3f I=%.3fA SOC=%d%% Δ=%.3fV crc_ok=%v)\n",
				*flagOut, res.BatteryVoltageV, res.BatteryCurrentA,
				res.SOCPercent, res.CellDeltaV, res.CRCOK)
			return
		case <-statusTick.C:
			log.Debug("waiting frame",
				"notifs", atomic.LoadUint64(&notifCount),
				"bytes", atomic.LoadUint64(&byteCount),
				"asm", len(asm))
		case <-deadline.C:
			fail("timeout waiting for cell-info frame",
				"notifs", atomic.LoadUint64(&notifCount),
				"bytes", atomic.LoadUint64(&byteCount),
				"asm", len(asm))
		}
	}
}

// fail logs at error level and exits 1.
func fail(msg string, args ...any) {
	if log != nil {
		log.Error(msg, args...)
	} else {
		fmt.Fprintf(os.Stderr, "jkbms-poll: %s %v\n", msg, args)
	}
	os.Exit(1)
}

func countSyncMap(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}
