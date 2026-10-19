// Package model builds the disk model UDisks2 publishes, drives and block
// devices with their partitions, filesystems and swap, from sysfs, the udev
// database, the mount table and fstab.
package model

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sources locates the system state the model is built from.
type Sources struct {
	Sys       string
	UdevData  string
	MountInfo string
	Fstab     string
	Crypttab  string
	Swaps     string
}

// DefaultSources reads the running system.
func DefaultSources() Sources {
	return Sources{
		Sys:       "/sys",
		UdevData:  "/run/udev/data",
		MountInfo: "/proc/self/mountinfo",
		Fstab:     "/etc/fstab",
		Crypttab:  "/etc/crypttab",
		Swaps:     "/proc/swaps",
	}
}

// ConfigItem is one entry of Block.Configuration: an fstab or crypttab line
// that refers to the device.
type ConfigItem struct {
	Kind   string
	Fields map[string]interface{}
}

// Block is a block device with the UDisks2 Block properties and the optional
// interfaces it carries.
type Block struct {
	Name                 string
	Device               string
	PreferredDevice      string
	Symlinks             []string
	DeviceNumber         uint64
	ID                   string
	Size                 uint64
	ReadOnly             bool
	Drive                string
	IDUsage              string
	IDType               string
	IDVersion            string
	IDLabel              string
	IDUUID               string
	Configuration        []ConfigItem
	CryptoBackingDevice  string
	HintPartitionable    bool
	HintSystem           bool
	HintIgnore           bool
	HintAuto             bool
	HintName             string
	HintIconName         string
	HintSymbolicIconName string

	Partition      *Partition
	PartitionTable *PartitionTable
	Filesystem     *Filesystem
	Swapspace      *Swapspace
	Loop           *Loop
	Encrypted      *Encrypted

	major, minor int
	disk         string
	slaves       []string
	props        map[string]string
}

// Partition holds the UDisks2 Partition properties.
type Partition struct {
	Number      uint32
	Type        string
	Flags       uint64
	Offset      uint64
	Size        uint64
	Name        string
	UUID        string
	Table       string
	IsContainer bool
	IsContained bool
}

// PartitionTable holds the UDisks2 PartitionTable properties.
type PartitionTable struct {
	Type       string
	Partitions []string
}

// Filesystem holds the UDisks2 Filesystem properties.
type Filesystem struct {
	MountPoints []string
	Size        uint64
}

// Swapspace holds the UDisks2 Swapspace properties.
type Swapspace struct {
	Active bool
}

// Encrypted holds the UDisks2 Encrypted properties.
type Encrypted struct {
	HintEncryptionType string
	MetadataSize       uint64
	CleartextDevice    string
}

// Prop returns a property of the device from the udev database.
func (b *Block) Prop(key string) string { return b.props[key] }

// MapperName is the device-mapper name of the cleartext device, empty when
// the block is not an unlocked cleartext device.
func (b *Block) MapperName() string { return b.props["DM_NAME"] }

// Loop holds the UDisks2 Loop properties.
type Loop struct {
	BackingFile string
	Autoclear   bool
	SetupByUID  uint32
}

// Drive is a physical drive with the UDisks2 Drive properties.
type Drive struct {
	Object              string
	Vendor, Model       string
	Revision, Serial    string
	WWN                 string
	ID                  string
	Media               string
	MediaCompatibility  []string
	MediaRemovable      bool
	MediaAvailable      bool
	MediaChangeDetected bool
	Size                uint64
	TimeDetected        uint64
	TimeMediaDetected   uint64
	Optical             bool
	RotationRate        int32
	ConnectionBus       string
	Seat                string
	Removable           bool
	Ejectable           bool
	SortKey             string
	CanPowerOff         bool
	SiblingID           string

	block string
	nvme  bool
}

// Block is the name of the block device of the drive.
func (d *Drive) Block() string { return d.block }

// Model is everything published at one moment.
type Model struct {
	Blocks map[string]*Block
	Drives map[string]*Drive
}

// ObjectPath turns a name into the object path element UDisks2 uses: every
// byte that is not a letter, a digit or an underscore becomes _xx in hex.
func ObjectPath(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteString("_" + strconv.FormatUint(uint64(c), 16))
		}
	}
	return b.String()
}

// BlockPath is the object path of a block device.
func BlockPath(name string) string {
	return "/org/freedesktop/UDisks2/block_devices/" + ObjectPath(name)
}

func driveObjectName(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readUdev(dir string, major, minor int) (map[string]string, []string) {
	props := map[string]string{}
	var links []string
	f, err := os.Open(filepath.Join(dir, "b"+strconv.Itoa(major)+":"+strconv.Itoa(minor)))
	if err != nil {
		return props, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "E:"):
			if k, v, ok := strings.Cut(line[2:], "="); ok {
				props[k] = v
			}
		case strings.HasPrefix(line, "S:"):
			links = append(links, "/dev/"+line[2:])
		}
	}
	return props, links
}

// Decode turns the \xNN escapes of the *_ENC udev properties back into text.
func Decode(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func boolProp(props map[string]string, key string) (bool, bool) {
	v, ok := props[key]
	if !ok {
		return false, false
	}
	return v == "1" || strings.EqualFold(v, "true"), true
}

// Scan builds the model from the sources.
func Scan(src Sources, detected map[string]uint64) *Model {
	m := &Model{Blocks: map[string]*Block{}, Drives: map[string]*Drive{}}
	entries, _ := os.ReadDir(filepath.Join(src.Sys, "class/block"))
	mounts := readMounts(src.MountInfo)
	swaps := readSwaps(src.Swaps)
	fstab := readTab(src.Fstab)
	now := uint64(time.Now().UnixMicro())
	for _, e := range entries {
		name := e.Name()
		dir := filepath.Join(src.Sys, "class/block", name)
		var major, minor int
		if n, _ := fmtScan(readFile(filepath.Join(dir, "dev")), &major, &minor); n != 2 {
			continue
		}
		if strings.HasPrefix(name, "ram") {
			continue
		}
		if strings.HasPrefix(name, "loop") && readFile(filepath.Join(dir, "loop/backing_file")) == "" {
			continue
		}
		props, links := readUdev(src.UdevData, major, minor)
		if props["DM_UDEV_DISABLE_OTHER_RULES_FLAG"] == "1" {
			continue
		}
		sort.Strings(links)
		b := &Block{
			Name:                name,
			Device:              "/dev/" + name,
			Symlinks:            links,
			DeviceNumber:        uint64(major<<8 | minor),
			ReadOnly:            readFile(filepath.Join(dir, "ro")) == "1",
			Drive:               "/",
			CryptoBackingDevice: "/",
			major:               major,
			minor:               minor,
			props:               props,
		}
		if v, err := strconv.ParseUint(readFile(filepath.Join(dir, "size")), 10, 64); err == nil {
			b.Size = v * 512
		}
		b.PreferredDevice = b.Device
		if vg, lv := props["DM_VG_NAME"], props["DM_LV_NAME"]; vg != "" && lv != "" {
			b.PreferredDevice = "/dev/" + vg + "/" + lv
		} else if n := props["DM_NAME"]; n != "" {
			b.PreferredDevice = "/dev/mapper/" + n
		}
		for _, l := range links {
			if strings.HasPrefix(l, "/dev/disk/by-id/") {
				b.ID = "by-id-" + strings.TrimPrefix(l, "/dev/disk/by-id/")
				break
			}
		}
		if b.ID == "" && (props["ID_FS_USAGE"] == "filesystem" || props["ID_FS_USAGE"] == "crypto") {
			for _, l := range links {
				if strings.HasPrefix(l, "/dev/disk/by-uuid/") {
					b.ID = "by-uuid-" + strings.TrimPrefix(l, "/dev/disk/by-uuid/")
					break
				}
			}
		}
		b.IDUsage = props["ID_FS_USAGE"]
		b.IDType = props["ID_FS_TYPE"]
		b.IDVersion = props["ID_FS_VERSION"]
		b.IDLabel = Decode(firstOf(props, "ID_FS_LABEL_ENC", "ID_FS_LABEL"))
		b.IDUUID = Decode(firstOf(props, "ID_FS_UUID_ENC", "ID_FS_UUID"))
		b.HintPartitionable = !strings.HasPrefix(name, "dm-")
		b.HintIgnore, _ = boolProp(props, "UDISKS_IGNORE")
		b.HintName = props["UDISKS_NAME"]
		b.HintIconName = props["UDISKS_ICON_NAME"]
		b.HintSymbolicIconName = props["UDISKS_SYMBOLIC_ICON_NAME"]
		b.disk = name
		if readFile(filepath.Join(dir, "partition")) != "" {
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				b.disk = filepath.Base(filepath.Dir(real))
			}
		}
		b.Configuration = configFor(b, fstab)
		switch {
		case b.IDUsage == "filesystem":
			fs := &Filesystem{MountPoints: mountsOf(b, mounts)}
			switch b.IDType {
			case "ext2", "ext3", "ext4", "xfs", "btrfs":
				fs.Size, _ = strconv.ParseUint(props["ID_FS_SIZE"], 10, 64)
			}
			b.Filesystem = fs
		case b.IDUsage == "other" && b.IDType == "swap":
			b.Swapspace = &Swapspace{Active: swaps[b.Device]}
		}
		if b.IDUsage == "crypto" {
			b.Encrypted = &Encrypted{HintEncryptionType: strings.ToUpper(strings.TrimPrefix(b.IDType, "crypto_")) + b.IDVersion, CleartextDevice: "/"}
			if b.IDType == "crypto_LUKS" {
				b.Encrypted.HintEncryptionType = "luks" + b.IDVersion
				b.Encrypted.MetadataSize = 16 << 20
				if b.IDVersion == "1" {
					b.Encrypted.MetadataSize = 2 << 20
				}
			}
		}
		if strings.HasPrefix(name, "dm-") {
			entries, _ := os.ReadDir(filepath.Join(dir, "slaves"))
			for _, e := range entries {
				b.slaves = append(b.slaves, e.Name())
			}
			if strings.HasPrefix(props["DM_UUID"], "CRYPT-") && len(b.slaves) == 1 {
				b.CryptoBackingDevice = b.slaves[0]
			}
		}
		if backing := readFile(filepath.Join(dir, "loop/backing_file")); backing != "" {
			b.Loop = &Loop{BackingFile: backing, Autoclear: readFile(filepath.Join(dir, "loop/autoclear")) == "1"}
		}
		m.Blocks[name] = b
	}
	for _, b := range m.Blocks {
		if b.disk != b.Name {
			continue
		}
		if t := b.props["ID_PART_TABLE_TYPE"]; t != "" {
			b.PartitionTable = &PartitionTable{Type: t, Partitions: []string{}}
		}
	}
	for _, b := range m.Blocks {
		if b.disk == b.Name || b.props["ID_PART_ENTRY_NUMBER"] == "" {
			continue
		}
		parent := m.Blocks[b.disk]
		p := &Partition{Table: "/"}
		n, _ := strconv.ParseUint(b.props["ID_PART_ENTRY_NUMBER"], 10, 32)
		p.Number = uint32(n)
		p.Type = b.props["ID_PART_ENTRY_TYPE"]
		p.Flags, _ = strconv.ParseUint(strings.TrimPrefix(b.props["ID_PART_ENTRY_FLAGS"], "0x"), 16, 64)
		off, _ := strconv.ParseUint(b.props["ID_PART_ENTRY_OFFSET"], 10, 64)
		size, _ := strconv.ParseUint(b.props["ID_PART_ENTRY_SIZE"], 10, 64)
		p.Offset, p.Size = off*512, size*512
		p.Name = Decode(b.props["ID_PART_ENTRY_NAME"])
		p.UUID = b.props["ID_PART_ENTRY_UUID"]
		if parent != nil && parent.PartitionTable != nil {
			p.Table = BlockPath(parent.Name)
			parent.PartitionTable.Partitions = append(parent.PartitionTable.Partitions, BlockPath(b.Name))
			if parent.PartitionTable.Type == "dos" {
				switch p.Type {
				case "0x05", "0x0f", "0x85":
					p.IsContainer = true
				}
				p.IsContained = p.Number >= 5
			}
		}
		b.Partition = p
	}
	for _, b := range m.Blocks {
		if b.CryptoBackingDevice == "/" {
			continue
		}
		backing := m.Blocks[b.CryptoBackingDevice]
		b.CryptoBackingDevice = "/"
		if backing != nil {
			b.CryptoBackingDevice = BlockPath(backing.Name)
			if backing.Encrypted != nil {
				backing.Encrypted.CleartextDevice = BlockPath(b.Name)
			}
		}
	}
	for _, b := range m.Blocks {
		if b.disk != b.Name {
			continue
		}
		if d := driveFor(src, b, detected, now); d != nil {
			m.Drives[d.Object] = d
			b.Drive = "/org/freedesktop/UDisks2/drives/" + d.Object
		}
	}
	for _, b := range m.Blocks {
		if b.disk != b.Name {
			if parent := m.Blocks[b.disk]; parent != nil {
				b.Drive = parent.Drive
			}
		}
		system := true
		if d := m.driveOf(b); d != nil && (d.Removable || d.ConnectionBus == "usb" || d.ConnectionBus == "sdio" || d.ConnectionBus == "ieee1394") {
			system = false
		}
		if v, ok := boolProp(b.props, "UDISKS_SYSTEM"); ok {
			system = v
		}
		b.HintSystem = system
		auto := !system
		if v, ok := boolProp(b.props, "UDISKS_AUTO"); ok {
			auto = v
		}
		b.HintAuto = auto
	}
	for _, b := range m.Blocks {
		if b.CryptoBackingDevice == "/" {
			continue
		}
		for _, backing := range m.Blocks {
			if BlockPath(backing.Name) == b.CryptoBackingDevice {
				b.HintSystem, b.HintAuto = backing.HintSystem, backing.HintAuto
			}
		}
	}
	return m
}

func (m *Model) driveOf(b *Block) *Drive {
	return m.Drives[strings.TrimPrefix(b.Drive, "/org/freedesktop/UDisks2/drives/")]
}

func firstOf(props map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := props[k]; v != "" {
			return v
		}
	}
	return ""
}

func driveFor(src Sources, b *Block, detected map[string]uint64, now uint64) *Drive {
	props := b.props
	if strings.HasPrefix(b.Name, "dm-") || strings.HasPrefix(b.Name, "loop") || strings.HasPrefix(b.Name, "zram") || strings.HasPrefix(b.Name, "md") || strings.HasPrefix(b.Name, "nbd") {
		return nil
	}
	dir := filepath.Join(src.Sys, "class/block", b.Name)
	d := &Drive{
		Model:               Decode(firstOf(props, "ID_MODEL_ENC", "ID_MODEL")),
		Vendor:              Decode(firstOf(props, "ID_VENDOR_ENC", "ID_VENDOR")),
		Revision:            props["ID_REVISION"],
		Serial:              firstOf(props, "ID_SERIAL_SHORT", "ID_SERIAL"),
		MediaAvailable:      true,
		MediaChangeDetected: true,
		MediaCompatibility:  []string{},
		Seat:                "seat0",
		block:               b.Name,
		nvme:                strings.HasPrefix(b.Name, "nvme"),
	}
	d.Model = strings.TrimSpace(strings.ReplaceAll(d.Model, "_", " "))
	d.Vendor = strings.TrimSpace(d.Vendor)
	if !d.nvme {
		d.WWN = firstOf(props, "ID_WWN_WITH_EXTENSION", "ID_WWN")
		d.Size = b.Size
	}
	d.MediaRemovable = readFile(filepath.Join(dir, "removable")) == "1"
	switch props["ID_BUS"] {
	case "usb":
		d.ConnectionBus = "usb"
	case "ieee1394":
		d.ConnectionBus = "ieee1394"
	}
	if strings.HasPrefix(b.Name, "mmcblk") {
		d.ConnectionBus = "sdio"
	}
	d.Removable = d.MediaRemovable || d.ConnectionBus == "usb" || d.ConnectionBus == "sdio" || d.ConnectionBus == "ieee1394"
	d.Ejectable = d.MediaRemovable && !strings.HasPrefix(b.Name, "mmcblk")
	d.CanPowerOff = d.ConnectionBus == "usb"
	if props["ID_CDROM"] == "1" {
		d.Optical = true
		d.Ejectable = true
	}
	switch {
	case readFile(filepath.Join(dir, "queue/rotational")) == "0":
		d.RotationRate = 0
	default:
		if v, err := strconv.Atoi(props["ID_ATA_ROTATION_RATE_RPM"]); err == nil {
			d.RotationRate = int32(v)
		} else {
			d.RotationRate = -1
		}
	}
	parts := []string{}
	for _, p := range []string{d.Vendor, d.Model, d.Serial} {
		if p != "" {
			parts = append(parts, strings.ReplaceAll(strings.TrimSpace(p), " ", "-"))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	d.ID = strings.Join(parts, "-")
	d.Object = driveObjectName(strings.ReplaceAll(strings.Join(parts, "_"), "-", "_"))
	key := d.Object
	if t, ok := detected[key]; ok {
		d.TimeDetected = t
	} else {
		d.TimeDetected = now
		if detected != nil {
			detected[key] = now
		}
	}
	d.TimeMediaDetected = d.TimeDetected
	sortName := b.Name
	if d.nvme {
		if real, err := filepath.EvalSymlinks(filepath.Join(dir, "device")); err == nil {
			sortName = filepath.Base(real)
		}
	}
	fixed := "00fixed"
	if d.Removable {
		fixed = "01removable"
	}
	d.SortKey = "00coldplug/" + fixed + "/" + sortName
	return d
}

func fmtScan(s string, major, minor *int) (int, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, nil
	}
	var err error
	if *major, err = strconv.Atoi(a); err != nil {
		return 0, err
	}
	if *minor, err = strconv.Atoi(b); err != nil {
		return 1, err
	}
	return 2, nil
}

func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

type mount struct {
	devnum, source, point string
}

func readMounts(path string) []mount {
	var out []mount
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, x := range fields {
			if x == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 5 || sep < 0 || sep+2 >= len(fields) {
			continue
		}
		out = append(out, mount{devnum: fields[2], source: unescapeMount(fields[sep+2]), point: unescapeMount(fields[4])})
	}
	return out
}

// mountsOf lists where b is mounted. A mount names its device by number,
// except on filesystems such as btrfs that report an anonymous number; then
// the mount source names it.
func mountsOf(b *Block, mounts []mount) []string {
	devnum := strconv.Itoa(b.major) + ":" + strconv.Itoa(b.minor)
	out := []string{}
	for _, m := range mounts {
		if (m.devnum == devnum || m.source == b.Device || m.source == b.PreferredDevice ||
			(b.props["DM_NAME"] != "" && m.source == "/dev/mapper/"+b.props["DM_NAME"]) || contains(b.Symlinks, m.source)) && !contains(out, m.point) {
			out = append(out, m.point)
		}
	}
	sort.Strings(out)
	return out
}

func readSwaps(path string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for i, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); i > 0 && len(f) > 0 {
			out[unescapeMount(f[0])] = true
		}
	}
	return out
}

type tabEntry struct {
	fields []string
}

func readTab(path string) []tabEntry {
	var out []tabEntry
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, tabEntry{fields: strings.Fields(line)})
	}
	return out
}

func (b *Block) matches(spec string) bool {
	switch {
	case strings.HasPrefix(spec, "UUID="):
		return b.IDUUID != "" && strings.EqualFold(strings.TrimPrefix(spec, "UUID="), b.IDUUID)
	case strings.HasPrefix(spec, "LABEL="):
		return b.IDLabel != "" && strings.TrimPrefix(spec, "LABEL=") == b.IDLabel
	case strings.HasPrefix(spec, "PARTUUID="):
		return b.props["ID_PART_ENTRY_UUID"] != "" && strings.EqualFold(strings.TrimPrefix(spec, "PARTUUID="), b.props["ID_PART_ENTRY_UUID"])
	case strings.HasPrefix(spec, "PARTLABEL="):
		return b.props["ID_PART_ENTRY_NAME"] != "" && strings.TrimPrefix(spec, "PARTLABEL=") == Decode(b.props["ID_PART_ENTRY_NAME"])
	}
	return spec == b.Device || contains(b.Symlinks, spec) || spec == b.PreferredDevice
}

func configFor(b *Block, fstab []tabEntry) []ConfigItem {
	out := []ConfigItem{}
	for _, e := range fstab {
		if len(e.fields) < 2 || !b.matches(e.fields[0]) {
			continue
		}
		get := func(i int, def string) string {
			if i < len(e.fields) {
				return e.fields[i]
			}
			return def
		}
		freq, _ := strconv.Atoi(get(4, "0"))
		passno, _ := strconv.Atoi(get(5, "0"))
		out = append(out, ConfigItem{Kind: "fstab", Fields: map[string]interface{}{
			"fsname": e.fields[0], "dir": unescapeMount(e.fields[1]), "type": get(2, "auto"),
			"opts": get(3, "defaults"), "freq": int32(freq), "passno": int32(passno),
		}})
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// HideSystemLoops marks loop devices that no user set up as ignored, so file
// managers do not offer system images, their verity hash trees, the device
// mapper targets stacked on them or their partitions as removable volumes.
func (m *Model) HideSystemLoops(owner func(name string) uint32) {
	hidden := map[string]bool{}
	for _, b := range m.Blocks {
		if b.Loop != nil && owner(b.Name) == 0 {
			hidden[b.Name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, b := range m.Blocks {
			if hidden[b.Name] {
				continue
			}
			stacked := len(b.slaves) > 0
			for _, sl := range b.slaves {
				stacked = stacked && hidden[sl]
			}
			if stacked || (b.disk != b.Name && hidden[b.disk]) {
				hidden[b.Name] = true
				changed = true
			}
		}
	}
	for name := range hidden {
		m.Blocks[name].HintIgnore = true
	}
}
