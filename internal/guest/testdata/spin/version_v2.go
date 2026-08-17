//go:build wasm && v2

package main

// -tags v2：热替换后的"下一版"（release 已提供，返回即新版行为）。
func init() { version = "v2" }
