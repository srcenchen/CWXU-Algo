// Package payment 支付网关抽象（支付宝实现；参考 GuadArt 订单状态机）。
package payment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/smartwalle/alipay/v3"
)

// PaymentNotification 支付平台回调通知（已验签）
type PaymentNotification struct {
	// OrderNo 商户订单号
	OrderNo string
	// PlatformOrderNo 支付宝交易号
	PlatformOrderNo string
	// TradeStatus 交易状态：TRADE_SUCCESS / TRADE_FINISHED 才算成功
	TradeStatus string
	// Amount 订单金额（元字符串，如 2.00）
	Amount string
	// PayTime 支付时间
	PayTime time.Time
}

// PaymentGateway 支付网关接口（可替换/可测试）
type PaymentGateway interface {
	// PreCreate 预下单，返回二维码内容（alipay.trade.precreate）
	PreCreate(ctx context.Context, orderNo string, amountCents int64, subject string) (qrCode string, err error)
	// ParseNotification 解析并验签支付回调（values 为表单参数）；验签失败返回错误
	ParseNotification(values map[string]string) (*PaymentNotification, error)
}

// TradeStatusSuccess 回调中视为支付成功的状态
func TradeStatusSuccess(status string) bool {
	return status == string(alipay.TradeStatusSuccess) || status == string(alipay.TradeStatusFinished)
}

// alipay.TradeStatus 常量：
//   - alipay.TradeStatusSuccess  = TRADE_SUCCESS（支付成功）
//   - alipay.TradeStatusFinished = TRADE_FINISHED（交易完成且不可退款）

// AlipayGateway 支付宝实现（appID/私钥/公钥来自站点配置）
type AlipayGateway struct {
	client    *alipay.Client
	notifyURL string // 异步回调地址（precreate 时带给支付宝）
}

// NewAlipayGateway 创建支付宝网关；appID/私钥为空返回错误（调用方按「支付未配置」处理）。
// 支付宝公钥为空时不校验回调签名（生产必须配置；测试/沙箱建议配置）。
func NewAlipayGateway(appID, privateKey, publicKey string, sandbox bool, notifyURL string) (*AlipayGateway, error) {
	appID = strings.TrimSpace(appID)
	privateKey = strings.TrimSpace(privateKey)
	if appID == "" || privateKey == "" {
		return nil, fmt.Errorf("支付宝未配置（缺少 app_id 或私钥）")
	}
	client, err := alipay.New(appID, privateKey, !sandbox)
	if err != nil {
		return nil, fmt.Errorf("初始化支付宝客户端失败: %w", err)
	}
	if pk := strings.TrimSpace(publicKey); pk != "" {
		if err := client.LoadAliPayPublicKey(pk); err != nil {
			return nil, fmt.Errorf("加载支付宝公钥失败: %w", err)
		}
	}
	return &AlipayGateway{client: client, notifyURL: strings.TrimSpace(notifyURL)}, nil
}

// PreCreate 预下单；返回二维码内容（前端渲染二维码）
func (g *AlipayGateway) PreCreate(ctx context.Context, orderNo string, amountCents int64, subject string) (string, error) {
	if g == nil || g.client == nil {
		return "", fmt.Errorf("支付网关未初始化")
	}
	rsp, err := g.client.TradePreCreate(ctx, alipay.TradePreCreate{
		Trade: alipay.Trade{
			Subject:     subject,
			OutTradeNo:  orderNo,
			TotalAmount: FormatCents(amountCents),
			ProductCode: "FACE_TO_FACE_PAYMENT",
			NotifyURL:   g.notifyURL,
		},
	})
	if err != nil {
		return "", fmt.Errorf("支付宝预下单失败: %w", err)
	}
	if rsp.Code != alipay.CodeSuccess {
		// 支付宝业务错误（如 app_id 无权限 / 产品未签约）
		return "", fmt.Errorf("支付宝预下单被拒: %s %s", rsp.Code, rsp.Msg)
	}
	if strings.TrimSpace(rsp.QRCode) == "" {
		return "", fmt.Errorf("支付宝预下单未返回二维码")
	}
	return rsp.QRCode, nil
}

// ParseNotification 解析并验签支付宝异步通知（x-www-form-urlencoded 表单参数）。
// 返回的 TradeStatus 原样；是否成功由调用方按 TradeStatusSuccess 判断。
func (g *AlipayGateway) ParseNotification(values map[string]string) (*PaymentNotification, error) {
	if g == nil || g.client == nil {
		return nil, fmt.Errorf("支付网关未初始化")
	}
	form := make(map[string][]string, len(values))
	for k, v := range values {
		form[k] = []string{v}
	}
	ntf, err := g.client.DecodeNotification(context.Background(), form)
	if err != nil {
		return nil, fmt.Errorf("支付宝回调验签失败: %w", err)
	}
	return &PaymentNotification{
		OrderNo:         ntf.OutTradeNo,
		PlatformOrderNo: ntf.TradeNo,
		TradeStatus:     string(ntf.TradeStatus),
		Amount:          ntf.TotalAmount,
		PayTime:         time.Now(),
	}, nil
}

// FormatCents 分 → 元字符串（两位小数，如 200 → "2.00"）
func FormatCents(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
}

// ParseYuanStringToCents 元字符串 → 分（如 "2.00" → 200）；非法返回错误。
// 回调金额比对用：支付宝 total_amount 为元字符串。
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
