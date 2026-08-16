package opsrelease

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const Repository = "registry.cn-hangzhou.aliyuncs.com/sanenchen/goalgo"

const imagePrefix = Repository + "@sha256:"

var imageDigest = regexp.MustCompile(`^` + imagePrefix + `[0-9a-f]{64}$`)

var imageTag = regexp.MustCompile(`^` + regexp.QuoteMeta(Repository) + `:(frontend|gateway|user|core-data|agent)-latest$`)

var serviceKeys = []struct {
	Service string
	Key     string
}{
	{"frontend", "FRONTEND_IMAGE"},
	{"gateway", "GATEWAY_IMAGE"},
	{"user", "USER_IMAGE"},
	{"core-data", "CORE_DATA_IMAGE"},
	{"agent", "AGENT_IMAGE"},
}

// LatestTagRelease 返回引用 <repo>:<svc>-latest 的发布清单（实际镜像走 latest，版本判断用解析出的 digest）。
func LatestTagRelease() *Release {
	release := &Release{Images: map[string]string{}}
	for _, entry := range serviceKeys {
		release.Images[entry.Key] = Repository + ":" + entry.Service + "-latest"
	}
	return release
}

var required = []string{"FRONTEND_IMAGE", "GATEWAY_IMAGE", "USER_IMAGE", "CORE_DATA_IMAGE", "AGENT_IMAGE"}

type Release struct {
	Images map[string]string
}

func Parse(r io.Reader) (*Release, error) {
	seen := map[string]int{}
	values := map[string]string{}
	assign := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := assign.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("无效的 release 行：%q", line)
		}
		key, value := m[1], m[2]
		if seen[key]++; seen[key] > 1 {
			return nil, fmt.Errorf("重复的赋值：%s", key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, key := range required {
		value, ok := values[key]
		if !ok {
			return nil, fmt.Errorf("缺少必需的赋值：%s", key)
		}
		if !imageDigest.MatchString(value) && !imageTag.MatchString(value) {
			return nil, fmt.Errorf("%s 必须是 %s 加 64 位小写十六进制，或 <service>-latest 标签", key, imagePrefix)
		}
	}
	if len(values) != len(required) {
		return nil, fmt.Errorf("release 只能包含五个应用镜像的赋值")
	}
	release := &Release{Images: map[string]string{}}
	for _, key := range required {
		release.Images[key] = values[key]
	}
	return release, nil
}

func ParseFile(path string) (*Release, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Parse(file)
}

func (r *Release) WriteFile(path string) error {
	if r == nil || len(r.Images) != len(required) {
		return fmt.Errorf("release 无效")
	}
	dir := filepath.Dir(path)
	temporary := path + ".tmp"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var builder strings.Builder
	for _, key := range required {
		builder.WriteString(key + "=" + r.Images[key] + "\n")
	}
	if err := os.WriteFile(temporary, []byte(builder.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}
