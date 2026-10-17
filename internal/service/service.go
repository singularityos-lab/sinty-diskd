// Package service publishes the disk model on the system bus as
// org.freedesktop.UDisks2, with its object manager, so UDisks2 clients and
// libudisks2 keep working unchanged.
package service

import (
	"fmt"
	"log"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"github.com/singularityos-lab/sinty-diskd/internal/model"
)

const (
	busName       = "org.freedesktop.UDisks2"
	rootPath      = dbus.ObjectPath("/org/freedesktop/UDisks2")
	managerPath   = dbus.ObjectPath("/org/freedesktop/UDisks2/Manager")
	drivesPrefix  = "/org/freedesktop/UDisks2/drives/"
	ifManager     = "org.freedesktop.UDisks2.Manager"
	ifBlock       = "org.freedesktop.UDisks2.Block"
	ifPartition   = "org.freedesktop.UDisks2.Partition"
	ifTable       = "org.freedesktop.UDisks2.PartitionTable"
	ifFilesystem  = "org.freedesktop.UDisks2.Filesystem"
	ifSwapspace   = "org.freedesktop.UDisks2.Swapspace"
	ifLoop        = "org.freedesktop.UDisks2.Loop"
	ifDrive       = "org.freedesktop.UDisks2.Drive"
	ifObjManager  = "org.freedesktop.DBus.ObjectManager"
	daemonVersion = "2.11.0"
)

type object struct {
	path   dbus.ObjectPath
	ifaces map[string]map[string]interface{}
	props  *prop.Properties
}

// Service is the running daemon.
type Service struct {
	src      model.Sources
	conn     *dbus.Conn
	mu       sync.Mutex
	model    *model.Model
	objects  map[dbus.ObjectPath]*object
	detected map[string]uint64
}

// New connects to the system bus and publishes the manager.
func New(src model.Sources) (*Service, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	s := &Service{src: src, conn: conn, objects: map[dbus.ObjectPath]*object{}, detected: map[string]uint64{}}
	if err := conn.Export(&objectManager{s}, rootPath, ifObjManager); err != nil {
		conn.Close()
		return nil, err
	}
	root := &introspect.Node{Name: string(rootPath), Interfaces: []introspect.Interface{introspect.IntrospectData,
		{Name: ifObjManager, Methods: introspect.Methods(&objectManager{s}),
			Signals: []introspect.Signal{{Name: "InterfacesAdded"}, {Name: "InterfacesRemoved"}}}}}
	conn.Export(introspect.NewIntrospectable(root), rootPath, "org.freedesktop.DBus.Introspectable")
	if err := s.exportManager(); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := conn.RequestName(busName, dbus.NameFlagReplaceExisting|dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return nil, fmt.Errorf("%s is owned by another process", busName)
	}
	return s, nil
}

func (s *Service) exportManager() error {
	m := &managerObject{s}
	if err := s.conn.Export(m, managerPath, ifManager); err != nil {
		return err
	}
	ifs := map[string]map[string]interface{}{ifManager: {
		"Version":                  daemonVersion,
		"SupportedFilesystems":     supportedFilesystems(),
		"SupportedEncryptionTypes": []string{"luks1", "luks2"},
		"DefaultEncryptionType":    "luks2",
	}}
	return s.publish(managerPath, ifs, map[string]interface{}{ifManager: m})
}

func propMap(ifs map[string]map[string]interface{}) prop.Map {
	out := prop.Map{}
	for iface, props := range ifs {
		m := map[string]*prop.Prop{}
		for k, v := range props {
			m[k] = &prop.Prop{Value: v, Emit: prop.EmitTrue}
		}
		out[iface] = m
	}
	return out
}

func (s *Service) publish(path dbus.ObjectPath, ifs map[string]map[string]interface{}, methods map[string]interface{}) error {
	for iface, obj := range methods {
		if err := s.conn.Export(obj, path, iface); err != nil {
			return err
		}
	}
	props, err := prop.Export(s.conn, path, propMap(ifs))
	if err != nil {
		return err
	}
	node := &introspect.Node{Name: string(path), Interfaces: []introspect.Interface{introspect.IntrospectData, prop.IntrospectData}}
	names := make([]string, 0, len(ifs))
	for iface := range ifs {
		names = append(names, iface)
	}
	sort.Strings(names)
	for _, iface := range names {
		i := introspect.Interface{Name: iface, Properties: props.Introspection(iface)}
		if obj, ok := methods[iface]; ok {
			i.Methods = introspect.Methods(obj)
		}
		node.Interfaces = append(node.Interfaces, i)
	}
	s.conn.Export(introspect.NewIntrospectable(node), path, "org.freedesktop.DBus.Introspectable")
	s.objects[path] = &object{path: path, ifaces: ifs, props: props}
	return nil
}

func (s *Service) unpublish(path dbus.ObjectPath) {
	o := s.objects[path]
	if o == nil {
		return
	}
	for iface := range o.ifaces {
		s.conn.Export(nil, path, iface)
	}
	s.conn.Export(nil, path, "org.freedesktop.DBus.Properties")
	s.conn.Export(nil, path, "org.freedesktop.DBus.Introspectable")
	delete(s.objects, path)
}

func ay(str string) []byte { return append([]byte(str), 0) }

func ays(list []string) [][]byte {
	out := make([][]byte, 0, len(list))
	for _, x := range list {
		out = append(out, ay(x))
	}
	return out
}

type confItem struct {
	Kind   string
	Fields map[string]dbus.Variant
}

func blockIfaces(b *model.Block) map[string]map[string]interface{} {
	conf := []confItem{}
	for _, c := range b.Configuration {
		f := map[string]dbus.Variant{}
		for k, v := range c.Fields {
			if str, ok := v.(string); ok {
				f[k] = dbus.MakeVariant(ay(str))
			} else {
				f[k] = dbus.MakeVariant(v)
			}
		}
		conf = append(conf, confItem{c.Kind, f})
	}
	ifs := map[string]map[string]interface{}{ifBlock: {
		"Device": ay(b.Device), "PreferredDevice": ay(b.PreferredDevice), "Symlinks": ays(b.Symlinks),
		"DeviceNumber": b.DeviceNumber, "Id": b.ID, "Size": b.Size, "ReadOnly": b.ReadOnly,
		"Drive": dbus.ObjectPath(b.Drive), "MDRaid": dbus.ObjectPath("/"), "MDRaidMember": dbus.ObjectPath("/"),
		"IdUsage": b.IDUsage, "IdType": b.IDType, "IdVersion": b.IDVersion, "IdLabel": b.IDLabel, "IdUUID": b.IDUUID,
		"Configuration": conf, "CryptoBackingDevice": dbus.ObjectPath(b.CryptoBackingDevice),
		"HintPartitionable": b.HintPartitionable, "HintSystem": b.HintSystem, "HintIgnore": b.HintIgnore,
		"HintAuto": b.HintAuto, "HintName": b.HintName, "HintIconName": b.HintIconName,
		"HintSymbolicIconName": b.HintSymbolicIconName, "UserspaceMountOptions": []string{},
	}}
	if p := b.Partition; p != nil {
		ifs[ifPartition] = map[string]interface{}{"Number": p.Number, "Type": p.Type, "Flags": p.Flags,
			"Offset": p.Offset, "Size": p.Size, "Name": p.Name, "UUID": p.UUID, "Table": dbus.ObjectPath(p.Table),
			"IsContainer": p.IsContainer, "IsContained": p.IsContained}
	}
	if t := b.PartitionTable; t != nil {
		parts := make([]dbus.ObjectPath, 0, len(t.Partitions))
		for _, p := range t.Partitions {
			parts = append(parts, dbus.ObjectPath(p))
		}
		ifs[ifTable] = map[string]interface{}{"Type": t.Type, "Partitions": parts}
	}
	if f := b.Filesystem; f != nil {
		ifs[ifFilesystem] = map[string]interface{}{"MountPoints": ays(f.MountPoints), "Size": f.Size}
	}
	if sw := b.Swapspace; sw != nil {
		ifs[ifSwapspace] = map[string]interface{}{"Active": sw.Active}
	}
	if l := b.Loop; l != nil {
		ifs[ifLoop] = map[string]interface{}{"BackingFile": ay(l.BackingFile), "Autoclear": l.Autoclear, "SetupByUID": l.SetupByUID}
	}
	return ifs
}

func driveIfaces(d *model.Drive) map[string]map[string]interface{} {
	return map[string]map[string]interface{}{ifDrive: {
		"Vendor": d.Vendor, "Model": d.Model, "Revision": d.Revision, "Serial": d.Serial, "WWN": d.WWN, "Id": d.ID,
		"Configuration": map[string]dbus.Variant{}, "Media": d.Media, "MediaCompatibility": d.MediaCompatibility,
		"MediaRemovable": d.MediaRemovable, "MediaAvailable": d.MediaAvailable, "MediaChangeDetected": d.MediaChangeDetected,
		"Size": d.Size, "TimeDetected": d.TimeDetected, "TimeMediaDetected": d.TimeMediaDetected,
		"Optical": d.Optical, "OpticalBlank": false, "OpticalNumTracks": uint32(0), "OpticalNumAudioTracks": uint32(0),
		"OpticalNumDataTracks": uint32(0), "OpticalNumSessions": uint32(0), "RotationRate": d.RotationRate,
		"ConnectionBus": d.ConnectionBus, "Seat": d.Seat, "Removable": d.Removable, "Ejectable": d.Ejectable,
		"SortKey": d.SortKey, "CanPowerOff": d.CanPowerOff, "SiblingId": d.SiblingID,
	}}
}

// Refresh rebuilds the model and publishes what changed: new objects with
// InterfacesAdded, gone ones with InterfacesRemoved, and changed properties.
func (s *Service) Refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := model.Scan(s.src, s.detected)
	s.model = m
	want := map[dbus.ObjectPath]map[string]map[string]interface{}{}
	methods := map[dbus.ObjectPath]map[string]interface{}{}
	for _, d := range m.Drives {
		path := dbus.ObjectPath(drivesPrefix + d.Object)
		want[path] = driveIfaces(d)
		methods[path] = map[string]interface{}{ifDrive: &driveObject{s, d.Object}}
	}
	for _, b := range m.Blocks {
		path := dbus.ObjectPath(model.BlockPath(b.Name))
		want[path] = blockIfaces(b)
		mm := map[string]interface{}{ifBlock: &blockObject{s, b.Name}}
		if b.Filesystem != nil {
			mm[ifFilesystem] = &filesystemObject{s, b.Name}
		}
		if b.Partition != nil {
			mm[ifPartition] = &partitionObject{blockObject{s, b.Name}}
		}
		if b.PartitionTable != nil {
			mm[ifTable] = &tableObject{blockObject{s, b.Name}}
		}
		if b.Swapspace != nil {
			mm[ifSwapspace] = &swapObject{s, b.Name}
		}
		methods[path] = mm
	}
	for path, o := range s.objects {
		if path == managerPath {
			continue
		}
		if _, ok := want[path]; !ok {
			var gone []string
			for iface := range o.ifaces {
				gone = append(gone, iface)
			}
			s.unpublish(path)
			s.conn.Emit(rootPath, ifObjManager+".InterfacesRemoved", path, gone)
		}
	}
	for path, ifs := range want {
		o, ok := s.objects[path]
		if ok && sameIfaces(o.ifaces, ifs) {
			for iface, props := range ifs {
				for k, v := range props {
					if fmt.Sprint(o.ifaces[iface][k]) != fmt.Sprint(v) {
						o.props.SetMust(iface, k, v)
					}
				}
			}
			o.ifaces = ifs
			continue
		}
		if ok {
			s.unpublish(path)
		}
		if err := s.publish(path, ifs, methods[path]); err != nil {
			log.Printf("publish %s: %v", path, err)
			continue
		}
		s.conn.Emit(rootPath, ifObjManager+".InterfacesAdded", path, toVariants(ifs))
	}
}

func sameIfaces(a, b map[string]map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func toVariants(ifs map[string]map[string]interface{}) map[string]map[string]dbus.Variant {
	out := map[string]map[string]dbus.Variant{}
	for iface, props := range ifs {
		m := map[string]dbus.Variant{}
		for k, v := range props {
			m[k] = dbus.MakeVariant(v)
		}
		out[iface] = m
	}
	return out
}

type objectManager struct{ s *Service }

func (o *objectManager) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	out := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for path, obj := range o.s.objects {
		out[path] = toVariants(obj.ifaces)
	}
	return out, nil
}

type managerObject struct{ s *Service }

func (m *managerObject) GetBlockDevices(options map[string]dbus.Variant) ([]dbus.ObjectPath, *dbus.Error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	var out []dbus.ObjectPath
	for path := range m.s.objects {
		if strings.HasPrefix(string(path), "/org/freedesktop/UDisks2/block_devices/") {
			out = append(out, path)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (m *managerObject) ResolveDevice(devspec map[string]dbus.Variant, options map[string]dbus.Variant) ([]dbus.ObjectPath, *dbus.Error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	var out []dbus.ObjectPath
	for _, b := range m.s.model.Blocks {
		match := false
		if v, ok := devspec["path"]; ok {
			p, _ := v.Value().(string)
			match = p == b.Device || p == b.PreferredDevice || contains(b.Symlinks, p)
		}
		if v, ok := devspec["label"]; ok {
			l, _ := v.Value().(string)
			match = match || (l != "" && l == b.IDLabel)
		}
		if v, ok := devspec["uuid"]; ok {
			u, _ := v.Value().(string)
			match = match || (u != "" && strings.EqualFold(u, b.IDUUID))
		}
		if match {
			out = append(out, dbus.ObjectPath(model.BlockPath(b.Name)))
		}
	}
	return out, nil
}

func (m *managerObject) CanFormat(fstype string) (struct {
	Available bool
	Utility   string
}, *dbus.Error) {
	tool, ok := mkfsTools[fstype]
	r := struct {
		Available bool
		Utility   string
	}{}
	if !ok {
		return r, nil
	}
	if have(tool) {
		r.Available = true
	} else {
		r.Utility = tool
	}
	return r, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) caller(sender dbus.Sender) (uint32, string, uint32, error) {
	var uid uint32
	if err := s.conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixUser", 0, string(sender)).Store(&uid); err != nil {
		return 0, "", 0, err
	}
	u, err := user.LookupId(strconv.Itoa(int(uid)))
	if err != nil {
		return uid, "", uid, nil
	}
	gid, _ := strconv.Atoi(u.Gid)
	return uid, u.Username, uint32(gid), nil
}

func notAuthorized(what string) *dbus.Error {
	return dbus.NewError("org.freedesktop.UDisks2.Error.NotAuthorized", []interface{}{"not authorized to " + what})
}

func failed(err error) *dbus.Error {
	return dbus.NewError("org.freedesktop.UDisks2.Error.Failed", []interface{}{err.Error()})
}
