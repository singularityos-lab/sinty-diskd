package health

import (
	"regexp"
	"strings"
)

type Signal struct {
	Disk    string
	UsbPort string
	Kind    string
}

var (
	reIOError    = regexp.MustCompile(`(?:I/O error|critical medium error|critical target error), dev (\w+), sector`)
	reBufferIO   = regexp.MustCompile(`Buffer I/O error on dev(?:ice)? (\w+)`)
	reSdTag      = regexp.MustCompile(`^sd \S+ \[(\w+)\] (.*)$`)
	reUsbReset   = regexp.MustCompile(`^usb (\d+-[\d.]+): reset `)
	reFsReadOnly = regexp.MustCompile(`^(?:EXT[234]-fs|FAT-fs|exFAT-fs|F2FS-fs|NTFS-fs|ntfs3|XFS|BTRFS)(?: error)? \(?(?:device )?(\w+)\)?:? .*(?:Remounting filesystem read-only|Filesystem has been set read-only|switching fs to read-only|remounting as readonly|forced readonly)`)
)

func Disk(name string) string {
	switch {
	case strings.HasPrefix(name, "nvme"), strings.HasPrefix(name, "mmcblk"), strings.HasPrefix(name, "loop"):
		if i := strings.LastIndex(name, "p"); i > 0 && i < len(name)-1 && allDigits(name[i+1:]) && !strings.HasSuffix(name[:i], "n") {
			return name[:i]
		}
		return name
	}
	return strings.TrimRight(name, "0123456789")
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func KmsgText(record string) string {
	if i := strings.IndexByte(record, ';'); i >= 0 {
		record = record[i+1:]
	}
	if i := strings.IndexByte(record, '\n'); i >= 0 {
		record = record[:i]
	}
	return strings.TrimSpace(record)
}

func ParseKernel(line string) (Signal, bool) {
	if m := reUsbReset.FindStringSubmatch(line); m != nil {
		return Signal{UsbPort: m[1], Kind: Reset}, true
	}
	if m := reFsReadOnly.FindStringSubmatch(line); m != nil {
		return Signal{Disk: Disk(m[1]), Kind: ReadOnly}, true
	}
	if m := reSdTag.FindStringSubmatch(line); m != nil {
		rest := m[2]
		switch {
		case strings.Contains(rest, "Medium Error"), strings.Contains(rest, "Unrecovered read error"):
			return Signal{Disk: m[1], Kind: Unreadable}, true
		case strings.Contains(rest, "timing out command"):
			return Signal{Disk: m[1], Kind: Timeout}, true
		}
		return Signal{}, false
	}
	if m := reIOError.FindStringSubmatch(line); m != nil {
		if strings.Contains(line, "critical medium error") {
			return Signal{Disk: Disk(m[1]), Kind: Unreadable}, true
		}
		return Signal{Disk: Disk(m[1]), Kind: IOError}, true
	}
	if m := reBufferIO.FindStringSubmatch(line); m != nil {
		return Signal{Disk: Disk(m[1]), Kind: IOError}, true
	}
	return Signal{}, false
}
