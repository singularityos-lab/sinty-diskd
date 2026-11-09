package usbnvme

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBridgeFor(t *testing.T) {
	if BridgeFor("152d") != JMicron || BridgeFor("174C") != ASMedia || BridgeFor("0bda\n") != Realtek || BridgeFor("0781") != None {
		t.Fatal("bridge detection")
	}
}

func TestSmartLogCDW10(t *testing.T) {
	if got := logCDW10(0x02, 512); got != 0x007f0002 {
		t.Fatalf("cdw10 %#x", got)
	}
}

func TestASMediaCDB(t *testing.T) {
	cdb := ASMediaCDB(0x02, 0x007f0002, 0x11223344, 0x55667788)
	want := []byte{0xE6, 0x02, 0, 0x02, 0, 0, 0x00, 0x7f, 0x55, 0x66, 0x77, 0x88, 0x11, 0x22, 0x33, 0x44}
	if !bytes.Equal(cdb, want) {
		t.Fatalf("asmedia % x", cdb)
	}
}

func TestRealtekCDB(t *testing.T) {
	cdb := RealtekCDB(512, 0x02, 0x007f0002)
	want := []byte{0xE4, 0x00, 0x02, 0x02, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(cdb, want) {
		t.Fatalf("realtek % x", cdb)
	}
}

func TestJMicron(t *testing.T) {
	cdb := JMicronCDB(jmDataIn, 512)
	if !bytes.Equal(cdb, []byte{0xA1, 0x82, 0, 0x00, 0x02, 0x00, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("jmicron cdb % x", cdb)
	}
	cmd := JMicronCommand(0x02, 0xffffffff, 0x007f0002)
	le := binary.LittleEndian
	if string(cmd[0:4]) != "NVME" || le.Uint32(cmd[8:]) != 2 || le.Uint32(cmd[12:]) != 0xffffffff || le.Uint32(cmd[48:]) != 0x007f0002 || len(cmd) != 512 {
		t.Fatalf("jmicron command % x", cmd[:64])
	}
	reply := make([]byte, 512)
	copy(reply, "NVME")
	if s, err := JMicronStatus(reply); err != nil || s != 0 {
		t.Fatal("clean reply")
	}
	le.PutUint32(reply[20:], 0x2<<17)
	if s, _ := JMicronStatus(reply); s != 2 {
		t.Fatalf("status %d", s)
	}
	if _, err := JMicronStatus(make([]byte, 512)); err == nil {
		t.Fatal("reply without signature accepted")
	}
}
