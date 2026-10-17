package service

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
	"github.com/singularityos-lab/sinty-diskd/internal/nvme"
)

const (
	ifNVMeController = "org.freedesktop.UDisks2.NVMe.Controller"
	ifNVMeNamespace  = "org.freedesktop.UDisks2.NVMe.Namespace"
	ifManagerNVMe    = "org.freedesktop.UDisks2.Manager.NVMe"
)

var namespaceName = regexp.MustCompile(`^(nvme\d+)n(\d+)$`)

type nvmeState struct {
	ctrl     nvme.Controller
	smart    nvme.Smart
	selftest nvme.SelfTest
	updated  uint64
	ok       bool
	checked  uint64
}

func (s *Service) nvmeController(ctrl string, refresh bool) *nvmeState {
	st := s.nvme[ctrl]
	if st == nil {
		st = &nvmeState{selftest: nvme.SelfTest{PercentRemaining: -1}}
		s.nvme[ctrl] = st
	}
	if !refresh && st.ok {
		return st
	}
	st.checked = uint64(time.Now().Unix())
	d, err := nvme.Open("/dev/" + ctrl)
	if err != nil {
		return st
	}
	defer d.Close()
	if c, err := d.IdentifyController(); err == nil {
		st.ctrl = c
		st.ok = true
	}
	if sm, err := d.SmartLog(); err == nil {
		st.smart = sm
		st.updated = uint64(time.Now().Unix())
	}
	if t, err := d.SelfTestLog(); err == nil {
		st.selftest = t
	}
	return st
}

func (s *Service) controllerIfaces(ctrl string, refresh bool) map[string]interface{} {
	st := s.nvmeController(ctrl, refresh)
	sys := nvme.ReadSysfs(s.src.Sys, ctrl)
	warnings := st.smart.CriticalWarning
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]interface{}{
		"State": sys.State, "ControllerID": sys.ControllerID, "SubsystemNQN": ay(sys.SubsystemNQN),
		"FGUID": st.ctrl.FGUID, "NVMeRevision": st.ctrl.Revision, "UnallocatedCapacity": st.ctrl.UnallocatedCapacity,
		"SmartUpdated": st.updated, "SmartCriticalWarning": warnings, "SmartPowerOnHours": st.smart.PowerOnHours,
		"SmartTemperature": st.smart.Temperature, "SmartSelftestStatus": st.selftest.Status,
		"SmartSelftestPercentRemaining": st.selftest.PercentRemaining, "SanitizeStatus": "never_sanitized",
		"SanitizePercentRemaining": int32(-1),
	}
}

type lbaFormat struct {
	Size     uint16
	Metadata uint16
	Perf     uint8
}

func (s *Service) namespaceIfaces(b *model.Block) map[string]interface{} {
	m := namespaceName.FindStringSubmatch(b.Name)
	nsid, _ := strconv.Atoi(m[2])
	wwid, _ := os.ReadFile(s.src.Sys + "/class/block/" + b.Name + "/wwid")
	out := map[string]interface{}{
		"NSID": uint32(nsid), "WWN": strings.TrimSpace(string(wwid)), "EUI64": "", "NGUID": "", "UUID": "",
		"NamespaceSize": uint64(0), "NamespaceCapacity": uint64(0), "NamespaceUtilization": uint64(0),
		"FormattedLBASize": lbaFormat{}, "LBAFormats": []lbaFormat{}, "FormatPercentRemaining": int32(0),
	}
	d, err := nvme.Open("/dev/" + b.Name)
	if err != nil {
		return out
	}
	defer d.Close()
	n, err := d.IdentifyNamespace(uint32(nsid))
	if err != nil {
		return out
	}
	formats := []lbaFormat{}
	for _, f := range n.Formats {
		formats = append(formats, lbaFormat{f.DataSize, f.MetadataSize, f.Performance})
	}
	out["EUI64"], out["NGUID"] = n.EUI64, n.NGUID
	out["NamespaceSize"], out["NamespaceCapacity"], out["NamespaceUtilization"] = n.Size, n.Capacity, n.Utilization
	out["FormattedLBASize"] = lbaFormat{n.Current.DataSize, n.Current.MetadataSize, n.Current.Performance}
	out["LBAFormats"] = formats
	return out
}

func managerNVMe() map[string]interface{} {
	read := func(p string) string {
		data, _ := os.ReadFile(p)
		return strings.TrimSpace(string(data))
	}
	return map[string]interface{}{"HostNQN": ay(read("/etc/nvme/hostnqn")), "HostID": ay(read("/etc/nvme/hostid"))}
}

type controllerObject struct {
	s    *Service
	ctrl string
}

func (c *controllerObject) SmartUpdate(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	c.s.mu.Lock()
	c.s.nvmeController(c.ctrl, true)
	c.s.mu.Unlock()
	c.s.Refresh()
	return nil
}

func (c *controllerObject) SmartGetAttributes(options map[string]dbus.Variant) (map[string]dbus.Variant, *dbus.Error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	st := c.s.nvmeController(c.ctrl, true)
	sm := st.smart
	sensors := sm.TemperatureSensors
	if sensors == nil {
		sensors = []uint16{}
	}
	return map[string]dbus.Variant{
		"avail_spare": dbus.MakeVariant(sm.AvailSpare), "spare_thresh": dbus.MakeVariant(sm.SpareThreshold),
		"percent_used": dbus.MakeVariant(sm.PercentUsed), "total_data_read": dbus.MakeVariant(sm.DataUnitsRead * 512000),
		"total_data_written": dbus.MakeVariant(sm.DataUnitsWritten * 512000), "ctrl_busy_time": dbus.MakeVariant(sm.ControllerBusyTime),
		"power_cycles": dbus.MakeVariant(sm.PowerCycles), "unsafe_shutdowns": dbus.MakeVariant(sm.UnsafeShutdowns),
		"media_errors": dbus.MakeVariant(sm.MediaErrors), "num_err_log_entries": dbus.MakeVariant(sm.NumErrLogEntries),
		"temp_sensors": dbus.MakeVariant(sensors), "wctemp": dbus.MakeVariant(uint16(0)), "cctemp": dbus.MakeVariant(uint16(0)),
		"warning_temp_time": dbus.MakeVariant(sm.WarningTempTime), "critical_temp_time": dbus.MakeVariant(sm.CriticalTempTime),
	}, nil
}

func (c *controllerObject) selftest(sender dbus.Sender, kind string) *dbus.Error {
	uid, _, _, err := c.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	if uid != 0 && uid < 1000 {
		return notAuthorized("run a self-test")
	}
	d, err := nvme.Open("/dev/" + c.ctrl)
	if err != nil {
		return failed(err)
	}
	defer d.Close()
	if err := d.StartSelfTest(kind); err != nil {
		return failed(err)
	}
	c.s.mu.Lock()
	c.s.nvmeController(c.ctrl, true)
	c.s.mu.Unlock()
	c.s.Refresh()
	return nil
}

func (c *controllerObject) SmartSelftestStart(sender dbus.Sender, kind string, options map[string]dbus.Variant) *dbus.Error {
	if kind != "short" && kind != "extended" {
		return failed(errInvalid("self-test type " + kind))
	}
	return c.selftest(sender, kind)
}

func (c *controllerObject) SmartSelftestAbort(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	return c.selftest(sender, "abort")
}

type invalidArg string

func (e invalidArg) Error() string { return "invalid " + string(e) }

func errInvalid(what string) error { return invalidArg(what) }
