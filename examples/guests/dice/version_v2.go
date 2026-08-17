//go:build wasm && v2

package main

// -tags v2 构建热替换演示的"下一版"：同名工具、不同行为。
// init 在 start 之前运行，调用期读取的 version 即 v2。
func init() { version = "v2" }
