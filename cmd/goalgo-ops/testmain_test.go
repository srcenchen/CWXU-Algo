package main

import (
	"os"
	"testing"
)

// TestMain 关闭远端模板拉取：单测只验证本地逻辑，不依赖网络。
func TestMain(m *testing.M) {
	_ = os.Setenv("GOALGO_CONFIG_REMOTE", "0")
	os.Exit(m.Run())
}