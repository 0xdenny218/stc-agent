//go:build wasm

// dice 是 stc-agent 的示例 guest 工具：掷一颗骰子，无网络依赖。
// 构建（在仓库根目录）：
//
//	tinygo build -target wasip1 -buildmode=c-shared -o tools.d/dice.wasm ./examples/guests/dice
//
// version 区分在役版本：热替换演示与测试用 -tags v2 构建换版
// （见 version_v2.go；TinyGo 下 -ldflags -X 对此不生效）。
package main

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/0xdenny218/stc-go/guest"
)

var version = "v1"

const sides = 6

// 工具描述经 tool.dice 服务公布（服务键与 .wasm 文件名对应）；
// stc_alloc/stc_free/invoke 由 guest SDK 导出，OnInvoke 即全部接线。
func init() {
	guest.OnInvoke(func(args string) string {
		n, err := rand.Int(rand.Reader, big.NewInt(sides))
		if err != nil {
			return fmt.Sprintf(`{"error":%q,"version":%q}`, err.Error(), version)
		}
		return fmt.Sprintf(`{"roll":%d,"sides":%d,"version":%q}`, n.Int64()+1, sides, version)
	})
}

//export start
func start() {
	_ = guest.Provide("tool.dice", `{"name":"dice","description":"roll a six-sided die","parameters":{"type":"object","properties":{}}}`)
}

func main() {} // reactor 模式：入口是 start/invoke
