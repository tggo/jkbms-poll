// JK-BMS JK02_32S cell-info frame decoder.
// Reference: syssi/esphome-jk-bms, components/jk_bms_ble/jk_bms_ble.cpp
//
// Frame layout (JK02_32S, 300 bytes total, last byte = sum(0..298) mod 256):
//
//   byte  size  field
//   0     4     header 0x55 0xAA 0xEB 0x90
//   4     1     frame type (0x02 = cell info)
//   5     1     frame counter
//   6     64    32 cell voltages, uint16_le mV  (only first numCells are populated)
//   70    4     enabled-cells bitmask, uint32_le
//   74    2     average cell voltage, uint16_le mV
//   76    2     delta cell voltage, uint16_le mV
//   78    1     max-voltage cell index (0-based)
//   79    1     min-voltage cell index (0-based)
//   80    64    32 cell resistances, uint16_le mΩ
//   144   2     power-tube (MOS) temperature, int16_le ×0.1 °C
//   146   4     wire-resistance warning bitmask
//   150   4     battery voltage, uint32_le ×0.001 V
//   154   4     battery power, uint32_le ×0.001 W (unsigned, see note)
//   158   4     battery current, int32_le ×0.001 A (positive=charge)
//   162   2     T1, int16_le ×0.1 °C
//   164   2     T2, int16_le ×0.1 °C
//   166   2     errors bitmask, uint16_le
//   170   2     balance current, int16_le ×0.001 A
//   172   1     balancing action (0=off, 1=charge bal, 2=discharge bal)
//   173   1     state of charge, %
//   174   4     remaining capacity, uint32_le ×0.001 Ah
//   178   4     nominal capacity, uint32_le ×0.001 Ah
//   182   4     cycle count, uint32_le
//   186   4     total cycle capacity, uint32_le ×0.001 Ah
//   190   1     state of health, %
//   194   4     total runtime, uint32_le seconds
//   198   1     charging mosfet on
//   199   1     discharging mosfet on
//
// Field "battery_power_w" we compute as voltage*current rather than reading the
// unsigned big-endian word at 154 (matches what the upstream component does).

package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

const frameLen = 300

type Result struct {
	TimestampUnix      int64     `json:"timestamp_unix"`
	TimestampISO       string    `json:"timestamp_iso"`
	BMSAddress         string    `json:"bms_address"`
	FrameType          byte      `json:"frame_type"`
	FrameCounter       byte      `json:"frame_counter"`
	CRCOK              bool      `json:"crc_ok"`
	NumCells           int       `json:"num_cells"`
	CellVoltagesV      []float64 `json:"cell_voltages_v"`
	CellResistancesMOh []float64 `json:"cell_resistances_mohm"`
	EnabledCellMask    uint32    `json:"enabled_cell_mask"`
	CellAvgV           float64   `json:"cell_avg_v"`
	CellMinV           float64   `json:"cell_min_v"`
	CellMaxV           float64   `json:"cell_max_v"`
	CellDeltaV         float64   `json:"cell_delta_v"`
	CellMinNum         int       `json:"cell_min_num"` // 1-based, like upstream
	CellMaxNum         int       `json:"cell_max_num"` // 1-based, like upstream
	BatteryVoltageV    float64   `json:"battery_voltage_v"`
	BatteryCurrentA    float64   `json:"battery_current_a"`
	BatteryPowerW      float64   `json:"battery_power_w"`
	PowerTubeTempC     float64   `json:"power_tube_temp_c"`
	T1C                float64   `json:"t1_c"`
	T2C                float64   `json:"t2_c"`
	BalanceCurrentA    float64   `json:"balance_current_a"`
	BalanceStatus      byte      `json:"balance_status"`
	ErrorBitmask       uint16    `json:"error_bitmask"`
	SOCPercent         byte      `json:"soc_percent"`
	SOHPercent         byte      `json:"soh_percent"`
	RemainingAh        float64   `json:"remaining_capacity_ah"`
	NominalAh          float64   `json:"nominal_capacity_ah"`
	CycleCount         uint32    `json:"cycle_count"`
	CycleCapacityAh    float64   `json:"cycle_capacity_ah"`
	TotalRuntimeS      uint32    `json:"total_runtime_s"`
	Charging           bool      `json:"charging"`
	Discharging        bool      `json:"discharging"`
	RawHex             string    `json:"raw_frame_hex"`
}

func crcSum(buf []byte) byte {
	var s byte
	for _, b := range buf {
		s += b
	}
	return s
}

// ParseJK02CellInfo parses a 300-byte JK02_32S 0x02 frame.
// numCells is how many of the 32 cell slots to expose in CellVoltagesV /
// CellResistancesMOh — typically 8 for a B2A8S20P, 16 for a 16S BMS, etc.
// The min/max cell numbers are computed from the voltages themselves
// (matching the upstream esphome-jk-bms behavior).
func ParseJK02CellInfo(buf []byte, numCells int, mac string) (*Result, error) {
	if len(buf) < frameLen {
		return nil, fmt.Errorf("short frame: %d bytes, want %d", len(buf), frameLen)
	}
	if !(buf[0] == 0x55 && buf[1] == 0xAA && buf[2] == 0xEB && buf[3] == 0x90) {
		return nil, fmt.Errorf("bad header %02x %02x %02x %02x", buf[0], buf[1], buf[2], buf[3])
	}
	if buf[4] != 0x02 {
		return nil, fmt.Errorf("not a cell-info frame, type=0x%02x", buf[4])
	}
	if numCells <= 0 || numCells > 32 {
		return nil, fmt.Errorf("invalid numCells %d, must be 1..32", numCells)
	}

	r := &Result{
		BMSAddress:   mac,
		FrameType:    buf[4],
		FrameCounter: buf[5],
		CRCOK:        crcSum(buf[:frameLen-1]) == buf[frameLen-1],
		NumCells:     numCells,
		RawHex:       hex.EncodeToString(buf),
	}

	// Cell voltages and self-derived stats.
	r.CellVoltagesV = make([]float64, numCells)
	r.CellResistancesMOh = make([]float64, numCells)
	minV, maxV := 100.0, -100.0
	minIdx, maxIdx := 0, 0
	var sum float64
	enabled := 0
	for i := 0; i < numCells; i++ {
		v := float64(binary.LittleEndian.Uint16(buf[6+i*2:])) / 1000.0
		res := float64(binary.LittleEndian.Uint16(buf[80+i*2:])) / 1000.0
		r.CellVoltagesV[i] = v
		r.CellResistancesMOh[i] = res
		if v > 0 {
			sum += v
			enabled++
			if v < minV {
				minV = v
				minIdx = i + 1
			}
		}
		if v > maxV {
			maxV = v
			maxIdx = i + 1
		}
	}
	if enabled > 0 {
		r.CellAvgV = sum / float64(enabled)
	}
	r.CellMinV = minV
	r.CellMaxV = maxV
	r.CellDeltaV = maxV - minV
	r.CellMinNum = minIdx
	r.CellMaxNum = maxIdx

	r.EnabledCellMask = binary.LittleEndian.Uint32(buf[70:])
	r.PowerTubeTempC = float64(int16(binary.LittleEndian.Uint16(buf[144:]))) / 10.0
	r.BatteryVoltageV = float64(binary.LittleEndian.Uint32(buf[150:])) / 1000.0
	r.BatteryCurrentA = float64(int32(binary.LittleEndian.Uint32(buf[158:]))) / 1000.0
	r.BatteryPowerW = r.BatteryVoltageV * r.BatteryCurrentA
	r.T1C = float64(int16(binary.LittleEndian.Uint16(buf[162:]))) / 10.0
	r.T2C = float64(int16(binary.LittleEndian.Uint16(buf[164:]))) / 10.0
	r.ErrorBitmask = binary.LittleEndian.Uint16(buf[166:])
	r.BalanceCurrentA = float64(int16(binary.LittleEndian.Uint16(buf[170:]))) / 1000.0
	r.BalanceStatus = buf[172]
	r.SOCPercent = buf[173]
	r.RemainingAh = float64(binary.LittleEndian.Uint32(buf[174:])) / 1000.0
	r.NominalAh = float64(binary.LittleEndian.Uint32(buf[178:])) / 1000.0
	r.CycleCount = binary.LittleEndian.Uint32(buf[182:])
	r.CycleCapacityAh = float64(binary.LittleEndian.Uint32(buf[186:])) / 1000.0
	r.SOHPercent = buf[190]
	r.TotalRuntimeS = binary.LittleEndian.Uint32(buf[194:])
	r.Charging = buf[198] != 0
	r.Discharging = buf[199] != 0

	now := time.Now()
	r.TimestampUnix = now.Unix()
	r.TimestampISO = now.UTC().Format(time.RFC3339)
	return r, nil
}
