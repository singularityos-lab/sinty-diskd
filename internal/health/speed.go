package health

import (
	"strconv"
	"strings"
)

type IOStat struct {
	WriteSectors uint64
	WriteTicks   uint64
}

func ParseStat(text string) (IOStat, bool) {
	f := strings.Fields(text)
	if len(f) < 8 {
		return IOStat{}, false
	}
	sectors, err1 := strconv.ParseUint(f[6], 10, 64)
	ticks, err2 := strconv.ParseUint(f[7], 10, 64)
	if err1 != nil || err2 != nil {
		return IOStat{}, false
	}
	return IOStat{WriteSectors: sectors, WriteTicks: ticks}, true
}

const (
	minSampleBytes = 64 << 20
	baselineDepth  = 3
	collapseRatio  = 5
	collapseRun    = 3
)

type Speed struct {
	last     IOStat
	primed   bool
	samples  int
	Baseline float64
	slow     int
}

func (s *Speed) Observe(st IOStat) (rate float64, collapsed bool) {
	if !s.primed || st.WriteSectors < s.last.WriteSectors || st.WriteTicks < s.last.WriteTicks {
		s.last, s.primed = st, true
		return 0, false
	}
	bytes := (st.WriteSectors - s.last.WriteSectors) * 512
	ticks := st.WriteTicks - s.last.WriteTicks
	if bytes < minSampleBytes || ticks == 0 {
		return 0, false
	}
	s.last = st
	rate = float64(bytes) / (float64(ticks) / 1000)
	if s.samples < baselineDepth {
		s.samples++
		if rate > s.Baseline {
			s.Baseline = rate
		}
		return rate, false
	}
	if rate*collapseRatio < s.Baseline {
		s.slow++
		if s.slow == collapseRun {
			return rate, true
		}
		return rate, false
	}
	s.slow = 0
	s.Baseline = s.Baseline*0.9 + rate*0.1
	return rate, false
}
