package nvme

import (
	"encoding/binary"
	"reflect"
	"testing"
	"unsafe"
)

func TestAdminCommandMatchesKernelLayout(t *testing.T) {
	if s := unsafe.Sizeof(adminCmd{}); s != 72 {
		t.Fatalf("nvme_admin_cmd is %d bytes, the kernel uses 72", s)
	}
}

func TestParseController(t *testing.T) {
	b := make([]byte, 4096)
	binary.LittleEndian.PutUint32(b[80:], 1<<16|4<<8)
	c := ParseController(b)
	if c.Revision != "1.4" || c.FGUID != "" || c.UnallocatedCapacity != 0 {
		t.Errorf("controller %+v", c)
	}
	binary.LittleEndian.PutUint32(b[80:], 2<<16|0<<8|1)
	b[296] = 0x10
	if c := ParseController(b); c.Revision != "2.0.1" || c.UnallocatedCapacity != 16 {
		t.Errorf("controller %+v", c)
	}
}

func TestParseNamespace(t *testing.T) {
	b := make([]byte, 4096)
	le := binary.LittleEndian
	le.PutUint64(b[0:], 1000215216)
	le.PutUint64(b[8:], 1000215216)
	le.PutUint64(b[16:], 920207880)
	b[25] = 1
	b[26] = 0
	b[128+2] = 9
	b[128+4+2] = 12
	copy(b[120:], []byte{0x00, 0x25, 0x38, 0xe8, 0x31, 0xc9, 0xeb, 0xac})
	n := ParseNamespace(b)
	want := []LBAFormat{{DataSize: 512}, {DataSize: 4096}}
	if n.Size != 1000215216 || n.Utilization != 920207880 || !reflect.DeepEqual(n.Formats, want) || n.Current.DataSize != 512 || n.EUI64 != "002538e831c9ebac" || n.NGUID != "" {
		t.Errorf("namespace %+v", n)
	}
}

func TestParseSmart(t *testing.T) {
	b := make([]byte, 512)
	b[0] = 0x02
	binary.LittleEndian.PutUint16(b[1:], 326)
	binary.LittleEndian.PutUint64(b[128:], 5277)
	binary.LittleEndian.PutUint64(b[112:], 1200)
	s := ParseSmart(b)
	if s.Temperature != 326 || s.PowerOnHours != 5277 || s.PowerCycles != 1200 || !reflect.DeepEqual(s.CriticalWarning, []string{"temperature"}) {
		t.Errorf("smart %+v", s)
	}
}

func TestParseSelfTest(t *testing.T) {
	b := make([]byte, 564)
	b[4] = 0x0
	if st := ParseSelfTest(b); st.Status != "success" || st.PercentRemaining != -1 {
		t.Errorf("finished test %+v", st)
	}
	b[0], b[1] = 1, 30
	if st := ParseSelfTest(b); st.Status != "inprogress" || st.PercentRemaining != 70 {
		t.Errorf("running test %+v", st)
	}
	b[0], b[4] = 0, 0xf
	if st := ParseSelfTest(b); st.Status != "" {
		t.Errorf("no test %+v", st)
	}
}
