package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	"github.com/singularityos-lab/sinty-diskd/internal/ata"
	"github.com/singularityos-lab/sinty-diskd/internal/health"
	"github.com/singularityos-lab/sinty-diskd/internal/model"
	"github.com/singularityos-lab/sinty-diskd/internal/usbnvme"
)

const (
	healthName = "dev.sinty.DiskHealth"
	healthPath = dbus.ObjectPath("/dev/sinty/DiskHealth")
	ifHealth   = "dev.sinty.DiskHealth1"
)

type tracked struct {
	key     string
	disk    string
	sysPath string
	speed   *health.Speed
}

type healthMonitor struct {
	s      *Service
	book   *health.Book
	mu     sync.Mutex
	disks  map[string]*tracked
	states map[string]string
	verify map[string]bool
	devDir string
	dirty  bool
}

type DriveHealth struct {
	Key      string
	Device   string
	Name     string
	Serial   string
	Size     uint64
	State    string
	Reasons  []string
	LastSeen int64
	RealSize uint64
}

func (s *Service) StartHealth(storePath, kernelLog string) error {
	h := &healthMonitor{s: s, book: health.Open(storePath), disks: map[string]*tracked{}, states: map[string]string{}, verify: map[string]bool{}, devDir: "/dev"}
	s.health = h
	if err := s.conn.Export(&healthObject{h}, healthPath, ifHealth); err != nil {
		return err
	}
	node := &introspect.Node{Name: string(healthPath), Interfaces: []introspect.Interface{introspect.IntrospectData, {
		Name:    ifHealth,
		Methods: introspect.Methods(&healthObject{h}),
		Signals: []introspect.Signal{
			{Name: "Changed", Args: []introspect.Arg{{Name: "key", Type: "s"}, {Name: "state", Type: "s"}, {Name: "reasons", Type: "as"}, {Name: "alert", Type: "b"}}},
			{Name: "Progress", Args: []introspect.Arg{{Name: "device", Type: "s"}, {Name: "percent", Type: "u"}}},
		}}}}
	s.conn.Export(introspect.NewIntrospectable(node), healthPath, "org.freedesktop.DBus.Introspectable")
	reply, err := s.conn.RequestName(healthName, dbus.NameFlagReplaceExisting|dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("%s is owned by another process", healthName)
	}
	for _, key := range h.book.Keys() {
		r, _ := h.book.Get(key)
		h.states[key], _ = health.Assess(r, h.book.Now().Unix())
	}
	go h.followKernel(kernelLog)
	go h.sampleSpeed(20 * time.Second)
	return nil
}

func followed(d *model.Drive) bool {
	return d.ConnectionBus == "usb" || d.ConnectionBus == "sdio"
}

func (h *healthMonitor) track(m *model.Model, sys string) {
	seen := map[string]bool{}
	var fresh []*tracked
	h.mu.Lock()
	for _, d := range m.Drives {
		if !followed(d) || d.Size == 0 {
			continue
		}
		disk := d.Block()
		seen[disk] = true
		id := health.Identity{Vendor: d.Vendor, Model: d.Model, Serial: d.Serial, Size: d.Size}
		t := h.disks[disk]
		if t != nil && t.key == id.Key() {
			continue
		}
		r := h.book.Seen(id)
		sysPath, _ := filepath.EvalSymlinks(filepath.Join(sys, "class/block", disk))
		t = &tracked{key: id.Key(), disk: disk, sysPath: sysPath, speed: &health.Speed{}}
		if r.WriteSpeed > 0 {
			t.speed.Baseline = r.WriteSpeed
		}
		h.disks[disk] = t
		h.dirty = true
		fresh = append(fresh, t)
	}
	for disk := range h.disks {
		if !seen[disk] {
			delete(h.disks, disk)
		}
	}
	h.mu.Unlock()
	for _, t := range fresh {
		go h.probe(t)
	}
	h.flush()
}

func usbVendor(sysPath string) string {
	for dir := sysPath; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, "idVendor")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func (h *healthMonitor) noteChanged(key, kind, detail string) {
	r, ok := h.book.Get(key)
	if !ok {
		return
	}
	for i := len(r.Events) - 1; i >= 0; i-- {
		if r.Events[i].Kind == kind {
			if r.Events[i].Detail == detail {
				return
			}
			break
		}
	}
	h.book.Note(key, kind, detail)
}

func (h *healthMonitor) probe(t *tracked) {
	dev := filepath.Join(h.devDir, t.disk)
	if bridge := usbnvme.BridgeFor(usbVendor(t.sysPath)); bridge != usbnvme.None {
		if sm, err := usbnvme.SmartLog(dev, bridge); err == nil {
			if len(sm.CriticalWarning) > 0 {
				h.noteChanged(t.key, health.SmartFailing, strings.Join(sm.CriticalWarning, ","))
			}
			if sm.MediaErrors > 0 || sm.PercentUsed >= 100 {
				h.noteChanged(t.key, health.SmartWorn, fmt.Sprintf("media errors %d, used %d%%", sm.MediaErrors, sm.PercentUsed))
			}
			h.evaluate(t.key)
			return
		}
	}
	d, err := ata.Open(dev)
	if err != nil {
		return
	}
	defer d.Close()
	id, err := d.Identify()
	if err != nil || !id.SmartEnabled {
		return
	}
	if failing, err := d.Failing(); err == nil && failing {
		h.noteChanged(t.key, health.SmartFailing, "self-assessment")
	}
	if sm, err := d.ReadSmart(); err == nil {
		var bad uint64
		for _, a := range sm.Attributes {
			switch a.ID {
			case 5, 197, 198:
				bad += a.Raw & 0xffffffff
			}
			if a.Failing() {
				h.noteChanged(t.key, health.SmartFailing, a.Name)
			}
		}
		if bad > 0 {
			h.noteChanged(t.key, health.SmartWorn, fmt.Sprintf("%d bad sectors", bad))
		}
	}
	h.evaluate(t.key)
}

func (h *healthMonitor) byDisk(disk string) *tracked {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.disks[health.Disk(disk)]
}

func (h *healthMonitor) byPort(port string) *tracked {
	h.mu.Lock()
	defer h.mu.Unlock()
	needle := "/" + port + "/" + port + ":"
	for _, t := range h.disks {
		if strings.Contains(t.sysPath, needle) {
			return t
		}
	}
	return nil
}

func (h *healthMonitor) signal(sig health.Signal, detail string) {
	var t *tracked
	if sig.UsbPort != "" {
		t = h.byPort(sig.UsbPort)
	} else {
		t = h.byDisk(sig.Disk)
	}
	if t == nil {
		return
	}
	h.book.Note(t.key, sig.Kind, detail)
	h.evaluate(t.key)
}

func (h *healthMonitor) followKernel(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("kernel log: %v", err)
		return
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)
	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EINTR) {
				continue
			}
			log.Printf("kernel log: %v", err)
			return
		}
		line := health.KmsgText(string(buf[:n]))
		if sig, ok := health.ParseKernel(line); ok {
			h.signal(sig, line)
		}
	}
}

func (h *healthMonitor) sampleSpeed(every time.Duration) {
	for range time.Tick(every) {
		h.mu.Lock()
		list := make([]*tracked, 0, len(h.disks))
		for _, t := range h.disks {
			list = append(list, t)
		}
		h.mu.Unlock()
		for _, t := range list {
			b, err := os.ReadFile(filepath.Join(t.sysPath, "stat"))
			if err != nil {
				continue
			}
			st, ok := health.ParseStat(string(b))
			if !ok {
				continue
			}
			rate, collapsed := t.speed.Observe(st)
			if rate > 0 {
				base := t.speed.Baseline
				h.book.Update(t.key, func(r *health.Record) { r.WriteSpeed = base })
				h.mu.Lock()
				h.dirty = true
				h.mu.Unlock()
			}
			if collapsed {
				h.book.Note(t.key, health.SlowWrites, fmt.Sprintf("%.1f MB/s, usually %.1f MB/s", rate/1e6, t.speed.Baseline/1e6))
				h.evaluate(t.key)
			}
		}
		h.flush()
	}
}

func (h *healthMonitor) evaluate(key string) {
	r, ok := h.book.Get(key)
	if !ok {
		return
	}
	state, reasons := health.Assess(r, h.book.Now().Unix())
	alert := health.ShouldAlert(state, r.Alerted)
	if alert || health.Rank(state) < health.Rank(r.Alerted) {
		h.book.Update(key, func(r *health.Record) {
			if health.Rank(state) >= health.Rank(health.Worn) {
				r.Alerted = state
			} else {
				r.Alerted = ""
			}
		})
	}
	h.mu.Lock()
	changed := h.states[key] != state
	h.states[key] = state
	h.dirty = true
	h.mu.Unlock()
	if reasons == nil {
		reasons = []string{}
	}
	if changed || alert {
		h.s.conn.Emit(healthPath, ifHealth+".Changed", key, state, reasons, alert)
	}
	h.flush()
}

func (h *healthMonitor) flush() {
	h.mu.Lock()
	dirty := h.dirty
	h.dirty = false
	h.mu.Unlock()
	if dirty {
		if err := h.book.Save(); err != nil {
			log.Printf("health history: %v", err)
		}
	}
}

func (h *healthMonitor) describe(key string) DriveHealth {
	r, _ := h.book.Get(key)
	state, reasons := health.Assess(r, h.book.Now().Unix())
	if reasons == nil {
		reasons = []string{}
	}
	out := DriveHealth{Key: key, Name: r.Name(), Serial: r.Serial, Size: r.Size, State: state, Reasons: reasons, LastSeen: r.LastSeen, RealSize: r.RealSize}
	h.mu.Lock()
	for _, t := range h.disks {
		if t.key == key {
			out.Device = "/dev/" + t.disk
		}
	}
	h.mu.Unlock()
	return out
}

func (h *healthMonitor) keyFor(device string) string {
	if t := h.byDisk(filepath.Base(device)); t != nil {
		return t.key
	}
	for _, k := range h.book.Keys() {
		if k == device {
			return k
		}
	}
	return ""
}

var errFakeCapacity = errors.New("this drive reports more space than it really has")

func (h *healthMonitor) verifyCapacity(name string, size uint64) (health.Verdict, error) {
	h.mu.Lock()
	if h.verify[name] {
		h.mu.Unlock()
		return health.Verdict{}, errors.New("a check of this drive is already running")
	}
	h.verify[name] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.verify, name)
		h.mu.Unlock()
	}()
	f, err := health.OpenDirect(filepath.Join(h.devDir, name))
	if err != nil {
		return health.Verdict{}, err
	}
	defer f.Close()
	device := "/dev/" + name
	v, err := health.Verify(f, size, health.Samples, func(p uint) {
		h.s.conn.Emit(healthPath, ifHealth+".Progress", device, uint32(p))
	})
	if err != nil {
		return v, err
	}
	if t := h.byDisk(name); t != nil {
		switch {
		case v.Fake():
			h.book.Note(t.key, health.FakeCapacity, fmt.Sprintf("real size about %d bytes", v.RealSize))
			h.book.Update(t.key, func(r *health.Record) { r.RealSize = v.RealSize })
		case v.Damaged():
			h.book.Note(t.key, health.Unreadable, fmt.Sprintf("%d of %d samples did not read back", len(v.Failed), v.Checked))
		default:
			h.book.Note(t.key, health.CapacityValid, fmt.Sprintf("%d samples", v.Checked))
			h.book.Update(t.key, func(r *health.Record) { r.RealSize = 0 })
		}
		h.evaluate(t.key)
	}
	return v, nil
}

func (s *Service) checkCapacity(name string, options map[string]dbus.Variant) *dbus.Error {
	if s.health == nil || optBool(options, "no-verify") {
		return nil
	}
	s.mu.Lock()
	blk := s.block(name)
	var drive *model.Drive
	if blk != nil {
		drive = s.model.Drives[strings.TrimPrefix(blk.Drive, drivesPrefix)]
	}
	s.mu.Unlock()
	if blk == nil || drive == nil || !followed(drive) || blk.Size == 0 {
		return nil
	}
	v, err := s.health.verifyCapacity(name, blk.Size)
	if err != nil {
		return failed(err)
	}
	if v.Fake() {
		return dbus.NewError("dev.sinty.DiskHealth1.Error.FakeCapacity", []interface{}{errFakeCapacity.Error()})
	}
	return nil
}

type healthObject struct{ h *healthMonitor }

func (o *healthObject) Drives() ([]DriveHealth, *dbus.Error) {
	out := []DriveHealth{}
	for _, k := range o.h.book.Keys() {
		out = append(out, o.h.describe(k))
	}
	return out, nil
}

func (o *healthObject) Health(device string) (DriveHealth, *dbus.Error) {
	key := o.h.keyFor(device)
	if key == "" {
		return DriveHealth{}, dbus.NewError("dev.sinty.DiskHealth1.Error.Unknown", []interface{}{"unknown drive " + device})
	}
	return o.h.describe(key), nil
}

func (o *healthObject) VerifyCapacity(sender dbus.Sender, device string) (bool, uint64, *dbus.Error) {
	s := o.h.s
	name := filepath.Base(device)
	s.mu.Lock()
	blk := s.block(name)
	if blk == nil {
		s.mu.Unlock()
		return false, 0, failed(errors.New("no such device"))
	}
	_, derr := s.mayModify(sender, blk)
	if derr == nil {
		derr = s.busy(blk)
	}
	size := blk.Size
	s.mu.Unlock()
	if derr != nil {
		return false, 0, derr
	}
	v, err := o.h.verifyCapacity(name, size)
	if err != nil {
		return false, 0, failed(err)
	}
	real := size
	if v.Fake() {
		real = v.RealSize
	}
	return !v.Fake() && !v.Damaged(), real, nil
}

var checkKinds = map[string]bool{"startup-key": true, "backup": true, "key-backup": true}

func (o *healthObject) Report(sender dbus.Sender, device, check string, ok bool, detail string) *dbus.Error {
	uid, _, _, err := o.h.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	if uid != 0 {
		return notAuthorized("report drive checks")
	}
	if !checkKinds[check] {
		return failed(fmt.Errorf("unknown check %q", check))
	}
	key := o.h.keyFor(device)
	if key == "" {
		return nil
	}
	kind := health.CheckPassed
	if !ok {
		kind = health.CheckFailed
	}
	o.h.book.Note(key, kind, strings.TrimSpace(check+" "+detail))
	o.h.evaluate(key)
	return nil
}
