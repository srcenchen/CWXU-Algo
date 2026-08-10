// Package payment 支付网关抽象（支付FM聚合支付实现；https://docs.zhifux.com）。
// 对接规范：创建订单 POST {接口根地址}/startOrder（query 传参，MD5 签名），
// 返回 data.payUrl 支付链接（前端跳转支付页）；支付成功后平台回调 notifyUrl（GET，
// apiMode=post_form 时 POST），商户系统验签后返回字符串 success。
package payment

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PaymentNotification 支付平台回调通知（已验签）
type PaymentNotification struct {
	// OrderNo 商户订单号
	OrderNo string
	// PlatformOrderNo 平台订单号（platformOrderNo）
	PlatformOrderNo string
	// Amount 订单金额（元字符串，如 2.00）
	Amount string
	// PayTime 支付时间
	PayTime time.Time
}

// PaymentGateway 支付网关接口（可替换/可测试）
type PaymentGateway interface {
	// CreateOrder 创建订单，返回支付链接 payUrl（前端跳转支付页）
	CreateOrder(ctx context.Context, orderNo string, amountCents int64, subject string) (payURL string, err error)
	// ParseNotification 解析并验签支付回调（values 为 GET query / POST form 参数）；验签失败返回错误
	ParseNotification(values map[string]string) (*PaymentNotification, error)
}

// PayFmGateway 支付FM实现（接口根地址/商户号/接入密钥来自站点配置）。
type PayFmGateway struct {
	apiBase    string // 接口根地址（商户后台用户中心查看）
	merchantNo string // 商户号
	secret     string // 接入密钥
	payType    string // 支付方式（如 aloop=支付宝轮循池）
	notifyURL  string // 异步回调地址（下单时带给支付FM）
	client     *http.Client
}

// NewPayFmGateway 创建支付FM网关；接口地址/商户号/密钥为空返回错误（调用方按「支付未配置」处理）。
func NewPayFmGateway(apiBase, merchantNo, secret, payType, notifyURL string) (*PayFmGateway, error) {
	apiBase = strings.TrimSpace(apiBase)
	merchantNo = strings.TrimSpace(merchantNo)
	secret = strings.TrimSpace(secret)
	if apiBase == "" || merchantNo == "" || secret == "" {
		return nil, fmt.Errorf("支付未配置（缺少接口根地址/商户号/接入密钥）")
	}
	if payType = strings.TrimSpace(payType); payType == "" {
		payType = "aloop" // 默认支付宝轮循池
	}
	return &PayFmGateway{
		apiBase:    strings.TrimRight(apiBase, "/"),
		merchantNo: merchantNo,
		secret:     secret,
		payType:    payType,
		notifyURL:  strings.TrimSpace(notifyURL),
		client:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// sign 待签名字符串 MD5（小写 32 位）：调用方按文档顺序拼接。
func signMD5(raw string) string {
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateOrder 创建订单；返回支付链接 payUrl（前端跳转支付页）。
// 文档：POST {接口根地址}/startOrder?merchantNum=..&orderNo=..&amount=..&notifyUrl=..&payType=..&sign=..
// 签名 = md5(商户号 + 商户订单号 + 支付金额 + 异步通知地址 + 接入密钥)。
func (g *PayFmGateway) CreateOrder(ctx context.Context, orderNo string, amountCents int64, subject string) (string, error) {
	if g == nil || g.apiBase == "" {
		return "", fmt.Errorf("支付网关未初始化")
	}
	amount := FormatCents(amountCents) // 元字符串，最多两位小数
	params := url.Values{}
	params.Set("merchantNum", g.merchantNo)
	params.Set("orderNo", orderNo)
	params.Set("amount", amount)
	params.Set("notifyUrl", g.notifyURL)
	params.Set("payType", g.payType)
	if strings.TrimSpace(subject) != "" {
		params.Set("subject", subject)
	}
	params.Set("returnType", "json")
	params.Set("sign", signMD5(g.merchantNo+orderNo+amount+g.notifyURL+g.secret))

	endpoint := g.apiBase + "/startOrder?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("支付FM下单请求构建失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("支付FM下单请求失败: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Data    struct {
			ID     string `json:"id"`
			PayURL string `json:"payUrl"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("支付FM下单响应解析失败: %w", err)
	}
	if !out.Success || out.Code != 200 || strings.TrimSpace(out.Data.PayURL) == "" {
		msg := strings.TrimSpace(out.Msg)
		if msg == "" {
			msg = "未知错误"
		}
		return "", fmt.Errorf("支付FM下单被拒: %s", msg)
	}
	return out.Data.PayURL, nil
}

// ParseNotification 解析并验签支付回调（GET query / POST form 参数）。
// 签名 = md5(付款成功状态state的值 + 商户号 + 商户订单号 + 订单金额 + 接入密钥)。
// state=1 才视为付款成功；商户号不匹配或验签失败返回错误。
func (g *PayFmGateway) ParseNotification(values map[string]string) (*PaymentNotification, error) {
	if g == nil || g.secret == "" {
		return nil, fmt.Errorf("支付网关未初始化")
	}
	state := values["state"]
	merchantNo := values["merchantNum"]
	orderNo := values["orderNo"]
	amount := values["amount"]
	sign := values["sign"]
	if state == "" || merchantNo == "" || orderNo == "" || amount == "" || sign == "" {
		return nil, fmt.Errorf("支付FM回调参数缺失")
	}
	if merchantNo != g.merchantNo {
		return nil, fmt.Errorf("支付FM回调商户号不匹配: %s", merchantNo)
	}
	if state != "1" {
		return nil, fmt.Errorf("支付FM回调非付款成功状态 state=%s", state)
	}
	expect := signMD5(state + merchantNo + orderNo + amount + g.secret)
	if expect != strings.ToLower(strings.TrimSpace(sign)) {
		return nil, fmt.Errorf("支付FM回调验签失败")
	}
	payTime, _ := time.ParseInLocation("2006-01-02 15:04:05", values["payTime"], time.Local)
	return &PaymentNotification{
		OrderNo:         orderNo,
		PlatformOrderNo: values["platformOrderNo"],
		Amount:          amount,
		PayTime:         payTime,
	}, nil
}

// FormatCents 分 → 元字符串（两位小数，如 200 → "2.00"）
func FormatCents(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
}

// ParseYuanStringToCents 元字符串 → 分（如 "2.00" → 200）；非法返回错误。
// 回调金额比对用：支付FM amount 为元字符串。
func ParseYuanStringToCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("金额为空")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式非法: %s", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("金额为负: %s", s)
	}
	return int64(f*100 + 0.5), nil // 四舍五入到分
}
