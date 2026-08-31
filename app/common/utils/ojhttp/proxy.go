package ojhttp

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type ProxyConfig struct {
	BaseURL string
	Secret  string
	Enabled func(string) bool
}

var proxyState struct {
	sync.RWMutex
	cfg ProxyConfig
}

func SetProxyConfig(cfg ProxyConfig) {
	proxyState.Lock()
	proxyState.cfg = cfg
	proxyState.Unlock()
}

func proxyTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid OJ URL")
	}
	proxyState.RLock()
	cfg := proxyState.cfg
	proxyState.RUnlock()
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Secret) == "" || cfg.Enabled == nil || !cfg.Enabled(u.Hostname()) {
		return raw, nil
	}
	var b strings.Builder
	for i, r := range raw {
		key := []rune(cfg.Secret)[i%len([]rune(cfg.Secret))]
		b.WriteRune(r ^ key)
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(b.String()))
	return strings.TrimRight(cfg.BaseURL, "/") + "/" + encoded, nil
}

func Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	target, err := proxyTarget(req.URL.String())
	if err != nil {
		return nil, err
	}
	if target != req.URL.String() {
		clone := req.Clone(req.Context())
		parsed, parseErr := url.Parse(target)
		if parseErr != nil {
			return nil, parseErr
		}
		clone.URL = parsed
		clone.Host = parsed.Host
		clone.RequestURI = ""
		req = clone
	}
	EnsureHeaders(req)
	return Client.Do(req)
}
