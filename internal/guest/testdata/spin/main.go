//go:build wasm

// spin 是测试专用 guest：invoke 自旋直到宿主提供字符串服务
// "release"，然后返回带版本号的结果。用于 Contract/UpdateWaitsInflight
// （进行中的调用在旧版本上完整跑完、热替换等待）。
package main

import "github.com/0xdenny218/stc-go/guest"

var version = "v1"

func init() {
	guest.OnInvoke(func(args string) string {
		guest.Log("spinning")
		for {
			if _, ok := guest.Get("release"); ok {
				return `{"version":"` + version + `"}`
			}
		}
	})
}

//export start
func start() {
	_ = guest.Provide("tool.spin", `{"name":"spin","description":"blocks until the host provides the release service","parameters":{"type":"object","properties":{}}}`)
}

func main() {}
