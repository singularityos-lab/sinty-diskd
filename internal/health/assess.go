package health

const (
	Good    = "good"
	Watch   = "watch"
	Worn    = "worn"
	Failing = "failing"
)

var rank = map[string]int{"": 0, Good: 0, Watch: 1, Worn: 2, Failing: 3}

func Rank(state string) int { return rank[state] }

const day = 24 * 60 * 60

func Assess(r Record, now int64) (string, []string) {
	var reasons []string
	state := Good
	raise := func(s, reason string) {
		if rank[s] > rank[state] {
			state = s
		}
		for _, x := range reasons {
			if x == reason {
				return
			}
		}
		reasons = append(reasons, reason)
	}

	last := map[string]Event{}
	recent := map[string]int{}
	days := map[string]map[int64]bool{}
	for _, e := range r.Events {
		last[e.Kind] = e
		if now-e.Time <= day {
			recent[e.Kind]++
		}
		if now-e.Time <= 30*day {
			if days[e.Kind] == nil {
				days[e.Kind] = map[int64]bool{}
			}
			days[e.Kind][e.Time/day] = true
		}
	}

	if _, ok := last[FakeCapacity]; ok && !after(last, CapacityValid, FakeCapacity) {
		raise(Failing, FakeCapacity)
	}
	if _, ok := last[SmartFailing]; ok {
		raise(Failing, SmartFailing)
	}
	if _, ok := last[Unreadable]; ok && now-last[Unreadable].Time <= 30*day {
		raise(Failing, Unreadable)
	}
	if e, ok := last[ReadOnly]; ok && now-e.Time <= 30*day {
		raise(Failing, ReadOnly)
	}
	if _, ok := last[CheckFailed]; ok && !after(last, CheckPassed, CheckFailed) {
		raise(Failing, CheckFailed)
	}
	if _, ok := last[SmartWorn]; ok {
		raise(Worn, SmartWorn)
	}

	for _, kind := range []string{IOError, Timeout} {
		switch {
		case recent[kind] >= 5 || len(days[kind]) >= 2:
			raise(Worn, kind)
		case len(days[kind]) == 1:
			raise(Watch, kind)
		}
	}
	switch {
	case recent[Reset] >= 5 || len(days[Reset]) >= 3:
		raise(Worn, Reset)
	case len(days[Reset]) >= 1:
		raise(Watch, Reset)
	}
	switch {
	case len(days[SlowWrites]) >= 2:
		raise(Worn, SlowWrites)
	case len(days[SlowWrites]) == 1:
		raise(Watch, SlowWrites)
	}
	return state, reasons
}

func after(last map[string]Event, newer, older string) bool {
	n, ok := last[newer]
	return ok && n.Time >= last[older].Time
}

func ShouldAlert(state, alerted string) bool {
	return rank[state] >= rank[Worn] && rank[state] > rank[alerted]
}
