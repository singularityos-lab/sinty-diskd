package health

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Identity struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	Size   uint64 `json:"size"`
}

func (id Identity) Key() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(id.Vendor), strings.TrimSpace(id.Model),
		strings.TrimSpace(id.Serial), strconv.FormatUint(id.Size, 10)}, "\x00")))
	return hex.EncodeToString(h[:8])
}

func (id Identity) Name() string {
	return strings.TrimSpace(strings.TrimSpace(id.Vendor) + " " + strings.TrimSpace(id.Model))
}

const (
	IOError       = "io-error"
	Reset         = "reset"
	Timeout       = "timeout"
	Unreadable    = "unreadable"
	ReadOnly      = "read-only"
	SlowWrites    = "slow-writes"
	SmartFailing  = "smart-failing"
	SmartWorn     = "smart-worn"
	FakeCapacity  = "fake-capacity"
	CheckFailed   = "check-failed"
	CheckPassed   = "check-passed"
	CapacityValid = "capacity-verified"
)

type Event struct {
	Kind   string `json:"kind"`
	Time   int64  `json:"time"`
	Detail string `json:"detail,omitempty"`
}

type Record struct {
	Identity
	FirstSeen  int64   `json:"first_seen"`
	LastSeen   int64   `json:"last_seen"`
	Events     []Event `json:"events"`
	Alerted    string  `json:"alerted,omitempty"`
	RealSize   uint64  `json:"real_size,omitempty"`
	WriteSpeed float64 `json:"write_speed,omitempty"`
}

const maxEvents = 200

type Book struct {
	mu      sync.Mutex
	path    string
	records map[string]*Record
	now     func() time.Time
}

func Open(path string) *Book {
	b := &Book{path: path, records: map[string]*Record{}, now: time.Now}
	data, err := os.ReadFile(path)
	if err == nil {
		var list []*Record
		if json.Unmarshal(data, &list) == nil {
			for _, r := range list {
				b.records[r.Key()] = r
			}
		}
	}
	return b
}

func (b *Book) SetClock(now func() time.Time) { b.now = now }

func (b *Book) Seen(id Identity) *Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().Unix()
	r := b.records[id.Key()]
	if r == nil {
		r = &Record{Identity: id, FirstSeen: now}
		b.records[id.Key()] = r
	}
	r.LastSeen = now
	return r
}

func (b *Book) Note(key, kind, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.records[key]
	if r == nil {
		return
	}
	r.Events = append(r.Events, Event{Kind: kind, Time: b.now().Unix(), Detail: detail})
	if len(r.Events) > maxEvents {
		r.Events = r.Events[len(r.Events)-maxEvents:]
	}
}

func (b *Book) Update(key string, fn func(r *Record)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.records[key]; r != nil {
		fn(r)
	}
}

func (b *Book) Get(key string) (Record, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.records[key]
	if r == nil {
		return Record{}, false
	}
	c := *r
	c.Events = append([]Event(nil), r.Events...)
	return c, true
}

func (b *Book) Keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.records))
	for k := range b.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (b *Book) Save() error {
	b.mu.Lock()
	list := make([]*Record, 0, len(b.records))
	for _, r := range b.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].FirstSeen < list[j].FirstSeen })
	data, err := json.MarshalIndent(list, "", " ")
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

func (b *Book) Now() time.Time { return b.now() }
