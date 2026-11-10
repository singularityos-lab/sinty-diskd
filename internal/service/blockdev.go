package service

import (
	"os"
	"strings"

	core "github.com/singularityos-lab/libsingularity/src/system/core/go/sintysystem"
)

func announce(name string) {
	core.BlockUevent(name, "change")
	entries, _ := os.ReadDir("/sys/class/block/" + name)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), name) {
			core.BlockUevent(e.Name(), "change")
		}
	}
	core.SettleBlock(name, 10000)
}
