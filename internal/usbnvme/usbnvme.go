package usbnvme

import (
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/singularityos-lab/sinty-diskd/internal/nvme"
)

type Bridge int

const (
	None Bridge = iota
	JMicron
	ASMedia
	Realtek
)

func BridgeFor(vendorID string) Bridge {
	switch strings.ToLower(strings.TrimSpace(vendorID)) {
	case "152d":
		return JMicron
	case "174c":
		return ASMedia
	case "0bda":
		return Realtek
	}
	return None
}

const (
	opGetLogPage = 0x02
	logSmart     = 0x02
	smartSize    = 512
	jmSignature  = 0x454D564E
)

func logCDW10(lid uint8, size int) uint32 {
	return uint32(lid) | uint32(size/4-1)<<16
}

func ASMediaCDB(opcode uint8, cdw10, cdw12, cdw13 uint32) []byte {
	cdb := make([]byte, 16)
	cdb[0] = 0xE6
	cdb[1] = opcode
	cdb[3] = byte(cdw10)
	cdb[6] = byte(cdw10 >> 24)
	cdb[7] = byte(cdw10 >> 16)
	binary.BigEndian.PutUint32(cdb[8:], cdw13)
	binary.BigEndian.PutUint32(cdb[12:], cdw12)
	return cdb
}

func RealtekCDB(size uint16, opcode uint8, cdw10 uint32) []byte {
	cdb := make([]byte, 16)
	cdb[0] = 0xE4
	binary.LittleEndian.PutUint16(cdb[1:], size)
	cdb[3] = opcode
	cdb[4] = byte(cdw10)
	return cdb
}

const (
	jmCommand  = 0x0
	jmDataIn   = 0x2
	jmResponse = 0xF
)

func JMicronCDB(protocol uint8, length uint32) []byte {
	cdb := make([]byte, 12)
	cdb[0] = 0xA1
	cdb[1] = 0x80 | protocol
	cdb[3] = byte(length >> 16)
	cdb[4] = byte(length >> 8)
	cdb[5] = byte(length)
	return cdb
}

func JMicronCommand(opcode uint8, nsid, cdw10 uint32) []byte {
	b := make([]byte, 512)
	le := binary.LittleEndian
	le.PutUint32(b[0:], jmSignature)
	le.PutUint32(b[8:], uint32(opcode))
	le.PutUint32(b[12:], nsid)
	le.PutUint32(b[48:], cdw10)
	return b
}

func JMicronStatus(reply []byte) (uint32, error) {
	if len(reply) < 24 || binary.LittleEndian.Uint32(reply[0:]) != jmSignature {
		return 0, errors.New("unexpected bridge reply")
	}
	return binary.LittleEndian.Uint32(reply[20:]) >> 17, nil
}

func SmartLog(path string, bridge Bridge) (nvme.Smart, error) {
	if bridge == None {
		return nvme.Smart{}, errors.New("not an NVMe bridge")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nvme.Smart{}, err
	}
	defer f.Close()
	buf := make([]byte, smartSize)
	cdw10 := logCDW10(logSmart, smartSize)
	switch bridge {
	case ASMedia:
		err = sg(f, ASMediaCDB(opGetLogPage, cdw10, 0, 0), dirFromDev, buf)
	case Realtek:
		err = sg(f, RealtekCDB(smartSize, opGetLogPage, cdw10), dirFromDev, buf)
	case JMicron:
		err = jmicron(f, JMicronCommand(opGetLogPage, 0xffffffff, cdw10), buf)
	}
	if err != nil {
		return nvme.Smart{}, err
	}
	if allZero(buf) {
		return nvme.Smart{}, errors.New("empty health log")
	}
	return nvme.ParseSmart(buf), nil
}

func jmicron(f *os.File, cmd, data []byte) error {
	if err := sg(f, JMicronCDB(jmCommand, uint32(len(cmd))), dirToDev, cmd); err != nil {
		return err
	}
	if err := sg(f, JMicronCDB(jmDataIn, uint32(len(data))), dirFromDev, data); err != nil {
		return err
	}
	reply := make([]byte, 512)
	if err := sg(f, JMicronCDB(jmResponse, uint32(len(reply))), dirFromDev, reply); err != nil {
		return err
	}
	status, err := JMicronStatus(reply)
	if err != nil {
		return err
	}
	if status != 0 {
		return errors.New("NVMe command failed behind the bridge")
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

const (
	sgIO       = 0x2285
	dirToDev   = -2
	dirFromDev = -3
)

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

func sg(f *os.File, cdb []byte, dir int32, data []byte) error {
	sense := make([]byte, 32)
	h := sgIOHdr{interfaceID: 'S', dxferDirection: dir, cmdLen: uint8(len(cdb)), mxSbLen: uint8(len(sense)), timeout: 15000}
	h.dxferLen = uint32(len(data))
	h.dxferp = uintptr(unsafe.Pointer(&data[0]))
	h.cmdp = uintptr(unsafe.Pointer(&cdb[0]))
	h.sbp = uintptr(unsafe.Pointer(&sense[0]))
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), sgIO, uintptr(unsafe.Pointer(&h))); errno != 0 {
		return errno
	}
	if h.status != 0 || h.hostStatus != 0 || h.driverStatus&0xf != 0 {
		return errors.New("bridge refused the command")
	}
	return nil
}
