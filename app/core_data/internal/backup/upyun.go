package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var ErrObjectExists = errors.New("immutable object already exists")

// UpyunStore implements ObjectStore using Upyun's authenticated REST API.
type UpyunStore struct {
	bucket, operator, password string
	client                     *http.Client
	endpoint                   string
}

func NewUpyunStore(bucket, operator, password string, client *http.Client) *UpyunStore {
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}}
	}
	return &UpyunStore{bucket: bucket, operator: operator, password: password, client: client, endpoint: "https://v0.api.upyun.com"}
}

func (s *UpyunStore) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	exists, err := s.exists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return ErrObjectExists
	}
	req, err := s.request(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if file, ok := body.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		position, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		req.ContentLength = info.Size() - position
	}
	return s.do(req, http.StatusOK, http.StatusCreated)
}

func (s *UpyunStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp.Body, nil
}

func (s *UpyunStore) List(ctx context.Context, prefix, token string) ([]string, string, error) {
	req, err := s.request(ctx, http.MethodGet, strings.TrimSuffix(prefix, "/")+"/", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-List-Limit", "100")
	if token != "" {
		req.Header.Set("X-List-Iter", token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", responseError(resp)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	var keys []string
	for _, line := range strings.Split(string(b), "\n") {
		name := strings.SplitN(line, "\t", 2)[0]
		if name != "" {
			keys = append(keys, strings.TrimSuffix(prefix, "/")+"/"+name)
		}
	}
	next := resp.Header.Get("X-Upyun-List-Iter")
	if next == "g2gCZAAEbmV4dGQAA2VvZg" {
		next = ""
	}
	return keys, next, nil
}

func (s *UpyunStore) Delete(ctx context.Context, key string) error {
	req, err := s.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	return s.do(req, http.StatusOK)
}

func (s *UpyunStore) Move(ctx context.Context, source, destination string) error {
	req, err := s.request(ctx, http.MethodPut, destination, nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("X-Upyun-Move-Source", s.objectURI(source))
	if err := s.do(req, http.StatusOK); err != nil {
		return fmt.Errorf("%w: %v", ErrAmbiguousPublish, err)
	}
	return nil
}

func (s *UpyunStore) exists(ctx context.Context, key string) (bool, error) {
	req, err := s.request(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, responseError(resp)
	}
}

func (s *UpyunStore) request(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	uri := s.objectURI(key)
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+uri, body)
	if err != nil {
		return nil, err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	passwordMD5 := md5.Sum([]byte(s.password))
	mac := hmac.New(sha1.New, []byte(fmt.Sprintf("%x", passwordMD5)))
	_, _ = io.WriteString(mac, method+"&"+uri+"&"+date)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "UPYUN "+s.operator+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return req, nil
}

func (s *UpyunStore) objectURI(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '/' })
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/" + url.PathEscape(s.bucket) + "/" + strings.Join(parts, "/")
}

func (s *UpyunStore) do(req *http.Request, wanted ...int) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, status := range wanted {
		if resp.StatusCode == status {
			return nil
		}
	}
	return responseError(resp)
}

func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("upyun %s %s: status %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, strings.TrimSpace(string(bytes.TrimSpace(b))))
}
