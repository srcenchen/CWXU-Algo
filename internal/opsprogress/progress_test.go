package opsprogress

import (
	"bytes"
	"strings"
	"testing"
)

func TestStepAndSubOutput(t *testing.T) {
	var out bytes.Buffer
	p := New(5, &out)
	p.Step("初始化目录")
	p.Sub("拉取发布镜像")
	if !strings.Contains(out.String(), "[1/5] 初始化目录") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	if !strings.Contains(out.String(), "  - 拉取发布镜像") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestDoneOutput(t *testing.T) {
	var out bytes.Buffer
	Done(&out, "安装完成")
	if !strings.Contains(out.String(), "安装完成") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestNoteOutput(t *testing.T) {
	var out bytes.Buffer
	Note(&out, "拉取发布镜像")
	if !strings.Contains(out.String(), "  - 拉取发布镜像") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}
