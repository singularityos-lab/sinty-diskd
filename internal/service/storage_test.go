package service

import (
	"testing"

	"github.com/singularityos-lab/libsingularity/src/system/core/go/orgpolicy"
)

func TestStorageRuleOnlyTouchesRemovableDrives(t *testing.T) {
	cases := []struct {
		removable      bool
		rule           orgpolicy.Storage
		deny, readOnly bool
	}{
		{true, orgpolicy.StorageAllow, false, false},
		{true, orgpolicy.StorageReadOnly, false, true},
		{true, orgpolicy.StorageDeny, true, false},
		{false, orgpolicy.StorageDeny, false, false},
		{false, orgpolicy.StorageReadOnly, false, false},
		{true, "", false, false},
	}
	for _, c := range cases {
		deny, ro := storageRule(c.removable, c.rule)
		if deny != c.deny || ro != c.readOnly {
			t.Errorf("removable=%v rule=%q: deny=%v ro=%v", c.removable, c.rule, deny, ro)
		}
	}
}
