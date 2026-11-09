package health

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"sort"
	"syscall"
	"unsafe"
)

type Device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
}

const (
	SampleSize = 1 << 20
	Samples    = 96
	magic      = "SNTYCHK1"
)

type Verdict struct {
	Checked  int
	Failed   []uint64
	Aliased  bool
	RealSize uint64
}

func (v Verdict) Fake() bool {
	return v.Aliased || (v.RealSize > 0 && len(v.Failed) >= 2)
}

func (v Verdict) Damaged() bool { return len(v.Failed) > 0 && !v.Fake() }

func Positions(size uint64, n int) []uint64 {
	if size < SampleSize {
		return nil
	}
	last := (size - SampleSize) / SampleSize * SampleSize
	if n < 2 || last == 0 {
		return []uint64{0}
	}
	seen := map[uint64]bool{}
	var out []uint64
	for i := 0; i < n; i++ {
		p := last * uint64(i) / uint64(n-1) / SampleSize * SampleSize
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if !seen[last] {
		out = append(out, last)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func fill(buf []byte, nonce, off uint64) {
	copy(buf, magic)
	binary.LittleEndian.PutUint64(buf[8:], off)
	binary.LittleEndian.PutUint64(buf[16:], nonce)
	x := nonce ^ off ^ 0x9e3779b97f4a7c15
	for i := 24; i+8 <= len(buf); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(buf[i:], x)
	}
}

func alignedBuffer(n int) []byte {
	raw := make([]byte, n+4096)
	shift := (4096 - int(uintptr(unsafe.Pointer(&raw[0]))%4096)) % 4096
	return raw[shift : shift+n]
}

func Verify(dev Device, size uint64, n int, progress func(percent uint)) (Verdict, error) {
	pos := Positions(size, n)
	if len(pos) == 0 {
		return Verdict{}, errors.New("drive too small to check")
	}
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return Verdict{}, err
	}
	nonce := binary.LittleEndian.Uint64(nb[:])
	want := alignedBuffer(SampleSize)
	got := alignedBuffer(SampleSize)
	total := uint(2 * len(pos))
	step := uint(0)
	report := func() {
		step++
		if progress != nil {
			progress(step * 100 / total)
		}
	}
	var failed []uint64
	writeFailed := map[uint64]bool{}
	for _, p := range pos {
		fill(want, nonce, p)
		if _, err := dev.WriteAt(want, int64(p)); err != nil {
			if !errors.Is(err, syscall.EIO) && !errors.Is(err, syscall.ENOSPC) {
				return Verdict{}, err
			}
			writeFailed[p] = true
		}
		report()
	}
	if err := dev.Sync(); err != nil && !errors.Is(err, syscall.EIO) {
		return Verdict{}, err
	}
	if f, ok := dev.(*os.File); ok {
		dropCache(f)
	}
	v := Verdict{Checked: len(pos)}
	for _, p := range pos {
		fill(want, nonce, p)
		_, err := dev.ReadAt(got, int64(p))
		bad := writeFailed[p]
		if err != nil {
			if !errors.Is(err, syscall.EIO) {
				return Verdict{}, err
			}
			bad = true
		} else if !bytes.Equal(want, got) {
			bad = true
			if string(got[:8]) == magic && binary.LittleEndian.Uint64(got[16:]) == nonce &&
				binary.LittleEndian.Uint64(got[8:]) != p {
				v.Aliased = true
			}
		}
		if bad {
			failed = append(failed, p)
		}
		report()
	}
	v.Failed = failed
	if len(failed) > 0 {
		first := failed[0]
		contiguous := true
		idx := sort.Search(len(pos), func(i int) bool { return pos[i] >= first })
		if len(pos)-idx != len(failed) {
			contiguous = false
		}
		if contiguous && len(failed) >= 2 {
			v.RealSize = first
		}
		if v.Aliased && v.RealSize == 0 {
			v.RealSize = first
		}
	}
	return v, nil
}

func dropCache(f *os.File) {
	const blkflsbuf = 0x1261
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkflsbuf, 0)
}

func OpenDirect(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT|syscall.O_SYNC|syscall.O_EXCL|syscall.O_CLOEXEC, 0)
}
