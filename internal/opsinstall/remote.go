package opsinstall

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// 受管模板的远端来源：ops install / upgrade --all 从 GitHub raw 拉取仓库里
// 最新的 compose/config 模板（经 gh-proxy 代理），保证服务器端配置与仓库一致，
// 不再依赖二进制里编译期的内嵌快照。远端不可达时回退内嵌模板，离线也能安装。
//
// 可用环境变量覆盖（私有部署/离线镜像源）：
//
//	GOALGO_CONFIG_REMOTE   "0"/"false" 禁用远端拉取（仅用内嵌模板）
//	GOALGO_CONFIG_OWNER    仓库 owner，默认 WXUProjects
//	GOALGO_CONFIG_REPO     仓库名，默认 CWXU-Algo
//	GOALGO_CONFIG_BRANCH   分支，默认 main
//	GOALGO_CONFIG_PROXY    gh-proxy 前缀，默认 https://gh-proxy.com/
const (
	defaultConfigOwner  = "WXUProjects"
	defaultConfigRepo   = "CWXU-Algo"
	defaultConfigBranch = "main"
	defaultConfigProxy  = "https://gh-proxy.com/"
)

// FetchAsset 返回受管模板内容：优先从 GitHub raw（gh-proxy 前缀）拉取，
// 拉取失败时回退到编译期内嵌资产。
func FetchAsset(ctx context.Context, relative string) ([]byte, error) {
	if remoteEnabled() {
		data, err := fetchRemote(ctx, relative)
		if err == nil {
			return data, nil
		}
		fmt.Fprintf(os.Stderr, "goalgo-ops: 拉取远端模板 %s 失败（%v），回退内嵌模板\n", relative, err)
	}
	return ReadAsset(relative)
}

func remoteEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOALGO_CONFIG_REMOTE"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// remoteBaseURL 形如 https://gh-proxy.com/https://raw.githubusercontent.com/<owner>/<repo>/<branch>/ 。
func remoteBaseURL() string {
	owner := envOr("GOALGO_CONFIG_OWNER", defaultConfigOwner)
	repo := envOr("GOALGO_CONFIG_REPO", defaultConfigRepo)
	branch := envOr("GOALGO_CONFIG_BRANCH", defaultConfigBranch)
	proxy := strings.TrimRight(envOr("GOALGO_CONFIG_PROXY", defaultConfigProxy), "/")
	return fmt.Sprintf("%s/https://raw.githubusercontent.com/%s/%s/%s/", proxy, owner, repo, branch)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func fetchRemote(ctx context.Context, relative string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := remoteBaseURL() + "deploy/" + relative
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}