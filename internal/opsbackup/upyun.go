package opsbackup

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"cwxu-algo/app/common/cwxubak"
)

// Upyun 又拍云对象存储下载客户端（与 core_data 备份发布同一签名算法）。
type Upyun struct {
	Endpoint string
	Bucket   string
	Operator string
	Password string
	Prefix   string
	client   *http.Client
}

type UpyunConfig struct {
	Bucket   string
	Operator string
	Password string
	Prefix   string
}

func NewUpyun(cfg UpyunConfig) *Upyun {
	return &Upyun{
		Endpoint: "https://v0.api.upyun.com",
		Bucket:   cfg.Bucket,
		Operator: cfg.Operator,
		Password: cfg.Password,
		Prefix:   strings.Trim(cfg.Prefix, "/"),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Upyun) request(ctx context.Context, method, key string) (*http.Request, error) {
	escaped := strings.Join(strings.FieldsFunc(key, func(r rune) bool { return r == '/' }), "/")
	uri := "/" + url.PathEscape(s.Bucket) + "/" + escaped
	req, err := http.NewRequestWithContext(ctx, method, s.Endpoint+uri, nil)
	if err != nil {
		return nil, err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	passwordMD5 := md5.Sum([]byte(s.Password))
	mac := hmac.New(sha1.New, []byte(fmt.Sprintf("%x", passwordMD5)))
	_, _ = io.WriteString(mac, method+"&"+uri+"&"+date)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "UPYUN "+s.Operator+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return req, nil
}

// LatestPointer 拉取 <prefix>/latest.json。
func (s *Upyun) LatestPointer(ctx context.Context) (*cwxubak.Pointer, error) {
	req, err := s.request(ctx, http.MethodGet, path.Join(s.Prefix, "latest.json"))
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取 latest.json：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 latest.json：HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	return cwxubak.ParsePointer(data)
}

// DownloadArchive 下载归档到本地文件并校验 size + SHA-256（与 latest.json 比对）。
func (s *Upyun) DownloadArchive(ctx context.Context, pointer *cwxubak.Pointer, destination string) (string, error) {
	if !strings.HasPrefix(pointer.ArchiveKey, s.Prefix+"/") {
		return "", fmt.Errorf("归档 key %q 不在前缀 %q 内", pointer.ArchiveKey, s.Prefix)
	}
	if !strings.HasSuffix(pointer.ArchiveKey, ".cwxubak") {
		return "", fmt.Errorf("归档 key 必须以 .cwxubak 结尾：%q", pointer.ArchiveKey)
	}
	req, err := s.request(ctx, http.MethodGet, pointer.ArchiveKey)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载归档：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载归档：HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, digest), resp.Body)
	if err != nil {
		file.Close()
		os.Remove(destination)
		return "", fmt.Errorf("写入归档：%w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(destination)
		return "", err
	}
	actualHash := hex.EncodeToString(digest.Sum(nil))
	if size != pointer.Size || actualHash != pointer.SHA256 {
		os.Remove(destination)
		return "", fmt.Errorf("归档校验失败：size=%d(期望%d) sha256=%s(期望%s)", size, pointer.Size, actualHash, pointer.SHA256)
	}
	return actualHash, nil
}
