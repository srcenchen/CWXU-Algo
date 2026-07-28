// Package upyun implements a minimal REST client for UpYun cloud storage.
package upyun

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultAPIHost = "https://v0.api.upyun.com"

// Config holds operator credentials and public access domain.
type Config struct {
	Bucket   string
	Operator string
	Password string
	// Domain is the CDN / access host, e.g. "zhiyuansofts.cn" or "http://zhiyuansofts.cn"
	Domain string
	// Scheme "http" | "https"; empty → infer from Domain or default "http"
	Scheme string
	// APIHost overrides default REST endpoint (tests)
	APIHost string
	// HTTPClient optional
	HTTPClient *http.Client
}

// Client uploads / deletes objects on UpYun.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New returns a client; Configured() is false if required fields missing.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{cfg: cfg, hc: hc}
}

// Configured reports whether bucket/operator/password are set.
func (c *Client) Configured() bool {
	if c == nil {
		return false
	}
	return strings.TrimSpace(c.cfg.Bucket) != "" &&
		strings.TrimSpace(c.cfg.Operator) != "" &&
		strings.TrimSpace(c.cfg.Password) != ""
}

// PublicBaseURL returns scheme://domain without trailing slash.
func (c *Client) PublicBaseURL() string {
	domain := strings.TrimSpace(c.cfg.Domain)
	scheme := strings.ToLower(strings.TrimSpace(c.cfg.Scheme))
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return strings.TrimRight(domain, "/")
	}
	if scheme != "http" && scheme != "https" {
		scheme = "http"
	}
	domain = strings.TrimPrefix(domain, "//")
	domain = strings.TrimRight(domain, "/")
	if domain == "" {
		return ""
	}
	return scheme + "://" + domain
}

// PublicURL joins base + object key.
func (c *Client) PublicURL(objectKey string) string {
	base := c.PublicBaseURL()
	key := "/" + strings.TrimPrefix(objectKey, "/")
	if base == "" {
		return key
	}
	return base + key
}

// Put uploads raw bytes to objectKey (leading slash optional).
func (c *Client) Put(objectKey string, data []byte, contentType string) error {
	if !c.Configured() {
		return fmt.Errorf("upyun not configured")
	}
	key := "/" + strings.TrimPrefix(objectKey, "/")
	uri := "/" + strings.Trim(c.cfg.Bucket, "/") + key
	host := strings.TrimRight(c.cfg.APIHost, "/")
	if host == "" {
		host = defaultAPIHost
	}
	url := host + uri
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", c.sign("PUT", uri, date))
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upyun put %s: %s %s", key, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Delete removes an object. Missing object is treated as success.
func (c *Client) Delete(objectKey string) error {
	if !c.Configured() {
		return fmt.Errorf("upyun not configured")
	}
	key := "/" + strings.TrimPrefix(objectKey, "/")
	uri := "/" + strings.Trim(c.cfg.Bucket, "/") + key
	host := strings.TrimRight(c.cfg.APIHost, "/")
	if host == "" {
		host = defaultAPIHost
	}
	url := host + uri
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", c.sign("DELETE", uri, date))
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// 404 = already gone
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upyun delete %s: %s %s", key, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// sign builds Authorization: UPYUN operator:base64(hmac-sha1(md5(password), METHOD&URI&Date))
func (c *Client) sign(method, uri, date string) string {
	passMD5 := md5Hex(c.cfg.Password)
	msg := strings.ToUpper(method) + "&" + uri + "&" + date
	mac := hmac.New(sha1.New, []byte(passMD5))
	mac.Write([]byte(msg))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return "UPYUN " + c.cfg.Operator + ":" + sig
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SignForTest exposes sign for unit tests.
func (c *Client) SignForTest(method, uri, date string) string {
	return c.sign(method, uri, date)
}
