// Package nvme reads the identity and health of NVMe controllers and
// namespaces through the admin command interface of the kernel driver.
package nvme

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const ioctlAdminCmd = 0xC0484E41

type adminCmd struct {
	opcode      uint8
	flags       uint8
	rsvd1       uint16
	nsid        uint32
	cdw2        uint32
	cdw3        uint32
	metadata    uint64
	addr        uint64
	metadataLen uint32
	dataLen     uint32
	cdw10       uint32
	cdw11       uint32
	cdw12       uint32
	cdw13       uint32
	cdw14       uint32
	cdw15       uint32
	timeoutMs   uint32
	result      uint32
}

const (
	opGetLogPage  = 0x02
	opIdentify    = 0x06
	opSelfTest    = 0x14
	logSmart      = 0x02
	logSelfTest   = 0x06
	cnsNamespace  = 0x00
	cnsController = 0x01
	selfTestShort = 0x1
	selfTestLong  = 0x2
	selfTestAbort = 0xf
)

// Device is an open NVMe controller or namespace node.
type Device struct{ f *os.File }

// Open opens a controller (/dev/nvme0) or namespace (/dev/nvme0n1) node.
func Open(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return &Device{f}, nil
}

// Close closes the node.
func (d *Device) Close() error { return d.f.Close() }

func (d *Device) admin(c *adminCmd, buf []byte) error {
	if len(buf) > 0 {
		c.addr = uint64(uintptr(unsafe.Pointer(&buf[0])))
		c.dataLen = uint32(len(buf))
	}
	c.timeoutMs = 10000
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), ioctlAdminCmd, uintptr(unsafe.Pointer(c)))
	if errno != 0 {
		return errno
	}
	if c.result != 0 {
		return fmt.Errorf("nvme admin command %#x failed with status %#x", c.opcode, c.result)
	}
	return nil
}

func (d *Device) identify(cns, nsid uint32) ([]byte, error) {
	buf := make([]byte, 4096)
	return buf, d.admin(&adminCmd{opcode: opIdentify, nsid: nsid, cdw10: cns}, buf)
}

func (d *Device) logPage(lid uint8, nsid uint32, size int) ([]byte, error) {
	buf := make([]byte, size)
	numd := uint32(size/4 - 1)
	return buf, d.admin(&adminCmd{opcode: opGetLogPage, nsid: nsid, cdw10: numd<<16 | uint32(lid)}, buf)
}

// Controller is what Identify Controller reports.
type Controller struct {
	Revision            string
	FGUID               string
	UnallocatedCapacity uint64
}

// ParseController decodes an Identify Controller data structure.
func ParseController(b []byte) Controller {
	var c Controller
	ver := binary.LittleEndian.Uint32(b[80:])
	major, minor, tertiary := ver>>16, (ver>>8)&0xff, ver&0xff
	if ver != 0 {
		c.Revision = fmt.Sprintf("%d.%d", major, minor)
		if tertiary != 0 {
			c.Revision += fmt.Sprintf(".%d", tertiary)
		}
	}
	if fguid := b[112:128]; !allZero(fguid) {
		c.FGUID = fmt.Sprintf("%x", fguid)
	}
	unvm := new(big.Int).SetBytes(reverse(b[296:312]))
	if unvm.IsUint64() {
		c.UnallocatedCapacity = unvm.Uint64()
	}
	return c
}

// IdentifyController runs Identify Controller.
func (d *Device) IdentifyController() (Controller, error) {
	b, err := d.identify(cnsController, 0)
	if err != nil {
		return Controller{}, err
	}
	return ParseController(b), nil
}

// LBAFormat is one entry of the namespace format table.
type LBAFormat struct {
	DataSize     uint16
	MetadataSize uint16
	Performance  uint8
}

// Namespace is what Identify Namespace reports.
type Namespace struct {
	Size, Capacity, Utilization uint64
	Formats                     []LBAFormat
	Current                     LBAFormat
	EUI64                       string
	NGUID                       string
}

// ParseNamespace decodes an Identify Namespace data structure.
func ParseNamespace(b []byte) Namespace {
	le := binary.LittleEndian
	n := Namespace{Size: le.Uint64(b[0:]), Capacity: le.Uint64(b[8:]), Utilization: le.Uint64(b[16:])}
	count := int(b[25]) + 1
	for i := 0; i < count && i < 64; i++ {
		e := b[128+4*i:]
		f := LBAFormat{MetadataSize: le.Uint16(e[0:]), DataSize: 1 << e[2], Performance: e[3] & 0x3}
		n.Formats = append(n.Formats, f)
	}
	cur := int(b[26]&0xf) | int(b[26]>>5&0x3)<<4
	if cur < len(n.Formats) {
		n.Current = n.Formats[cur]
	}
	if !allZero(b[104:120]) {
		n.NGUID = fmt.Sprintf("%x", b[104:120])
	}
	if !allZero(b[120:128]) {
		n.EUI64 = fmt.Sprintf("%x", b[120:128])
	}
	return n
}

// IdentifyNamespace runs Identify Namespace for nsid.
func (d *Device) IdentifyNamespace(nsid uint32) (Namespace, error) {
	b, err := d.identify(cnsNamespace, nsid)
	if err != nil {
		return Namespace{}, err
	}
	return ParseNamespace(b), nil
}

// Smart is the SMART / health information log.
type Smart struct {
	CriticalWarning    []string
	Temperature        uint16
	AvailSpare         uint8
	SpareThreshold     uint8
	PercentUsed        uint8
	PowerOnHours       uint64
	PowerCycles        uint64
	UnsafeShutdowns    uint64
	MediaErrors        uint64
	NumErrLogEntries   uint64
	DataUnitsRead      uint64
	DataUnitsWritten   uint64
	ControllerBusyTime uint64
	WarningTempTime    uint32
	CriticalTempTime   uint32
	TemperatureSensors []uint16
}

func le128(b []byte) uint64 {
	v := new(big.Int).SetBytes(reverse(b[:16]))
	if v.IsUint64() {
		return v.Uint64()
	}
	return ^uint64(0)
}

// ParseSmart decodes the SMART / health information log page.
func ParseSmart(b []byte) Smart {
	le := binary.LittleEndian
	s := Smart{CriticalWarning: []string{}}
	names := []string{"spare", "temperature", "degraded", "readonly", "volatile_mem", "pmr_readonly"}
	for i, name := range names {
		if b[0]&(1<<i) != 0 {
			s.CriticalWarning = append(s.CriticalWarning, name)
		}
	}
	s.Temperature = le.Uint16(b[1:])
	s.AvailSpare, s.SpareThreshold, s.PercentUsed = b[3], b[4], b[5]
	s.DataUnitsRead = le128(b[32:])
	s.DataUnitsWritten = le128(b[48:])
	s.ControllerBusyTime = le128(b[96:])
	s.PowerCycles = le128(b[112:])
	s.PowerOnHours = le128(b[128:])
	s.UnsafeShutdowns = le128(b[144:])
	s.MediaErrors = le128(b[160:])
	s.NumErrLogEntries = le128(b[176:])
	s.WarningTempTime = le.Uint32(b[192:])
	s.CriticalTempTime = le.Uint32(b[196:])
	for i := 0; i < 8; i++ {
		if t := le.Uint16(b[200+2*i:]); t != 0 {
			s.TemperatureSensors = append(s.TemperatureSensors, t)
		}
	}
	return s
}

// SmartLog reads the SMART / health information log.
func (d *Device) SmartLog() (Smart, error) {
	b, err := d.logPage(logSmart, 0xffffffff, 512)
	if err != nil {
		return Smart{}, err
	}
	return ParseSmart(b), nil
}

// SelfTest is the state of the device self-test.
type SelfTest struct {
	Status           string
	PercentRemaining int32
}

var selfTestResults = map[uint8]string{
	0x0: "success", 0x1: "aborted", 0x2: "aborted", 0x3: "aborted", 0x4: "fatal_error",
	0x5: "known_seg_fail", 0x6: "unknown_seg_fail", 0x7: "unknown_seg_fail", 0x8: "fatal_error",
}

// ParseSelfTest decodes the device self-test log.
func ParseSelfTest(b []byte) SelfTest {
	if b[0]&0xf != 0 {
		return SelfTest{Status: "inprogress", PercentRemaining: int32(100 - int(b[1]&0x7f))}
	}
	result := b[4] & 0xf
	if result == 0xf {
		return SelfTest{Status: "", PercentRemaining: -1}
	}
	if status, ok := selfTestResults[result]; ok {
		return SelfTest{Status: status, PercentRemaining: -1}
	}
	return SelfTest{Status: "unknown", PercentRemaining: -1}
}

// SelfTestLog reads the device self-test log.
func (d *Device) SelfTestLog() (SelfTest, error) {
	b, err := d.logPage(logSelfTest, 0xffffffff, 564)
	if err != nil {
		return SelfTest{}, err
	}
	return ParseSelfTest(b), nil
}

// StartSelfTest starts a short or extended self-test, or aborts the running
// one.
func (d *Device) StartSelfTest(kind string) error {
	code := map[string]uint32{"short": selfTestShort, "extended": selfTestLong, "abort": selfTestAbort}[kind]
	if code == 0 {
		return fmt.Errorf("unknown self-test %q", kind)
	}
	return d.admin(&adminCmd{opcode: opSelfTest, nsid: 0xffffffff, cdw10: code}, nil)
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// Sysfs holds what the kernel exposes for a controller without a command.
type Sysfs struct {
	ControllerID uint16
	SubsystemNQN string
	State        string
}

// ReadSysfs reads /sys/class/nvme/<ctrl>.
func ReadSysfs(sys, ctrl string) Sysfs {
	read := func(name string) string {
		data, _ := os.ReadFile(sys + "/class/nvme/" + ctrl + "/" + name)
		return strings.TrimSpace(string(data))
	}
	var id int
	fmt.Sscanf(read("cntlid"), "%d", &id)
	return Sysfs{ControllerID: uint16(id), SubsystemNQN: read("subsysnqn"), State: read("state")}
}
