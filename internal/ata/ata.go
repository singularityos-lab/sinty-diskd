// Package ata reads the identity and SMART health of ATA drives through
// ATA PASS-THROUGH commands sent with the SCSI generic SG_IO interface.
package ata

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const sgIO = 0x2285

type sgIOHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         uintptr
	cmdp           uintptr
	sbp            uintptr
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         uintptr
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

const (
	dxferNone    = -1
	dxferFromDev = -3

	cmdIdentify = 0xEC
	cmdSmart    = 0xB0

	smartReadData   = 0xD0
	smartReadThresh = 0xD1
	smartEnable     = 0xD8
	smartExecOffln  = 0xD4
	smartStatus     = 0xDA
)

// Device is an open ATA drive.
type Device struct{ f *os.File }

// Open opens the whole-disk node of an ATA drive.
func Open(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return &Device{f}, nil
}

// Close closes the drive.
func (d *Device) Close() error { return d.f.Close() }

// passthrough sends one ATA command. With data it reads one 512-byte sector
// in PIO; without, it is a non-data command whose result registers come back
// in the sense data.
func (d *Device) passthrough(command, feature, lbaLow byte, data []byte) ([]byte, error) {
	cdb := make([]byte, 16)
	sense := make([]byte, 32)
	cdb[0] = 0x85
	h := sgIOHdr{interfaceID: 'S', cmdLen: 16, mxSbLen: uint8(len(sense)), timeout: 15000}
	if data != nil {
		cdb[1] = 4 << 1
		cdb[2] = 0x0e
		h.dxferDirection = dxferFromDev
		h.dxferLen = uint32(len(data))
		h.dxferp = uintptr(unsafe.Pointer(&data[0]))
	} else {
		cdb[1] = 3 << 1
		cdb[2] = 0x20
		h.dxferDirection = dxferNone
	}
	cdb[4] = feature
	cdb[6] = 1
	cdb[8] = lbaLow
	cdb[10] = 0x4F
	cdb[12] = 0xC2
	cdb[14] = command
	h.cmdp = uintptr(unsafe.Pointer(&cdb[0]))
	h.sbp = uintptr(unsafe.Pointer(&sense[0]))
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), sgIO, uintptr(unsafe.Pointer(&h))); errno != 0 {
		return nil, errno
	}
	if h.hostStatus != 0 || (h.driverStatus&0xf) != 0 && h.driverStatus != 0x08 {
		return nil, errors.New("ATA command failed")
	}
	return sense, nil
}

// Identity is what IDENTIFY DEVICE reports about SMART and power features.
type Identity struct {
	SmartSupported, SmartEnabled               bool
	PmSupported, PmEnabled                     bool
	ApmSupported, ApmEnabled                   bool
	AamSupported, AamEnabled                   bool
	WriteCacheSupported, WriteCacheEnabled     bool
	ReadLookaheadSupported, ReadLookahead      bool
	SecurityEraseMinutes, EnhancedEraseMinutes int32
	SecurityFrozen                             bool
	RotationRate                               int32
}

// ParseIdentify decodes IDENTIFY DEVICE data.
func ParseIdentify(b []byte) Identity {
	w := func(n int) uint16 { return binary.LittleEndian.Uint16(b[2*n:]) }
	bit := func(word, n int) bool { return w(word)&(1<<n) != 0 }
	id := Identity{
		SmartSupported: bit(82, 0), SmartEnabled: bit(85, 0),
		PmSupported: bit(82, 3), PmEnabled: bit(85, 3),
		WriteCacheSupported: bit(82, 5), WriteCacheEnabled: bit(85, 5),
		ReadLookaheadSupported: bit(82, 6), ReadLookahead: bit(85, 6),
		ApmSupported: bit(83, 3), ApmEnabled: bit(86, 3),
		AamSupported: bit(83, 9), AamEnabled: bit(86, 9),
		SecurityFrozen: bit(128, 3),
	}
	minutes := func(v uint16) int32 {
		if v == 0 {
			return 0
		}
		if v&0x8000 != 0 {
			return int32(v&0x7fff) * 2
		}
		return int32(v&0xff) * 2
	}
	id.SecurityEraseMinutes = minutes(w(89))
	id.EnhancedEraseMinutes = minutes(w(90))
	switch r := w(217); {
	case r == 1:
		id.RotationRate = 0
	case r >= 0x401 && r <= 0xfffe:
		id.RotationRate = int32(r)
	default:
		id.RotationRate = -1
	}
	return id
}

// Identify runs IDENTIFY DEVICE.
func (d *Device) Identify() (Identity, error) {
	buf := make([]byte, 512)
	if _, err := d.passthrough(cmdIdentify, 0, 0, buf); err != nil {
		return Identity{}, err
	}
	return ParseIdentify(buf), nil
}

// Attribute is one SMART attribute.
type Attribute struct {
	ID        uint8
	Name      string
	Flags     uint16
	Value     int32
	Worst     int32
	Threshold int32
	Raw       uint64
}

// Failing reports whether the attribute is below its threshold now.
func (a Attribute) Failing() bool { return a.Threshold > 0 && a.Value <= a.Threshold }

// FailedInThePast reports whether the worst value went below the threshold.
func (a Attribute) FailedInThePast() bool { return a.Threshold > 0 && a.Worst <= a.Threshold }

var names = map[uint8]string{
	1: "raw-read-error-rate", 3: "spin-up-time", 4: "start-stop-count", 5: "reallocated-sector-count",
	7: "seek-error-rate", 9: "power-on-hours", 10: "spin-retry-count", 12: "power-cycle-count",
	177: "wear-leveling-count", 187: "reported-uncorrect", 188: "command-timeout", 190: "airflow-temperature-celsius",
	192: "power-off-retract-count", 193: "load-cycle-count", 194: "temperature-celsius-2", 196: "reallocated-event-count",
	197: "current-pending-sector", 198: "offline-uncorrectable", 199: "udma-crc-error-count", 241: "total-lbas-written",
	242: "total-lbas-read",
}

// Smart is the SMART READ DATA page joined with the thresholds.
type Smart struct {
	Attributes        []Attribute
	SelftestStatus    string
	SelftestRemaining int32
}

var selftestStatus = map[uint8]string{
	0: "success", 1: "aborted", 2: "interrupted", 3: "fatal", 4: "error_unknown",
	5: "error_electrical", 6: "error_servo", 7: "error_read", 8: "error_handling", 15: "inprogress",
}

// ParseSmart joins a SMART READ DATA page with its thresholds page.
func ParseSmart(data, thresholds []byte) Smart {
	th := map[uint8]int32{}
	for i := 0; i < 30; i++ {
		e := thresholds[2+12*i:]
		if e[0] != 0 {
			th[e[0]] = int32(e[1])
		}
	}
	var s Smart
	for i := 0; i < 30; i++ {
		e := data[2+12*i:]
		if e[0] == 0 {
			continue
		}
		raw := uint64(0)
		for j := 5; j >= 0; j-- {
			raw = raw<<8 | uint64(e[5+j])
		}
		name := names[e[0]]
		if name == "" {
			name = "attribute-" + itoa(int(e[0]))
		}
		s.Attributes = append(s.Attributes, Attribute{ID: e[0], Name: name, Flags: binary.LittleEndian.Uint16(e[1:]),
			Value: int32(e[3]), Worst: int32(e[4]), Threshold: th[e[0]], Raw: raw})
	}
	st := data[363]
	s.SelftestStatus = selftestStatus[st>>4]
	if s.SelftestStatus == "" {
		s.SelftestStatus = "error_unknown"
	}
	s.SelftestRemaining = -1
	if st>>4 == 15 {
		s.SelftestRemaining = int32(st&0xf) * 10
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ReadSmart reads the SMART attributes and thresholds.
func (d *Device) ReadSmart() (Smart, error) {
	data := make([]byte, 512)
	if _, err := d.passthrough(cmdSmart, smartReadData, 0, data); err != nil {
		return Smart{}, err
	}
	th := make([]byte, 512)
	if _, err := d.passthrough(cmdSmart, smartReadThresh, 0, th); err != nil {
		return Smart{}, err
	}
	return ParseSmart(data, th), nil
}

// Failing asks the drive whether it predicts its own failure.
func (d *Device) Failing() (bool, error) {
	sense, err := d.passthrough(cmdSmart, smartStatus, 0, nil)
	if err != nil {
		return false, err
	}
	desc := sense[8:]
	if sense[0]&0x7f == 0x72 && desc[0] == 0x09 {
		return desc[9] == 0xf4 && desc[11] == 0x2c, nil
	}
	return false, nil
}

// SelfTest starts a short, extended or conveyance self-test, or aborts one.
func (d *Device) SelfTest(kind string) error {
	codes := map[string]byte{"short": 1, "extended": 2, "conveyance": 3, "abort": 127}
	code, ok := codes[kind]
	if !ok {
		return errors.New("unknown self-test " + kind)
	}
	_, err := d.passthrough(cmdSmart, smartExecOffln, code, nil)
	return err
}

// Enable turns SMART on.
func (d *Device) Enable() error {
	_, err := d.passthrough(cmdSmart, smartEnable, 0, nil)
	return err
}
