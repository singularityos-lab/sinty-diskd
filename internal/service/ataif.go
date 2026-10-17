package service

import (
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-diskd/internal/ata"
)

const ifDriveAta = "org.freedesktop.UDisks2.Drive.Ata"

type ataState struct {
	id      ata.Identity
	smart   ata.Smart
	failing bool
	updated uint64
	ok      bool
	checked uint64
}

func (s *Service) ataDrive(disk string, refresh bool) *ataState {
	st := s.ata[disk]
	if st == nil {
		st = &ataState{smart: ata.Smart{SelftestRemaining: -1}}
		s.ata[disk] = st
	}
	if !refresh && st.ok {
		return st
	}
	st.checked = uint64(time.Now().Unix())
	d, err := ata.Open("/dev/" + disk)
	if err != nil {
		return st
	}
	defer d.Close()
	if id, err := d.Identify(); err == nil {
		st.id, st.ok = id, true
	}
	if st.id.SmartEnabled {
		if sm, err := d.ReadSmart(); err == nil {
			st.smart = sm
			st.updated = uint64(time.Now().Unix())
		}
		st.failing, _ = d.Failing()
	}
	return st
}

func (s *Service) ataIfaces(disk string, refresh bool) map[string]interface{} {
	st := s.ataDrive(disk, refresh)
	var poweron uint64
	var temp float64
	var failing, failed int32
	var bad int64
	for _, a := range st.smart.Attributes {
		switch a.ID {
		case 9:
			poweron = (a.Raw & 0xffffffff) * 3600
		case 194:
			temp = float64(a.Raw&0xff) + 273.15
		case 5, 197:
			bad += int64(a.Raw & 0xffffffff)
		}
		if a.Failing() {
			failing++
		}
		if a.FailedInThePast() {
			failed++
		}
	}
	id := st.id
	return map[string]interface{}{
		"SmartSupported": id.SmartSupported, "SmartEnabled": id.SmartEnabled, "SmartUpdated": st.updated,
		"SmartFailing": st.failing, "SmartPowerOnSeconds": poweron, "SmartTemperature": temp,
		"SmartNumAttributesFailing": failing, "SmartNumAttributesFailedInThePast": failed, "SmartNumBadSectors": bad,
		"SmartSelftestStatus": st.smart.SelftestStatus, "SmartSelftestPercentRemaining": st.smart.SelftestRemaining,
		"PmSupported": id.PmSupported, "PmEnabled": id.PmEnabled, "ApmSupported": id.ApmSupported, "ApmEnabled": id.ApmEnabled,
		"AamSupported": id.AamSupported, "AamEnabled": id.AamEnabled, "AamVendorRecommendedValue": int32(0),
		"WriteCacheSupported": id.WriteCacheSupported, "WriteCacheEnabled": id.WriteCacheEnabled,
		"ReadLookaheadSupported": id.ReadLookaheadSupported, "ReadLookaheadEnabled": id.ReadLookahead,
		"SecurityEraseUnitMinutes": id.SecurityEraseMinutes, "SecurityEnhancedEraseUnitMinutes": id.EnhancedEraseMinutes,
		"SecurityFrozen": id.SecurityFrozen,
	}
}

type ataObject struct {
	s    *Service
	disk string
}

type smartAttr struct {
	ID         uint8
	Name       string
	Flags      uint16
	Value      int32
	Worst      int32
	Threshold  int32
	Pretty     int64
	PrettyUnit int32
	Expansion  map[string]dbus.Variant
}

func (a *ataObject) SmartUpdate(options map[string]dbus.Variant) *dbus.Error {
	a.s.mu.Lock()
	a.s.ataDrive(a.disk, true)
	a.s.mu.Unlock()
	a.s.Refresh()
	return nil
}

func (a *ataObject) SmartGetAttributes(options map[string]dbus.Variant) ([]smartAttr, *dbus.Error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	st := a.s.ataDrive(a.disk, true)
	out := []smartAttr{}
	for _, x := range st.smart.Attributes {
		pretty, unit := int64(x.Raw&0xffffffffffff), int32(1)
		switch x.ID {
		case 9:
			pretty, unit = int64(x.Raw&0xffffffff)*3600000, 2
		case 194, 190:
			pretty, unit = int64(x.Raw&0xff)*1000+273150, 3
		case 5, 196, 197, 198:
			unit = 4
		}
		out = append(out, smartAttr{x.ID, x.Name, x.Flags, x.Value, x.Worst, x.Threshold, pretty, unit, map[string]dbus.Variant{}})
	}
	return out, nil
}

func (a *ataObject) selftest(sender dbus.Sender, kind string) *dbus.Error {
	uid, _, _, err := a.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	if uid != 0 && uid < 1000 {
		return notAuthorized("run a self-test")
	}
	d, err := ata.Open("/dev/" + a.disk)
	if err != nil {
		return failed(err)
	}
	defer d.Close()
	if err := d.SelfTest(kind); err != nil {
		return failed(err)
	}
	a.s.mu.Lock()
	a.s.ataDrive(a.disk, true)
	a.s.mu.Unlock()
	a.s.Refresh()
	return nil
}

func (a *ataObject) SmartSelftestStart(sender dbus.Sender, kind string, options map[string]dbus.Variant) *dbus.Error {
	if kind != "short" && kind != "extended" && kind != "conveyance" {
		return failed(errInvalid("self-test type " + kind))
	}
	return a.selftest(sender, kind)
}

func (a *ataObject) SmartSelftestAbort(sender dbus.Sender, options map[string]dbus.Variant) *dbus.Error {
	return a.selftest(sender, "abort")
}

func (a *ataObject) SmartSetEnabled(sender dbus.Sender, value bool, options map[string]dbus.Variant) *dbus.Error {
	uid, _, _, err := a.s.caller(sender)
	if err != nil {
		return failed(err)
	}
	if uid != 0 {
		return notAuthorized("change SMART")
	}
	if !value {
		return dbus.NewError("org.freedesktop.UDisks2.Error.NotSupported", []interface{}{"turning SMART off is not supported"})
	}
	d, err := ata.Open("/dev/" + a.disk)
	if err != nil {
		return failed(err)
	}
	defer d.Close()
	if err := d.Enable(); err != nil {
		return failed(err)
	}
	return a.SmartUpdate(options)
}

func (a *ataObject) PmGetState(options map[string]dbus.Variant) (uint8, *dbus.Error) {
	return 0xff, nil
}
