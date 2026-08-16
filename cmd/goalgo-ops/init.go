package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opsinstall"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

func cmdInit(args []string, runner opsexec.Command) int {
	var rootPath string
	var printTemplate bool
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.BoolVar(&printTemplate, "print", false, "仅打印带注释的 .env 模板，不写入")
	if err := flags.Parse(args); err != nil {
		return fail("init", err)
	}
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("init", err)
	}
	if printTemplate {
		fmt.Print(opsinstall.ReadTemplateEnv(root.Path))
		return 0
	}
	prompt := opsprompt.New()
	if !prompt.TTY {
		return fail("init", opsprompt.ErrNonInteractive)
	}
	path := root.Join(".env")
	if _, err := os.Stat(path); err == nil {
		keep, err := prompt.Confirm("检测到已有 .env，保留现有配置？", true)
		if err != nil {
			return fail("init", err)
		}
		if keep {
			fmt.Printf("已保留 %s\n", path)
			return 0
		}
	}
	order := []string{"GOALGO_HTTP_BIND", "GOALGO_HTTP_PORT", "TZ", "COMPOSE_PROJECT_NAME", "GOALGO_ROOT", "POSTGRES_USER", "RABBITMQ_USER"}
	defaults := map[string]string{
		"COMPOSE_PROJECT_NAME": "goalgo",
		"GOALGO_HTTP_BIND":     "0.0.0.0",
		"GOALGO_HTTP_PORT":     "8988",
		"GOALGO_ROOT":          root.Path,
		"TZ":                   "Asia/Shanghai",
		"POSTGRES_USER":        "goalgo",
		"RABBITMQ_USER":        "goalgo",
	}
	values := map[string]string{}
	for _, key := range order {
		got, err := prompt.String(envLabel(key), defaults[key])
		if err != nil {
			return fail("init", err)
		}
		if strings.TrimSpace(got) == "" {
			got = defaults[key]
		}
		values[key] = got
	}
	var builder strings.Builder
	builder.WriteString("COMPOSE_PROJECT_NAME=" + values["COMPOSE_PROJECT_NAME"] + "\n")
	builder.WriteString("GOALGO_HTTP_BIND=" + values["GOALGO_HTTP_BIND"] + "\n")
	builder.WriteString("GOALGO_HTTP_PORT=" + values["GOALGO_HTTP_PORT"] + "\n")
	builder.WriteString("GOALGO_ROOT=" + values["GOALGO_ROOT"] + "\n")
	builder.WriteString("TZ=" + values["TZ"] + "\n")
	builder.WriteString("\n# 运行时凭据保存在 secrets/，不在本文件填写。\n")
	builder.WriteString("POSTGRES_USER=" + values["POSTGRES_USER"] + "\n")
	builder.WriteString("RABBITMQ_USER=" + values["RABBITMQ_USER"] + "\n")
	if err := os.MkdirAll(root.Path, 0o755); err != nil {
		return fail("init", err)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fail("init", err)
	}
	fmt.Printf("已写入 %s（0600）。现在可执行：goalgo-ops install\n", path)
	return 0
}

func envLabel(key string) string {
	labels := map[string]string{
		"COMPOSE_PROJECT_NAME": "Compose 项目名",
		"GOALGO_HTTP_BIND":     "HTTP 绑定地址",
		"GOALGO_HTTP_PORT":     "HTTP 端口",
		"GOALGO_ROOT":          "GoAlgo 根目录",
		"TZ":                   "时区",
		"POSTGRES_USER":        "PostgreSQL 用户",
		"RABBITMQ_USER":        "RabbitMQ 用户",
	}
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}
