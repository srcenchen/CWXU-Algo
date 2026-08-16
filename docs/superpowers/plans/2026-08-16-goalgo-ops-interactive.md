# goalgo-ops 交互式简化 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 新增 `goalgo-ops init`，把 `install` 改造成带 5 步进度 + 管理员并入 + 镜像拉取/容器创建子步骤提示，并让全部命令「缺参自动交互」，同时把 compose 输出改为流式避免「像阻塞、Ctrl+C 才出日志」。

**架构：** 新增 `internal/opsprompt`（TTY 输入原语）、`internal/opsprogress`（分步进度输出）；把 `admin.go` 的交互采集下沉到 `internal/opsadmin`；`opsinstall` 拆分为 `Scaffold`/`Secrets` 两步并在 `state/install.json` 增加 `adminCreated` 标记；`opscompose.Pull`/`Up` 改为流式输出；命令层缺必填项且 TTY 时进入交互提示。

**技术栈：** Go（stdlib + `golang.org/x/term`，已有依赖）、Python unittest（部署契约测试）。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/opsprompt/prompt.go` + `_test.go` | TTY 判断与 String/Password/Confirm/Choice 原语 |
| `internal/opsprogress/progress.go` + `_test.go` | `[n/total]` 步骤 / 子步骤 / 成功摘要输出 |
| `internal/opsinstall/install.go` + `_test.go` | 拆 `Scaffold`/`Secrets`；`adminCreated` 标记读写 |
| `internal/opsadmin/admin.go` + `_test.go` | 管理员引导：交互采集 + 校验 + compose run + 标记 |
| `internal/opsexec/command.go` | 无改动（`Real.Run` 已支持流式） |
| `internal/opscompose/compose.go` + `_test.go` | `Pull`/`Up` 改流式；调用点进度由命令层负责 |
| `cmd/goalgo-ops/init.go` | 新命令 `init`（交互 + `--print` 模板） |
| `cmd/goalgo-ops/main.go` | 注册 `init`；usage；install 5 步进度 + 管理员并入 |
| `cmd/goalgo-ops/admin.go` | 改为调用 `opsadmin.Bootstrap`，删除重复代码 |
| `cmd/goalgo-ops/runtime.go`（新增） | deploy/rollback/restart 中 Pull/Up 前子步骤提示 + 候选文件交互 |
| `cmd/goalgo-ops/restore.go` | 缺参交互补全（归档/密钥/确认） |
| `cmd/goalgo-ops/backup.go` | `backup download`/`verify` 缺参交互 |
| `cmd/goalgo-ops/config.go` | `config export`/`import` 路径交互 |
| `deploy/tests/test_operations.py` | 契约断言：usage 含 `init`、install 帮助、非交互提示 |

---

## 任务 1：`internal/opsprompt`

**文件：** 创建 `internal/opsprompt/prompt.go`、`internal/opsprompt/prompt_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
package opsprompt

import (
	"strings"
	"testing"
)

func TestNonInteractiveReturnsError(t *testing.T) {
	p := New()
	p.TTY = false
	if _, err := p.String("a", "b"); err != ErrNonInteractive {
		t.Fatalf("expected ErrNonInteractive, got %v", err)
	}
}

func TestStringUsesDefaultOnEnter(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("\n"))
	got, err := p.String("name", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "default" {
		t.Fatalf("got %q, want default", got)
	}
}

func TestChoiceReturnsIndex(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("2\n"))
	idx, err := p.Choice("pick", 0, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("got %d, want 1", idx)
	}
}

func TestConfirmFalseOnNo(t *testing.T) {
	p := New()
	p.TTY = true
	p.In = bufio.NewReader(strings.NewReader("n\n"))
	if ok, err := p.Confirm("proceed?", false); err != nil || ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd cwxu-algo && go test ./internal/opsprompt/ -v`
预期：FAIL，`undefined: New` / 包不存在

- [ ] **步骤 3：编写实现**

```go
package opsprompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

var ErrNonInteractive = errors.New("非交互式终端：请提供参数或环境变量")

type Prompter struct {
	In  *bufio.Reader
	Out io.Writer
	TTY bool
}

func New() *Prompter {
	return &Prompter{
		In:  bufio.NewReader(os.Stdin),
		Out: os.Stdout,
		TTY: term.IsTerminal(int(os.Stdin.Fd())),
	}
}

func (p *Prompter) String(prompt, def string) (string, error) {
	if !p.TTY {
		return "", ErrNonInteractive
	}
	fmt.Fprintf(p.Out, "%s [%s]: ", prompt, def)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *Prompter) Password(prompt string) (string, error) {
	if !p.TTY {
		return "", ErrNonInteractive
	}
	fmt.Fprint(p.Out, prompt+": ")
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.Out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (p *Prompter) Confirm(prompt string, def bool) (bool, error) {
	if !p.TTY {
		return false, ErrNonInteractive
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(p.Out, "%s (%s): ", prompt, hint)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return def, nil
}

func (p *Prompter) Choice(prompt string, def int, options ...string) (int, error) {
	if !p.TTY {
		return -1, ErrNonInteractive
	}
	for i, opt := range options {
		fmt.Fprintf(p.Out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprintf(p.Out, "%s [%d]: ", prompt, def+1)
	line, err := p.In.ReadString('\n')
	if err != nil && err != io.EOF {
		return -1, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(options) {
		return def, nil
	}
	return n - 1, nil
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./internal/opsprompt/ -v`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add internal/opsprompt && git commit -m "feat(ops): add opsprompt interactive primitives"
```

---

## 任务 2：`internal/opsprogress`

**文件：** 创建 `internal/opsprogress/progress.go`、`internal/opsprogress/progress_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
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
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./internal/opsprogress/ -v`
预期：FAIL，`undefined: New`

- [ ] **步骤 3：编写实现**

```go
package opsprogress

import (
	"fmt"
	"io"
)

type Progress struct {
	total   int
	current int
	out     io.Writer
}

func New(total int, out io.Writer) *Progress {
	return &Progress{total: total, out: out}
}

func (p *Progress) Step(message string) {
	p.current++
	fmt.Fprintf(p.out, "[%d/%d] %s\n", p.current, p.total, message)
}

func (p *Progress) Sub(message string) {
	fmt.Fprintf(p.out, "  - %s\n", message)
}

func (p *Progress) Message(message string) {
	fmt.Fprintf(p.out, "%s\n", message)
}

func Done(out io.Writer, message string) {
	fmt.Fprintf(out, "%s\n", message)
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./internal/opsprogress/ -v`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add internal/opsprogress && git commit -m "feat(ops): add opsprogress step output"
```

---

## 任务 3：`opsinstall` 拆分与 `adminCreated` 标记

**文件：** 修改 `internal/opsinstall/install.go`、`internal/opsinstall/install_test.go`

- [ ] **步骤 1：编写失败的测试（追加到 install_test.go）**

```go
func TestAdminCreatedMarker(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if AdminCreated(root) {
		t.Fatal("fresh install must not be admin-created")
	}
	if err := MarkAdminCreated(root); err != nil {
		t.Fatal(err)
	}
	if !AdminCreated(root) {
		t.Fatal("marker must be readable after set")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./internal/opsinstall/ -run TestAdminCreatedMarker -v`
预期：FAIL，`undefined: AdminCreated`

- [ ] **步骤 3：修改 install.go**

拆 `Install` 为 `Scaffold`（目录 + managed 资产 + `.env`）与 `Secrets`（密钥 + JWT + `app.env` + ownership + marker），`Install` = 两者；marker struct 增加 `AdminCreated`：

```go
func (i *Installer) Install(ctx context.Context) error {
	if err := i.Scaffold(); err != nil {
		return err
	}
	return i.Secrets()
}

func (i *Installer) Scaffold() error {
	if err := i.Root.EnsureLayout(); err != nil {
		return err
	}
	for _, managed := range managedAssets() {
		if err := writeManaged(i.Root.Path, managed); err != nil {
			return err
		}
	}
	return writeEnvIfMissing(i.Root.Path)
}

func (i *Installer) Secrets() error {
	for _, secret := range []string{"postgres_password", "redis_password", "rabbitmq_password", "config_encryption_key"} {
		if err := writeHexSecret(i.Root.Path, "secrets/"+secret, 32); err != nil {
			return err
		}
	}
	if err := writeSecret(i.Root.Path, "secrets/backup_encryption_key", 32); err != nil {
		return err
	}
	if err := generateRSAKeyPair(i.Root.Path, 3072); err != nil {
		return err
	}
	if err := writeAppEnv(i.Root.Path); err != nil {
		return err
	}
	if err := applyOwnership(i.Root.Path); err != nil {
		return err
	}
	return writeInstallMarker(i.Root)
}
```

marker 与读写函数：

```go
type installMarker struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstalledAt   string `json:"installedAt"`
	AdminCreated  bool   `json:"adminCreated"`
}

func writeInstallMarker(root *opsroot.Root) error {
	marker := installMarker{
		SchemaVersion: 1,
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
		AdminCreated:  false,
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(root.Join("state", "install.json"), data, 0o644)
}

func AdminCreated(root *opsroot.Root) bool {
	data, err := os.ReadFile(root.Join("state", "install.json"))
	if err != nil {
		return false
	}
	var marker installMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}
	return marker.AdminCreated
}

func MarkAdminCreated(root *opsroot.Root) error {
	path := root.Join("state", "install.json")
	marker := installMarker{SchemaVersion: 1, AdminCreated: true}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &marker)
	}
	marker.AdminCreated = true
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o644)
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./internal/opsinstall/ -v`
预期：PASS（含既有 `TestInstallProvisionsFullLayout` 等，仍走 `Install`）

- [ ] **步骤 5：Commit**

```bash
git add internal/opsinstall && git commit -m "feat(ops): split scaffold/secrets and track adminCreated marker"
```

---

## 任务 4：`internal/opsadmin`

**文件：** 创建 `internal/opsadmin/admin.go`、`internal/opsadmin/admin_test.go`；修改 `cmd/goalgo-ops/admin.go`

- [ ] **步骤 1：编写失败的测试**

```go
package opsadmin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRejectsSymlinkAndMode(t *testing.T) {
	dir := t.TempDir()
	sym := filepath.Join(dir, "sym.env")
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sym); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAdminConfigFile(sym); err == nil {
		t.Fatal("expected symlink rejection")
	}
	loose := filepath.Join(dir, "loose.env")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAdminConfigFile(loose); err == nil {
		t.Fatal("expected 0600 requirement")
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./internal/opsadmin/ -v`
预期：FAIL，包不存在

- [ ] **步骤 3：迁移实现（admin.go）**

新建 `internal/opsadmin/admin.go`，把 `cmd/goalgo-ops/admin.go` 中 `validateAdminConfigFile` 迁移为导出 `ValidateAdminConfigFile`，并新增 `Bootstrap`（configPath 为空时用 `*opsprompt.Prompter` 交互采集写 `restore/admin-bootstrap.env`；调用 `compose.RunService` 跑 `admin-init`；成功后 `opsinstall.MarkAdminCreated`）：

```go
package opsadmin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"cwxu-algo/internal/opscompose"
	"cwxu-algo/internal/opsinstall"
	"cwxu-algo/internal/opsprompt"
	"cwxu-algo/internal/opsroot"
)

func ValidateAdminConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("配置文件不能是符号链接")
	}
	if !info.Mode().IsRegular() {
		return errors.New("配置文件必须是普通文件")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("配置文件权限必须为 0600，当前为 %o", info.Mode().Perm())
	}
	return nil
}

func Bootstrap(ctx context.Context, root *opsroot.Root, compose *opscompose.Compose, configPath string, prompt *opsprompt.Prompter) error {
	if configPath == "" {
		var err error
		configPath, err = promptAdminConfig(root, prompt)
		if err != nil {
			return err
		}
	}
	defer os.Remove(configPath)
	if err := ValidateAdminConfigFile(configPath); err != nil {
		return err
	}
	const containerMount = "/run/admin.env"
	output, err := compose.RunService(ctx, "user",
		[]string{"--user", "root", "--entrypoint", "/app/admin-init", "-v", configPath + ":" + containerMount + ":ro"},
		"--admin-config", containerMount)
	if err != nil {
		return fmt.Errorf("docker compose run 失败：%w\n%s", err, output)
	}
	return opsinstall.MarkAdminCreated(root)
}

func promptAdminConfig(root *opsroot.Root, prompt *opsprompt.Prompter) (string, error) {
	username, err := prompt.String("管理员用户名", "")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(username) == "" {
		return "", errors.New("管理员用户名不能为空")
	}
	email, err := prompt.String("管理员邮箱", "")
	if err != nil {
		return "", err
	}
	if !strings.Contains(email, "@") {
		return "", errors.New("邮箱格式不正确")
	}
	name, err := prompt.String("展示名（默认用户名）", username)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		name = username
	}
	var password string
	for {
		first, err := prompt.Password("密码")
		if err != nil {
			return "", err
		}
		second, err := prompt.Password("再次输入密码确认")
		if err != nil {
			return "", err
		}
		if first != second || first == "" {
			fmt.Fprintln(prompt.Out, "两次输入的密码不一致或为空，请重试")
			continue
		}
		password = first
		break
	}
	dir := root.Join("restore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "admin-bootstrap.env")
	content := fmt.Sprintf("ADMIN_USERNAME=%s\nADMIN_EMAIL=%s\nADMIN_NAME=%s\nADMIN_PASSWORD=%s\n",
		username, email, name, password)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
```

注意：`opsprompt.Password` 依赖 `os.Stdin` 掩码读取；测试中不触发该路径即可。

修改 `cmd/goalgo-ops/admin.go`：删除 `validateAdminConfigFile` 与 `promptAdminConfig`，`cmdAdminInit` 改为：

```go
func cmdAdminInit(args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	var rootPath, adminConfig string
	flags := flag.NewFlagSet("admin-init", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.StringVar(&adminConfig, "admin-config", "", "管理员配置文件路径（必须 root 所有、0600、非符号链接；成功后删除）")
	if err := flags.Parse(args); err != nil {
		return fail("参数", err)
	}
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("根目录", err)
	}
	if root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("管理员", errors.New("创建首个管理员需要 root 权限"))
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("锁", err)
	}
	defer lock.Release()
	compose := &opscompose.Compose{Root: root, Run: runner}
	if err := opsadmin.Bootstrap(ctx, root, compose, adminConfig, opsprompt.New()); err != nil {
		return fail("管理员", err)
	}
	return 0
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./internal/opsadmin/ ./internal/opsinstall/ && go build ./cmd/goalgo-ops`
预期：PASS + 构建成功

- [ ] **步骤 5：Commit**

```bash
git add internal/opsadmin cmd/goalgo-ops/admin.go && git commit -m "refactor(ops): extract opsadmin bootstrap"
```

---

## 任务 5：`opscompose.Pull`/`Up` 流式输出

**文件：** 修改 `internal/opscompose/compose.go`、`internal/opscompose/compose_test.go`

- [ ] **步骤 1：编写失败的测试（追加到 compose_test.go）**

```go
func TestPullAndUpUseStreamingRun(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".env", "release.env", "compose.yaml"} {
		path := root.Join(dir)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &streamRunner{}
	compose := &Compose{Root: root, Run: runner}
	if err := compose.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.ran == 0 {
		t.Fatal("Pull must use Run (streaming), not CombinedOutput")
	}
}
```

新增 fake：

```go
type streamRunner struct{ ran int }

func (f *streamRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	return "", nil
}

func (f *streamRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.ran++
	return nil
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./internal/opscompose/ -run TestPullAndUpUseStreamingRun -v`
预期：FAIL（`runner.ran == 0`）

- [ ] **步骤 3：修改 compose.go**

`Pull`/`Up` 改用流式（真实输出直接透传给调用者终端，解决「像阻塞、Ctrl+C 才出日志」）：

```go
func (c *Compose) Pull(ctx context.Context) error {
	err := c.Run.Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), "pull")...)
	if err != nil {
		return fmt.Errorf("compose pull：%w", err)
	}
	return nil
}

func (c *Compose) Up(ctx context.Context, timeout string) error {
	err := c.Run.Run(ctx, nil, os.Stdout, os.Stderr, "docker", append(c.baseArgs(), "up", "-d", "--wait", "--wait-timeout", timeout)...)
	if err != nil {
		return fmt.Errorf("compose up：%w", err)
	}
	return nil
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./internal/opscompose/ -v && go build ./...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add internal/opscompose/compose.go internal/opscompose/compose_test.go && git commit -m "fix(ops): stream compose pull/up output"
```

---

## 任务 6：`init` 命令 + usage 注册

**文件：** 创建 `cmd/goalgo-ops/init.go`；修改 `cmd/goalgo-ops/main.go`

- [ ] **步骤 1：编写契约测试（test_operations.py，先红）**

```python
def test_ops_usage_mentions_init_and_interactive(self):
    main = read("cmd/goalgo-ops/main.go")
    self.assertIn('"init"', main)
    self.assertIn("init [--root]", main)
```

在 `OperationalScriptTests` 类中追加（调用 `main.go` 的 usage 文本）。先运行确认失败。

- [ ] **步骤 2：运行测试验证失败**

运行：`python3 -m unittest deploy.tests.test_operations.OperationalScriptTests -v`
预期：FAIL，`'"init"' not found`

- [ ] **步骤 3：实现 init.go**

```go
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
		return initPrintTemplate(root)
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
	values := map[string]string{
		"COMPOSE_PROJECT_NAME": "goalgo",
		"GOALGO_HTTP_BIND":     "0.0.0.0",
		"GOALGO_HTTP_PORT":     "8988",
		"GOALGO_ROOT":          root.Path,
		"TZ":                   "Asia/Shanghai",
		"POSTGRES_USER":        "goalgo",
		"RABBITMQ_USER":        "goalgo",
	}
	order := []string{"GOALGO_HTTP_BIND", "GOALGO_HTTP_PORT", "TZ", "COMPOSE_PROJECT_NAME", "GOALGO_ROOT", "POSTGRES_USER", "RABBITMQ_USER"}
	for _, key := range order {
		def := values[key]
		got, err := prompt.String(envLabel(key), def)
		if err != nil {
			return fail("init", err)
		}
		if strings.TrimSpace(got) == "" {
			got = def
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

func initPrintTemplate(root *opsroot.Root) int {
	template := opsinstall.ReadTemplateEnv(root.Path)
	fmt.Print(template)
	return 0
}
```

在 `internal/opsinstall` 导出模板渲染：

```go
// ReadTemplateEnv 返回带注释的 .env 模板（首行写入 GOALGO_ROOT）。
func ReadTemplateEnv(root string) string {
	content, err := ReadAsset("env.example")
	if err != nil {
		return "GOALGO_ROOT=" + root + "\n"
	}
	rendered := fmt.Sprintf("GOALGO_ROOT=%s\n%s", root, string(content))
	return rendered + "\n# 其余密钥与凭据由 install 自动生成，无需填写。\n"
}
```

修改 `main.go`：注册 `case "init": return cmdInit(commandArgs, runner)`；usage 增加 `init [--root] 交互式填写 .env（--print 仅打印模板）`，并注明非交互报错文案。

- [ ] **步骤 4：运行测试验证通过**

运行：`go build ./cmd/goalgo-ops && python3 -m unittest deploy.tests.test_operations.OperationalScriptTests -v`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/goalgo-ops/init.go cmd/goalgo-ops/main.go internal/opsinstall/install.go deploy/tests/test_operations.py && git commit -m "feat(ops): add init command with interactive env prompt"
```

---

## 任务 7：`install` 5 步进度 + 管理员并入

**文件：** 修改 `cmd/goalgo-ops/main.go`；新增 `cmd/goalgo-ops/runtime.go`

- [ ] **步骤 1：编写失败的契约测试（test_operations.py）**

```python
def test_install_uses_progress_and_creates_admin(self):
    main = read("cmd/goalgo-ops/main.go")
    self.assertIn("opsprogress", main)
    self.assertIn("AdminCreated", main)
    self.assertIn("opsadmin.Bootstrap", main)
    self.assertIn("ResolveLatest", main)
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（`"opsprogress" not found`）

- [ ] **步骤 3：重写 cmdInstall（main.go）**

```go
func cmdInstall(args []string, runner opsexec.Command) int {
	ctx, stop := signalContext()
	defer stop()
	var rootPath, releaseFile string
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.StringVar(&rootPath, "root", "", "goalgo 根目录（默认 $GOALGO_ROOT 或 /opt/goalgo）")
	flags.StringVar(&releaseFile, "release-file", "", "从文件写入经过校验的 release.env（默认解析 <svc>-latest）")
	if err := flags.Parse(args); err != nil {
		return fail("安装", err)
	}
	root, err := opsroot.Resolve(rootPath)
	if err != nil {
		return fail("安装", err)
	}
	if root.IsProtectedInstall() && !opsroot.IsPrivileged() {
		return fail("安装", fmt.Errorf("安装到 %s 需要 root 权限", root.Path))
	}
	lock, err := opslock.Acquire(lockPath(root), 0)
	if err != nil {
		return fail("安装", err)
	}
	defer lock.Release()

	progress := opsprogress.New(5, os.Stderr)
	inst := opsinstall.New(root)

	progress.Step("初始化目录结构与模板")
	if err := inst.Scaffold(); err != nil {
		return fail("安装", err)
	}

	progress.Step("生成密钥与 JWT 密钥对")
	if err := inst.Secrets(); err != nil {
		return fail("安装", err)
	}

	compose := &opscompose.Compose{Root: root, Run: runner}

	progress.Step("解析发布镜像")
	if releaseFile != "" {
		release, err := opsrelease.ParseFile(releaseFile)
		if err != nil {
			return fail("安装", err)
		}
		if err := release.WriteFile(root.Join("release.env")); err != nil {
			return fail("安装", err)
		}
	} else {
		release, err := compose.ResolveLatest(ctx)
		if err != nil {
			return fail("安装", err)
		}
		if err := release.WriteFile(root.Join("release.env")); err != nil {
			return fail("安装", err)
		}
	}

	progress.Step("创建首个管理员（如无）")
	if !opsinstall.AdminCreated(root) {
		if err := opsadmin.Bootstrap(ctx, root, compose, "", opsprompt.New()); err != nil {
			return fail("安装", fmt.Errorf("创建管理员：%w", err))
		}
	} else {
		progress.Message("管理员已存在，跳过")
	}

	progress.Step("启动服务并冒烟")
	installStart(ctx, compose, progress)

	return 0
}
```

新增 `runtime.go` 的启动辅助（含 pull/up 子步骤提示；无 docker 时提示）：

```go
func installStart(ctx context.Context, compose *opscompose.Compose, progress *opsprogress.Progress) error {
	if err := compose.Version(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "goalgo-ops: 安装：初始化完成；未检测到 docker compose，请安装后执行 `goalgo-ops start`")
		return nil
	}
	progress.Sub("拉取发布镜像")
	if err := compose.Pull(ctx); err != nil {
		return fmt.Errorf("拉取镜像：%w", err)
	}
	progress.Sub("创建并启动容器")
	if err := compose.Up(ctx, compose.WaitTimeout()); err != nil {
		return fmt.Errorf("启动服务：%w", err)
	}
	progress.Sub("等待健康并冒烟")
	if err := compose.Health(ctx); err != nil {
		return fmt.Errorf("健康检查：%w", err)
	}
	if err := compose.Smoke(ctx); err != nil {
		return fmt.Errorf("冒烟测试：%w", err)
	}
	opsprogress.Done(os.Stderr, "安装完成")
	return nil
}
```

`opsprogress` 增加 `Done`：

```go
func Done(out io.Writer, message string) {
	fmt.Fprintf(out, "%s\n", message)
}
```

同样在 `deploy`/`rollback`/`restart`（`cmdRuntime` 相关）调用 `compose.Pull`/`Up` 前补 `progress.Sub(...)`；`deploy` 缺候选文件时用 `opsprompt.Choice` 选择（默认当前 `release.env`）。非 TTY 且缺候选时维持现状（读当前 release.env）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go build ./cmd/goalgo-ops && python3 -m unittest deploy.tests.test_operations -v`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/goalgo-ops/main.go cmd/goalgo-ops/runtime.go internal/opsprogress/progress.go deploy/tests/test_operations.py && git commit -m "feat(ops): install progress, admin bootstrap, streaming start"
```

---

## 任务 8：`restore` / `backup` / `config` 缺参交互

**文件：** 修改 `cmd/goalgo-ops/restore.go`、`backup.go`、`config.go`

- [ ] **步骤 1：编写失败的契约测试（test_operations.py）**

```python
def test_destructive_commands_prompt_when_missing_args(self):
    restore = read("cmd/goalgo-ops/restore.go")
    self.assertIn("opsprompt", restore)
    self.assertIn("RESTORE", restore)
    backup = read("cmd/goalgo-ops/backup.go")
    self.assertIn("opsprompt", backup)
    config = read("cmd/goalgo-ops/config.go")
    self.assertIn("opsprompt", config)
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（`"opsprompt" not found`）

- [ ] **步骤 3：实现交互补全**

`restore.go`：
- 缺 `--file`/`--latest` 且 TTY：扫描 `root/restore` 与 `./` 下 `*.cwxubak` 用 `opsprompt.Choice` 选择，或选「从又拍云拉取最新」。
- 缺 `--key-file` 且 TTY：`opsprompt.String` 输入路径。
- `--replace`/`--confirm` 未提供但 TTY：`opsprompt` 提示「将覆盖已有数据，输入 RESTORE 确认」，输入等于 `confirmToken` 则视为已确认。
- 非 TTY 且缺项：维持现有报错。

`backup.go`：
- `backup download` 缺 `UPYUN_*` 且 TTY：逐个 `opsprompt.String` 输入 bucket/operator/password（password 掩码）；缺 output 交互确认默认。
- `backup verify` 缺 `--file`/`--key-file` 且 TTY：`opsprompt.String` 输入路径。

`config.go`：
- `config export` 缺 output（默认值仍生效）无需交互；`config import` 缺 `--file` 且 TTY：`opsprompt.String` 输入 tar 路径。

非 TTY 分支保持原有 `fail` 报错文本不变（脚本兼容）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go build ./cmd/goalgo-ops && go vet ./cmd/goalgo-ops && python3 -m unittest deploy.tests.test_operations -v`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add cmd/goalgo-ops/restore.go cmd/goalgo-ops/backup.go cmd/goalgo-ops/config.go deploy/tests/test_operations.py && git commit -m "feat(ops): interactive completion for restore/backup/config"
```

---

## 任务 9：全量验证 + 文档 + 收尾

- [ ] **步骤 1：运行全量 Go 测试**

运行：`go test ./... 2>&1 | tail -40`
预期：无 FAIL（`cmd/goalgo-ops` 主包测试为 0，其余包 PASS）

- [ ] **步骤 2：运行全量契约测试**

运行：`python3 -m unittest discover -s deploy/tests -p 'test_*.py' -v`
预期：OK

- [ ] **步骤 3：运行 vet 与构建**

运行：`go vet ./... && go build ./... && git diff --check`
预期：无输出

- [ ] **步骤 4：更新 OPS.md 命令说明**

将 `init`、交互式触发、install 进度输出补充到根 `OPS.md`（goalgo-ops 使用说明）。

- [ ] **步骤 5：Commit + 根仓库钉住 + edits.md**

```bash
git add -A && git commit -m "feat(ops): interactive goalgo-ops (init/install progress/prompt)"
# 根仓库：git add cwxu-algo edits.md && git commit -m "feat(ops): pin interactive ops"
# 清空 edits.md 正文
```
