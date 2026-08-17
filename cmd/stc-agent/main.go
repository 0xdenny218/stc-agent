// Command stc-agent is a minimal CLI chat agent where every capability is a
// fiber, built on stc-go. M0 scaffold: root context plus an empty component
// list — capabilities land from M1 on.
package main

import (
	"fmt"

	stc "github.com/0xdenny218/stc-go"
)

// components is the assembly list: every capability is one fiber
// (config/model/session/cli, then tools/loop, then WASM guests with hot
// reload). M0 loads nothing.
func components() []stc.Component {
	return nil
}

func main() {
	root := stc.New()
	defer root.Close()

	for _, c := range components() {
		root.Load(c)
	}

	fmt.Println("stc-agent: M0 scaffold — no capabilities loaded yet")
}
