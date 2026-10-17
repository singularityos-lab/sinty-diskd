package ata

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func TestSgIOHeaderMatchesKernelLayout(t *testing.T) {
	if s := unsafe.Sizeof(sgIOHdr{}); s != 88 {
		t.Fatalf("sg_io_hdr is %d bytes, the kernel uses 88", s)
	}
}

func TestParseIdentify(t *testing.T) {
	b := make([]byte, 512)
	put := func(word int, v uint16) { binary.LittleEndian.PutUint16(b[2*word:], v) }
	put(82, 1|1<<3|1<<5|1<<6)
	put(85, 1|1<<5)
	put(83, 1<<3)
	put(89, 0x8000|10)
	put(217, 7200)
	id := ParseIdentify(b)
	if !id.SmartSupported || !id.SmartEnabled || !id.PmSupported || id.PmEnabled || !id.WriteCacheEnabled ||
		id.ReadLookahead || !id.ApmSupported || id.SecurityEraseMinutes != 20 || id.RotationRate != 7200 {
		t.Errorf("identity %+v", id)
	}
	put(217, 1)
	if ParseIdentify(b).RotationRate != 0 {
		t.Error("a solid state drive must report rotation rate 0")
	}
}

func TestParseSmart(t *testing.T) {
	data, th := make([]byte, 512), make([]byte, 512)
	entry := func(i int, id, value, worst byte, raw uint32) {
		e := data[2+12*i:]
		e[0], e[3], e[4] = id, value, worst
		binary.LittleEndian.PutUint32(e[5:], raw)
	}
	entry(0, 5, 100, 100, 3)
	entry(1, 9, 98, 98, 5277)
	entry(2, 194, 30, 20, 30)
	th[2], th[3] = 5, 36
	th[14], th[15] = 194, 25
	data[363] = 0xF3
	s := ParseSmart(data, th)
	if len(s.Attributes) != 3 || s.Attributes[1].Name != "power-on-hours" || s.Attributes[1].Raw != 5277 {
		t.Fatalf("attributes %+v", s.Attributes)
	}
	if s.Attributes[0].Failing() || !s.Attributes[2].FailedInThePast() || s.Attributes[2].Failing() {
		t.Errorf("threshold logic %+v", s.Attributes)
	}
	if s.SelftestStatus != "inprogress" || s.SelftestRemaining != 30 {
		t.Errorf("self-test %s %d", s.SelftestStatus, s.SelftestRemaining)
	}
}
