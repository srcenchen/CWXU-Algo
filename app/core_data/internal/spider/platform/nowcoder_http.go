package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cwxu-algo/app/common/utils/ojhttp"
)

// 牛客 ac.nowcoder.com / gw-c.nowcoder.com 挂了阿里云 WAF：
// GoAlgo 自定义 UA（含 "compatible; GoAlgo"）会返回 HTTP 200 的滑块挑战页，
// 解析端拿到空表/无 Rating 会当成「成功无数据」，甚至把已有 rating 写成 0。
// 题面抓取（problem_fetch）已用浏览器指纹；提交/Rating/比赛历史走同一策略。

const nowcoderBrowserUA = `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36`

func isNowCoderWAFBody(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	// 挑战页体积小且带 aliyun_waf meta；正文 profile 通常 >20KB
	s := string(b)
	return strings.Contains(s, "aliyun_waf") ||
		strings.Contains(s, "aliyun_waf_aa") ||
		strings.Contains(s, "name=\"waf\"")
}

func nowcoderErrIfWAF(body []byte) error {
	if isNowCoderWAFBody(body) {
		return fmt.Errorf("nowcoder 被 WAF 拦截（挑战页）")
	}
	return nil
}

// nowcoderPrepareReq 覆盖 ojhttp 默认 UA/Accept，伪装成浏览器导航/XHR。
func nowcoderPrepareReq(req *http.Request, html bool) {
	if req == nil {
		return
	}
	req.Header.Set("User-Agent", nowcoderBrowserUA)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	if html {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	} else {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	if req.Header.Get("Referer") == "" {
		req.Header.Set("Referer", "https://ac.nowcoder.com/")
	}
}

func nowcoderDo(req *http.Request, html bool) (*http.Response, error) {
	nowcoderPrepareReq(req, html)
	// ojhttp.EnsureHeaders 不覆盖已有 UA/Accept
	return ojhttp.Do(req)
}

func nowcoderGet(ctx context.Context, rawURL string, html bool) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return nowcoderDo(req, html)
}

// nowcoderReadOK 读响应体；WAF 或非 200 返回 error（调用方勿当「无数据」）。
func nowcoderReadOK(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	defer resp.Body.Close()
	if limit <= 0 {
		limit = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if err := nowcoderErrIfWAF(body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 240 {
			snippet = snippet[:240]
		}
		return nil, fmt.Errorf("请求响应码错误 %d, %s", resp.StatusCode, snippet)
	}
	return body, nil
}
