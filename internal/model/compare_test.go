package model

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func ay(s string) string { return "AY:" + s }

func ays(list []string) []interface{} {
	out := []interface{}{}
	for _, s := range list {
		out = append(out, ay(s))
	}
	return out
}

func blockProps(b *Block) map[string]interface{} {
	conf := []interface{}{}
	for _, c := range b.Configuration {
		f := map[string]interface{}{}
		for k, v := range c.Fields {
			if s, ok := v.(string); ok {
				f[k] = ay(s)
			} else {
				f[k] = v
			}
		}
		conf = append(conf, []interface{}{c.Kind, f})
	}
	return map[string]interface{}{
		"Device": ay(b.Device), "PreferredDevice": ay(b.PreferredDevice), "Symlinks": ays(b.Symlinks),
		"DeviceNumber": b.DeviceNumber, "Id": b.ID, "Size": b.Size, "ReadOnly": b.ReadOnly, "Drive": b.Drive,
		"MDRaid": "/", "MDRaidMember": "/", "IdUsage": b.IDUsage, "IdType": b.IDType, "IdVersion": b.IDVersion,
		"IdLabel": b.IDLabel, "IdUUID": b.IDUUID, "Configuration": conf, "CryptoBackingDevice": b.CryptoBackingDevice,
		"HintPartitionable": b.HintPartitionable, "HintSystem": b.HintSystem, "HintIgnore": b.HintIgnore,
		"HintAuto": b.HintAuto, "HintName": b.HintName, "HintIconName": b.HintIconName,
		"HintSymbolicIconName": b.HintSymbolicIconName, "UserspaceMountOptions": []interface{}{},
	}
}

func normalize(v interface{}) string {
	data, _ := json.Marshal(v)
	var any interface{}
	json.Unmarshal(data, &any)
	if list, ok := any.([]interface{}); ok {
		strs := make([]string, len(list))
		for i, x := range list {
			d, _ := json.Marshal(x)
			strs[i] = string(d)
		}
		sort.Strings(strs)
		return strings.Join(strs, ",")
	}
	d, _ := json.Marshal(any)
	return string(d)
}

// TestMatchesUDisks compares the model built from a real machine with what
// UDisks2 published for it. SINTY_DISKD_REF names a directory with the udev
// database, mountinfo, fstab, swaps and managed-s.json captured on that
// machine; /sys must be that machine's.
func TestMatchesUDisks(t *testing.T) {
	ref := os.Getenv("SINTY_DISKD_REF")
	if ref == "" {
		t.Skip("SINTY_DISKD_REF is not set")
	}
	var want map[string]map[string]map[string]interface{}
	data, err := os.ReadFile(filepath.Join(ref, "managed-s.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(data, &want)
	m := Scan(Sources{
		Sys: "/sys", UdevData: filepath.Join(ref, "udevdb"), MountInfo: filepath.Join(ref, "mountinfo"),
		Fstab: filepath.Join(ref, "fstab"), Swaps: filepath.Join(ref, "swaps"),
	}, nil)
	got := map[string]map[string]map[string]interface{}{}
	for _, b := range m.Blocks {
		ifs := map[string]map[string]interface{}{"org.freedesktop.UDisks2.Block": blockProps(b)}
		if p := b.Partition; p != nil {
			ifs["org.freedesktop.UDisks2.Partition"] = map[string]interface{}{"Number": p.Number, "Type": p.Type, "Flags": p.Flags,
				"Offset": p.Offset, "Size": p.Size, "Name": p.Name, "UUID": p.UUID, "Table": p.Table, "IsContainer": p.IsContainer, "IsContained": p.IsContained}
		}
		if p := b.PartitionTable; p != nil {
			ifs["org.freedesktop.UDisks2.PartitionTable"] = map[string]interface{}{"Type": p.Type, "Partitions": p.Partitions}
		}
		if f := b.Filesystem; f != nil {
			ifs["org.freedesktop.UDisks2.Filesystem"] = map[string]interface{}{"MountPoints": ays(f.MountPoints), "Size": f.Size}
		}
		if s := b.Swapspace; s != nil {
			ifs["org.freedesktop.UDisks2.Swapspace"] = map[string]interface{}{"Active": s.Active}
		}
		got[BlockPath(b.Name)] = ifs
	}
	for _, d := range m.Drives {
		got["/org/freedesktop/UDisks2/drives/"+d.Object] = map[string]map[string]interface{}{"org.freedesktop.UDisks2.Drive": {
			"Vendor": d.Vendor, "Model": d.Model, "Revision": d.Revision, "Serial": d.Serial, "WWN": d.WWN, "Id": d.ID,
			"Media": d.Media, "MediaCompatibility": d.MediaCompatibility, "MediaRemovable": d.MediaRemovable,
			"MediaAvailable": d.MediaAvailable, "MediaChangeDetected": d.MediaChangeDetected, "Size": d.Size,
			"Optical": d.Optical, "OpticalBlank": false, "OpticalNumTracks": 0, "OpticalNumAudioTracks": 0,
			"OpticalNumDataTracks": 0, "OpticalNumSessions": 0, "RotationRate": d.RotationRate, "ConnectionBus": d.ConnectionBus,
			"Seat": d.Seat, "Removable": d.Removable, "Ejectable": d.Ejectable, "SortKey": d.SortKey,
			"CanPowerOff": d.CanPowerOff, "SiblingId": d.SiblingID, "Configuration": map[string]interface{}{},
		}}
	}
	var diffs []string
	for path, wifs := range want {
		if strings.HasSuffix(path, "/Manager") {
			continue
		}
		gifs, ok := got[path]
		if !ok {
			diffs = append(diffs, "missing object "+path)
			continue
		}
		for iface, wprops := range wifs {
			if strings.Contains(iface, ".NVMe.") {
				continue
			}
			gprops, ok := gifs[iface]
			if !ok {
				diffs = append(diffs, fmt.Sprintf("%s: missing %s", path, iface))
				continue
			}
			for k, wv := range wprops {
				if k == "TimeDetected" || k == "TimeMediaDetected" {
					continue
				}
				gv, ok := gprops[k]
				if !ok {
					diffs = append(diffs, fmt.Sprintf("%s %s.%s: not published", path, iface, k))
					continue
				}
				if normalize(gv) != normalize(wv) {
					diffs = append(diffs, fmt.Sprintf("%s %s.%s: got %s want %s", path, iface, k, normalize(gv), normalize(wv)))
				}
			}
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			diffs = append(diffs, "extra object "+path)
		}
	}
	sort.Strings(diffs)
	for _, d := range diffs {
		t.Error(d)
	}
}
