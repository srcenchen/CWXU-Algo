package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cwxu-algo/api/core/v1/spider"
	"cwxu-algo/app/common/utils/clientip"
	kratoserrors "github.com/go-kratos/kratos/v2/errors"
)

const luoguResolverCacheTTL = time.Minute

var luoguResolverQueryPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type luoguResolvedUser struct {
	UID      string
	Username string
}

type luoguResolverCacheEntry struct {
	user      luoguResolvedUser
	expiresAt time.Time
}

var luoguResolverCache sync.Map // map[string]luoguResolverCacheEntry

// luoguResolverClient 携带 cookie jar，用于通过洛谷 C3VK 反爬（首次请求 302+Set-Cookie，
// 跟随重定向时带上 cookie 才能拿到 JSON）。公开接口免登录即可访问。
var luoguResolverClient = func() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		jar = nil
	}
	return &http.Client{Timeout: 10 * time.Second, Jar: jar}
}()

// ResolveLuoguUser 公开解析洛谷 UID/用户名；写入 GoAlgo 绑定仍由 SetSpider 的 JWT 保护。
func (s *SpiderService) ResolveLuoguUser(ctx context.Context, req *spider.ResolveLuoguUserReq) (*spider.ResolveLuoguUserRes, error) {
	if req == nil {
		return nil, kratoserrors.New(400, "LUOGU_USER_INVALID", "请输入有效的洛谷 UID 或用户名")
	}
	query := strings.TrimSpace(req.GetQuery())
	if query == "" || !luoguResolverQueryPattern.MatchString(query) {
		return nil, kratoserrors.New(400, "LUOGU_USER_INVALID", "请输入有效的洛谷 UID 或用户名")
	}
	key := strings.ToLower(query)
	if user, ok := resolveNumericLuoguUID(query); ok {
		return &spider.ResolveLuoguUserRes{Uid: user.UID, Username: user.Username}, nil
	}
	if cached, ok := luoguResolverCache.Load(key); ok {
		entry := cached.(luoguResolverCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return &spider.ResolveLuoguUserRes{Uid: entry.user.UID, Username: entry.user.Username}, nil
		}
		luoguResolverCache.Delete(key)
	}
	ip := clientip.FromContext(ctx)
	if ip == "" {
		ip = "anonymous"
	}
	if !s.allow(ctx, "ratelimit:luogu:resolve:"+ip, time.Second) {
		return nil, kratoserrors.New(429, "LUOGU_RESOLVER_RATE_LIMIT", "请求过于频繁，请稍后再试")
	}

	client := luoguResolverClient
	var user luoguResolvedUser
	var err error
	user, err = fetchLuoguSearch(ctx, client, query)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, kratoserrors.NotFound("LUOGU_USER_NOT_FOUND", "洛谷无法识别该用户，请检查用户名或 UID")
		}
		return nil, kratoserrors.New(502, "LUOGU_RESOLVER_UNAVAILABLE", "洛谷用户查询暂不可用，请稍后重试")
	}
	luoguResolverCache.Store(key, luoguResolverCacheEntry{user: user, expiresAt: time.Now().Add(luoguResolverCacheTTL)})
	return &spider.ResolveLuoguUserRes{Uid: user.UID, Username: user.Username}, nil
}

func resolveNumericLuoguUID(query string) (luoguResolvedUser, bool) {
	uid, err := strconv.ParseInt(query, 10, 64)
	if err != nil || uid <= 0 {
		return luoguResolvedUser{}, false
	}
	value := strconv.FormatInt(uid, 10)
	return luoguResolvedUser{UID: value, Username: value}, true
}

const luoguResolverUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

func fetchLuoguInfo(ctx context.Context, client *http.Client, uid int64) (luoguResolvedUser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://www.luogu.com.cn/api/user/info/%d", uid), nil)
	if err != nil {
		return luoguResolvedUser{}, err
	}
	request.Header.Set("User-Agent", luoguResolverUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return luoguResolvedUser{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return luoguResolvedUser{}, fmt.Errorf("not found")
	}
	if response.StatusCode/100 != 2 {
		return luoguResolvedUser{}, fmt.Errorf("status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if err != nil {
		return luoguResolvedUser{}, err
	}
	var payload struct {
		User struct {
			UID  int64  `json:"uid"`
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.User.UID <= 0 || strings.TrimSpace(payload.User.Name) == "" {
		return luoguResolvedUser{}, fmt.Errorf("not found")
	}
	return luoguResolvedUser{UID: strconv.FormatInt(payload.User.UID, 10), Username: payload.User.Name}, nil
}

func fetchLuoguSearch(ctx context.Context, client *http.Client, query string) (luoguResolvedUser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.luogu.com.cn/api/user/search?keyword="+url.QueryEscape(query), nil)
	if err != nil {
		return luoguResolvedUser{}, err
	}
	request.Header.Set("User-Agent", luoguResolverUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return luoguResolvedUser{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return luoguResolvedUser{}, fmt.Errorf("status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if err != nil {
		return luoguResolvedUser{}, err
	}
	return parseLuoguSearchBody(body, query)
}

func parseLuoguSearchBody(body []byte, query string) (luoguResolvedUser, error) {
	var payload struct {
		Users []struct {
			UID  int64  `json:"uid"`
			Name string `json:"name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return luoguResolvedUser{}, err
	}
	for _, candidate := range payload.Users {
		if candidate.UID > 0 && strings.EqualFold(candidate.Name, query) {
			return luoguResolvedUser{UID: strconv.FormatInt(candidate.UID, 10), Username: candidate.Name}, nil
		}
	}
	if len(payload.Users) == 1 && payload.Users[0].UID > 0 && strings.TrimSpace(payload.Users[0].Name) != "" {
		candidate := payload.Users[0]
		return luoguResolvedUser{UID: strconv.FormatInt(candidate.UID, 10), Username: candidate.Name}, nil
	}
	return luoguResolvedUser{}, fmt.Errorf("not found")
}
