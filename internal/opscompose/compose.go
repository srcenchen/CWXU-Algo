package opscompose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/internal/opsexec"
	"cwxu-algo/internal/opsrelease"
	"cwxu-algo/internal/opsroot"
)

type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

type Compose struct {
	Root       *opsroot.Root
	Run        Runner
	SmokeCheck func(context.Context) error
}

// runner 返回用于执行 docker 命令的 Runner。对真实的 opsexec.Real，
// 会把 .env 与 release.env 里的变量注入子进程环境，避免 shell 里残留的
// GOALGO_ROOT 等空值/旧值覆盖 docker compose 的 --env-file 插值。
func (c *Compose) runner() Runner {
	if real, ok := c.Run.(opsexec.Real); ok {
		real.Env = c.envOverlay()
		return real
	}
	return c.Run
}

// envOverlay 收集 .env 与 release.env 中的键值对作为子进程环境覆盖层。
func (c *Compose) envOverlay() map[string]string {
	overlay := map[string]string{}
	for _, file := range []string{".env", "release.env"} {
		content, err := readFile(c.Root.Join(file))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if ok {
				overlay[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return overlay
}

func (c *Compose) baseArgs() []string {
	return []string{
		"compose",
		"--env-file", c.Root.Join(".env"),
		"--env-file", c.Root.Join("release.env"),
		"-f", c.Root.Join("compose.yaml"),
	}
}

func (c *Compose) Command(ctx context.Context, args ...string) (string, error) {
	if err := c.ValidateRoot(); err != nil {
		return "", err
	}
	full := append(c.baseArgs(), args...)
	return c.runner().CombinedOutput(ctx, "docker", full...)
}

// CommandWithStdin 运行命令并把 data 作为 stdin 传入（用于 psql/pg_restore 管道）。
func (c *Compose) CommandWithStdin(ctx context.Context, data []byte, args ...string) (string, error) {
	if err := c.ValidateRoot(); err != nil {
		return "", err
	}
	full := append(c.baseArgs(), args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := c.runner().Run(ctx, bytes.NewReader(data), &stdout, &stderr, "docker", full...)
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}
	return output, err
}

func (c *Compose) Up(ctx context.Context, timeout string) error {
	if err := c.ValidateRoot(); err != nil {
		return err
	}
	err := c.runner().Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), "up", "-d", "--wait", "--wait-timeout", timeout)...)
	if err != nil {
		return fmt.Errorf("compose up：%w", err)
	}
	return nil
}

func (c *Compose) Pull(ctx context.Context) error {
	if err := c.ValidateRoot(); err != nil {
		return err
	}
	err := c.runner().Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), "pull")...)
	if err != nil {
		return fmt.Errorf("compose pull：%w", err)
	}
	return nil
}

func (c *Compose) Restart(ctx context.Context) error {
	output, err := c.Command(ctx, "restart")
	if err != nil {
		return fmt.Errorf("compose restart：%w\n%s", err, strings.TrimSpace(output))
	}
	return nil
}

func (c *Compose) Stop(ctx context.Context) error {
	output, err := c.Command(ctx, "stop")
	if err != nil {
		return fmt.Errorf("compose stop：%w\n%s", err, strings.TrimSpace(output))
	}
	return nil
}

func (c *Compose) PSRunning(ctx context.Context) error {
	output, err := c.Command(ctx, "ps", "--format", "json")
	if err != nil {
		return fmt.Errorf("compose ps：%w\n%s", err, strings.TrimSpace(output))
	}
	type serviceState struct {
		Service string `json:"Service"`
		State   string `json:"State"`
		Health  string `json:"Health"`
	}
	var states []serviceState
	trimmed := strings.TrimSpace(output)
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &states); err != nil {
			return fmt.Errorf("解析 compose ps：%w", err)
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			var state serviceState
			if err := json.Unmarshal([]byte(line), &state); err != nil {
				return fmt.Errorf("解析 compose ps：%w", err)
			}
			states = append(states, state)
		}
	}
	byService := make(map[string]serviceState, len(states))
	for _, state := range states {
		byService[state.Service] = state
	}
	for _, service := range []string{"frontend", "gateway", "user", "core-data", "agent", "postgres", "redis", "rabbitmq", "consul", "nginx"} {
		state, ok := byService[service]
		if !ok {
			return fmt.Errorf("预期服务未运行：%s", service)
		}
		if state.State != "running" || (state.Health != "" && state.Health != "healthy") {
			return fmt.Errorf("服务未健康：%s（state=%s health=%s）", service, state.State, state.Health)
		}
	}
	return nil
}

func (c *Compose) Logs(ctx context.Context, args ...string) error {
	full := append([]string{"logs"}, args...)
	if err := c.ValidateRoot(); err != nil {
		return err
	}
	err := c.runner().Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), full...)...)
	if err != nil {
		return fmt.Errorf("compose logs：%w", err)
	}
	return nil
}

func (c *Compose) ValidateRoot() error {
	configured := c.rootEnv("GOALGO_ROOT")
	if configured == "" {
		return fmt.Errorf(".env 缺少 GOALGO_ROOT")
	}
	resolved, err := opsroot.Resolve(configured)
	if err != nil {
		return fmt.Errorf(".env GOALGO_ROOT：%w", err)
	}
	if resolved.Path != c.Root.Path {
		return fmt.Errorf(".env GOALGO_ROOT=%s 与选中 root=%s 不一致", resolved.Path, c.Root.Path)
	}
	return nil
}

func (c *Compose) Version(ctx context.Context) error {
	_, err := c.runner().CombinedOutput(ctx, "docker", "compose", "version")
	return err
}

func (c *Compose) Config(ctx context.Context) error {
	output, err := c.Command(ctx, "config", "--quiet")
	if err != nil {
		return fmt.Errorf("compose config：%w\n%s", err, strings.TrimSpace(output))
	}
	return nil
}

// RunService 在指定服务内运行一次性命令（docker compose run --rm -T，自动带起依赖）。
// runOptions 会原样拼在 service 之前，entrypoint 覆盖与用户等选项可由此传入。
// 使用流式输出，避免拉取依赖镜像等耗时操作期间无回显。
func (c *Compose) RunService(ctx context.Context, service string, runOptions []string, args ...string) (string, error) {
	full := []string{"run", "--rm", "-T"}
	full = append(full, runOptions...)
	full = append(full, service)
	full = append(full, args...)
	err := c.runner().Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), full...)...)
	if err != nil {
		return "", fmt.Errorf("compose run：%w", err)
	}
	return "", nil
}

func (c *Compose) Health(ctx context.Context) error {
	return c.PSRunning(ctx)
}

func (c *Compose) Smoke(ctx context.Context) error {
	if c.SmokeCheck != nil {
		return c.SmokeCheck(ctx)
	}
	base := c.httpBase()
	client := &http.Client{Timeout: 15 * time.Second}
	for _, path := range []string{"/healthz", "/api/user/site/config"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("冒烟 %s：%w", path, err)
		}
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("冒烟 %s：意外状态码 %d", path, response.StatusCode)
		}
	}
	return nil
}

func (c *Compose) httpBase() string {
	bind := c.rootEnv("GOALGO_HTTP_BIND")
	port := c.rootEnv("GOALGO_HTTP_PORT")
	if port == "" {
		port = "8988"
	}
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		bind = "127.0.0.1"
	}
	return "http://" + bind + ":" + port
}

func (c *Compose) rootEnv(key string) string {
	path := c.Root.Join(".env")
	content, err := readFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			return strings.TrimPrefix(trimmed, key+"=")
		}
	}
	return ""
}

func (c *Compose) WaitTimeout() string {
	value := c.rootEnv("GOALGO_WAIT_TIMEOUT")
	if value == "" {
		return "300"
	}
	if _, err := strconv.Atoi(value); err != nil {
		return "300"
	}
	return value
}

func (c *Compose) Release() (*opsrelease.Release, error) {
	return opsrelease.ParseFile(c.Root.Join("release.env"))
}

// latestServices 与 ACR 镜像标签的 <service>-latest 约定一一对应。
var latestServices = []string{"frontend", "gateway", "user", "core-data", "agent"}

var digestInOutput = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// ResolveLatest 解析 ACR 上各服务的 <service>-latest 标签，解析为不可变 digest 形式的 Release。
// 生产机不直接引用可变标签，而是先把 latest 解析成 digest 后写入 release.env。
func (c *Compose) ResolveLatest(ctx context.Context) (*opsrelease.Release, error) {
	release := &opsrelease.Release{Images: map[string]string{}}
	for _, service := range latestServices {
		ref := opsrelease.Repository + ":" + service + "-latest"
		output, err := c.runner().CombinedOutput(ctx, "docker", "manifest", "inspect", "--verbose", ref)
		if err != nil {
			return nil, fmt.Errorf("解析 %s：%w", ref, err)
		}
		match := digestInOutput.FindString(output)
		if match == "" {
			return nil, fmt.Errorf("解析 %s：未找到 digest", ref)
		}
		key := strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_IMAGE"
		release.Images[key] = opsrelease.Repository + "@" + match
	}
	return release, nil
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
