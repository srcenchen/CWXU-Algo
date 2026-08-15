package sitesettings

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateEndpoint 校验 OpenAI-compatible 服务地址：
// http/https + host；拒绝 userinfo / query / fragment。
func ValidateEndpoint(endpoint string) error {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return fmt.Errorf("服务地址格式不合法: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("服务地址必须以 http 或 https 开头")
	}
	if u.Host == "" {
		return fmt.Errorf("服务地址缺少主机名")
	}
	if u.User != nil {
		return fmt.Errorf("服务地址不能包含用户名/密码")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("服务地址不能包含查询参数或锚点")
	}
	return nil
}
