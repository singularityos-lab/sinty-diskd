package health

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type stick struct {
	data   []byte
	wrap   bool
	broken map[int64]bool
}

func newStick(real uint64, wrap bool) *stick {
	return &stick{data: make([]byte, real), wrap: wrap, broken: map[int64]bool{}}
}

func (s *stick) place(off int64) (int64, bool) {
	if off+SampleSize <= int64(len(s.data)) {
		return off, true
	}
	if s.wrap {
		return off % int64(len(s.data)), true
	}
	return 0, false
}

func (s *stick) WriteAt(p []byte, off int64) (int, error) {
	if s.broken[off] {
		return 0, syscall.EIO
	}
	if at, ok := s.place(off); ok {
		copy(s.data[at:], p)
	}
	return len(p), nil
}

func (s *stick) ReadAt(p []byte, off int64) (int, error) {
	if s.broken[off] {
		return 0, syscall.EIO
	}
	at, ok := s.place(off)
	if !ok {
		for i := range p {
			p[i] = 0
		}
		return len(p), nil
	}
	copy(p, s.data[at:])
	return len(p), nil
}

func (s *stick) Sync() error { return nil }

const mb = 1 << 20

func TestVerifyGenuine(t *testing.T) {
	v, err := Verify(newStick(64*mb, false), 64*mb, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Fake() || v.Damaged() || v.Checked != 16 {
		t.Fatalf("genuine stick judged %+v", v)
	}
}

func TestVerifyWrappingFake(t *testing.T) {
	var last uint
	v, err := Verify(newStick(16*mb, true), 256*mb, 32, func(p uint) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if !v.Fake() || !v.Aliased {
		t.Fatalf("wrapping fake not caught: %+v", v)
	}
	if last != 100 {
		t.Fatalf("progress ended at %d", last)
	}
}

func TestVerifyDiscardingFake(t *testing.T) {
	v, err := Verify(newStick(32*mb, false), 512*mb, 32, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Fake() || v.RealSize == 0 || v.RealSize > 48*mb {
		t.Fatalf("discarding fake not caught: %+v", v)
	}
}

func TestVerifyDamaged(t *testing.T) {
	s := newStick(64*mb, false)
	s.broken[int64(Positions(64*mb, 16)[5])] = true
	v, err := Verify(s, 64*mb, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Fake() || !v.Damaged() || len(v.Failed) != 1 {
		t.Fatalf("damaged stick judged %+v", v)
	}
}

func TestPositionsCoverWholeDrive(t *testing.T) {
	p := Positions(1000*mb, 10)
	if p[0] != 0 || p[len(p)-1] != 999*mb {
		t.Fatalf("positions %v", p)
	}
	for _, x := range p {
		if x%SampleSize != 0 {
			t.Fatalf("unaligned %d", x)
		}
	}
}

func TestParseKernel(t *testing.T) {
	cases := []struct {
		line string
		want Signal
	}{
		{"I/O error, dev sdb, sector 2048 op 0x1:(WRITE) flags 0x800 phys_seg 1 prio class 2", Signal{Disk: "sdb", Kind: IOError}},
		{"blk_update_request: I/O error, dev sdc, sector 99", Signal{Disk: "sdc", Kind: IOError}},
		{"critical medium error, dev sdb, sector 4096 op 0x0:(READ)", Signal{Disk: "sdb", Kind: Unreadable}},
		{"Buffer I/O error on dev sdb1, logical block 0, async page read", Signal{Disk: "sdb", Kind: IOError}},
		{"sd 6:0:0:0: [sdb] tag#0 Sense Key : Medium Error [current]", Signal{Disk: "sdb", Kind: Unreadable}},
		{"sd 6:0:0:0: [sdb] tag#0 Add. Sense: Unrecovered read error", Signal{Disk: "sdb", Kind: Unreadable}},
		{"sd 6:0:0:0: [sdb] tag#3 timing out command, waited 180s", Signal{Disk: "sdb", Kind: Timeout}},
		{"usb 2-1.4: reset high-speed USB device number 5 using xhci_hcd", Signal{UsbPort: "2-1.4", Kind: Reset}},
		{"EXT4-fs (sdb1): Remounting filesystem read-only", Signal{Disk: "sdb", Kind: ReadOnly}},
		{"FAT-fs (sdd1): Filesystem has been set read-only", Signal{Disk: "sdd", Kind: ReadOnly}},
		{"exFAT-fs (mmcblk0p1): Filesystem has been set read-only", Signal{Disk: "mmcblk0", Kind: ReadOnly}},
	}
	for _, c := range cases {
		got, ok := ParseKernel(c.line)
		if !ok || got != c.want {
			t.Errorf("%q: got %+v %v", c.line, got, ok)
		}
	}
	for _, line := range []string{
		"usb 2-1: new high-speed USB device number 3 using xhci_hcd",
		"sd 6:0:0:0: [sdb] Attached SCSI removable disk",
		"EXT4-fs (sdb1): mounted filesystem with ordered data mode",
	} {
		if s, ok := ParseKernel(line); ok {
			t.Errorf("%q matched as %+v", line, s)
		}
	}
	if KmsgText("3,812,1234567,-;sd 1:0:0:0: [sdb] tag#0 Sense Key : Medium Error\n SUBSYSTEM=scsi") != "sd 1:0:0:0: [sdb] tag#0 Sense Key : Medium Error" {
		t.Fatal("kmsg record not unwrapped")
	}
}

func TestDisk(t *testing.T) {
	for in, want := range map[string]string{"sdb1": "sdb", "sdb": "sdb", "nvme0n1p3": "nvme0n1", "nvme0n1": "nvme0n1", "mmcblk1p2": "mmcblk1", "mmcblk1": "mmcblk1"} {
		if got := Disk(in); got != want {
			t.Errorf("Disk(%s) = %s", in, got)
		}
	}
}

func TestAssessOnlyRepeatedOrSerious(t *testing.T) {
	now := int64(100 * day)
	r := Record{Events: []Event{{Kind: IOError, Time: now - 60}}}
	if s, _ := Assess(r, now); s != Watch {
		t.Fatalf("single I/O error gave %s", s)
	}
	r.Events = append(r.Events, Event{Kind: IOError, Time: now - 3*day})
	if s, reasons := Assess(r, now); s != Worn || reasons[0] != IOError {
		t.Fatalf("I/O errors on two days gave %s %v", s, reasons)
	}
	r = Record{Events: []Event{{Kind: ReadOnly, Time: now - 10}}}
	if s, _ := Assess(r, now); s != Failing {
		t.Fatalf("read-only remount gave %s", s)
	}
	r = Record{Events: []Event{{Kind: FakeCapacity, Time: now - 10}}}
	if s, _ := Assess(r, now); s != Failing {
		t.Fatalf("fake capacity gave %s", s)
	}
	r.Events = append(r.Events, Event{Kind: CapacityValid, Time: now})
	if s, _ := Assess(r, now); s != Good {
		t.Fatalf("verified after fake gave %s", s)
	}
	r = Record{Events: []Event{{Kind: CheckFailed, Time: now - 10, Detail: "startup-key copy failed"}, {Kind: CheckPassed, Time: now, Detail: "startup-key"}}}
	if s, _ := Assess(r, now); s != Good {
		t.Fatalf("passed check after failure gave %s", s)
	}
	r = Record{Events: []Event{{Kind: CheckFailed, Time: now - 10, Detail: "key-backup damaged"}, {Kind: CheckPassed, Time: now, Detail: "backup"}}}
	if s, _ := Assess(r, now); s != Failing {
		t.Fatalf("a passing backup hid a damaged Key backup: %s", s)
	}
	r = Record{Events: []Event{{Kind: ReadOnly, Time: now - 40*day}}}
	if s, _ := Assess(r, now); s != Good {
		t.Fatalf("old remount gave %s", s)
	}
	if ShouldAlert(Watch, "") || !ShouldAlert(Worn, "") || ShouldAlert(Worn, Worn) || !ShouldAlert(Failing, Worn) {
		t.Fatal("alert escalation wrong")
	}
}

func TestSpeedCollapse(t *testing.T) {
	var s Speed
	st := IOStat{}
	s.Observe(st)
	step := func(mbytes, ms uint64) bool {
		st.WriteSectors += mbytes * mb / 512
		st.WriteTicks += ms
		_, c := s.Observe(st)
		return c
	}
	for i := 0; i < 3; i++ {
		if step(100, 5000) {
			t.Fatal("collapse while learning")
		}
	}
	if step(10, 100) {
		t.Fatal("small write counted")
	}
	collapsed := false
	for i := 0; i < 3; i++ {
		collapsed = step(100, 60000)
	}
	if !collapsed {
		t.Fatal("collapse not seen")
	}
	if st, ok := ParseStat("  100 0 200 30 50 0 4096 900 0 10 40"); !ok || st.WriteSectors != 4096 || st.WriteTicks != 900 {
		t.Fatalf("stat %+v", st)
	}
}

func TestBookKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	b := Open(path)
	clock := time.Unix(1_000_000, 0)
	b.SetClock(func() time.Time { return clock })
	id := Identity{Vendor: "Kingston", Model: "DataTraveler", Serial: "ABC123", Size: 32 << 30}
	b.Seen(id)
	b.Note(id.Key(), IOError, "sector 12")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	again := Open(path)
	r, ok := again.Get(id.Key())
	if !ok || len(r.Events) != 1 || r.Events[0].Kind != IOError || r.Serial != "ABC123" {
		t.Fatalf("reloaded %+v", r)
	}
	other := id
	other.Size = 64 << 30
	if other.Key() == id.Key() {
		t.Fatal("capacity must be part of the identity")
	}
}
