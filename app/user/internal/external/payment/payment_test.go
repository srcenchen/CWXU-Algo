package payment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 分 → 元字符串（创建订单金额参数基础）
func TestFormatCents(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{0, "0.00"},
		{2, "0.02"},
		{200, "2.00"},
		{700, "7.00"},
		{12345, "123.45"},
	}
	for _, c := range cases {
		if got := FormatCents(c.cents); got != c.want {
			t.Fatalf("FormatCents(%d) = %s, want %s", c.cents, got, c.want)
		}
	}
}

// 元字符串 → 分：支付FM 回调 amount 与订单金额比对
func TestParseYuanStringToCents(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"2.00", 200, false},
		{"7", 700, false},
		{"0.01", 1, false},
		{"123.45", 12345, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1.00", 0, true},
	}
	for _, c := range cases {
		got, err := ParseYuanStringToCents(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("ParseYuanStringToCents(%q) expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("ParseYuanStringToCents(%q) = (%d,%v), want (%d,nil)", c.in, got, err, c.want)
		}
	}
}

// 创建订单签名：md5(商户号 + 商户订单号 + 支付金额 + 异步通知地址 + 接入密钥)
// 文档约定：签名结果为 32 位小写。用已知参数对验证签名稳定性。
func TestCreateOrderSign(t *testing.T) {
	g := &PayFmGateway{
		merchantNo: "88888888",
		secret:     "testsecret",
		notifyURL:  "https://algo.zhiyuansofts.cn/v1/payment/notify",
	}
	orderNo := "N001"
	amount := FormatCents(200)
	got := signMD5(g.merchantNo + orderNo + amount + g.notifyURL + g.secret)
	if len(got) != 32 {
		t.Fatalf("sign length = %d, want 32", len(got))
	}
	// 小写 hex
	for _, ch := range got {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Fatalf("sign contains non-lowercase-hex char: %s", got)
		}
	}
	// 相同输入签名稳定
	if again := signMD5(g.merchantNo + orderNo + amount + g.notifyURL + g.secret); again != got {
		t.Fatalf("sign not stable: %s vs %s", got, again)
	}
}

// CreateOrder 成功：返回 data.payUrl
func TestCreateOrderSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/startOrder" {
			t.Fatalf("path = %s, want /startOrder", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("merchantNum") != "88888888" || q.Get("orderNo") != "N001" || q.Get("amount") != "2.00" {
			t.Fatalf("bad params: %v", q)
		}
		if q.Get("payType") == "" || q.Get("sign") == "" || q.Get("notifyUrl") == "" {
			t.Fatalf("missing params: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"msg":"success","code":200,"timestamp":1624553174921,"data":{"id":"1408103748495998976","payUrl":"http://pay.example/pay?orderNo=1408103748495998976"}}`))
	}))
	defer srv.Close()

	g, err := NewPayFmGateway(srv.URL, "88888888", "testsecret", "aloop", "https://algo.zhiyuansofts.cn/v1/payment/notify")
	if err != nil {
		t.Fatal(err)
	}
	payURL, err := g.CreateOrder(context.Background(), "N001", 200, "GoAlgo Plus 会员（30 天）")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if payURL != "http://pay.example/pay?orderNo=1408103748495998976" {
		t.Fatalf("payURL = %s", payURL)
	}
}

// CreateOrder 业务失败（签名不正确等）：返回 msg 错误
func TestCreateOrderRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"msg":"签名不正确","code":500,"timestamp":1584413776852,"data":null}`))
	}))
	defer srv.Close()

	g, err := NewPayFmGateway(srv.URL, "88888888", "testsecret", "aloop", "https://algo.zhiyuansofts.cn/v1/payment/notify")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateOrder(context.Background(), "N001", 200, ""); err == nil {
		t.Fatal("expected error for rejected order")
	}
}

// CreateOrder 未配置：报「支付未配置」
func TestNewPayFmGatewayUnconfigured(t *testing.T) {
	if _, err := NewPayFmGateway("", "88888888", "testsecret", "aloop", "https://example.com/notify"); err == nil {
		t.Fatal("expected error for empty api base")
	}
	if _, err := NewPayFmGateway("http://api.example", "", "testsecret", "aloop", "https://example.com/notify"); err == nil {
		t.Fatal("expected error for empty merchant")
	}
	if _, err := NewPayFmGateway("http://api.example", "88888888", "", "aloop", "https://example.com/notify"); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

// 回调验签：md5(state + merchantNum + orderNo + amount + 密钥)；state=1 成功
func TestParseNotificationSuccess(t *testing.T) {
	g, err := NewPayFmGateway("http://api.example", "88888888", "testsecret", "aloop", "https://algo.zhiyuansofts.cn/v1/payment/notify")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"state":           "1",
		"merchantNum":     "88888888",
		"orderNo":         "N001",
		"amount":          "2.00",
		"platformOrderNo": "633718715472560128",
		"payTime":         "2026-04-02 11:50:24",
		"sign":            signMD5("1" + "88888888" + "N001" + "2.00" + "testsecret"),
	}
	ntf, err := g.ParseNotification(values)
	if err != nil {
		t.Fatalf("ParseNotification: %v", err)
	}
	if ntf.OrderNo != "N001" || ntf.PlatformOrderNo != "633718715472560128" || ntf.Amount != "2.00" {
		t.Fatalf("unexpected notification: %+v", ntf)
	}
}

// 回调验签失败：签名错 / 商户号不匹配 / state≠1 / 缺参数
func TestParseNotificationRejected(t *testing.T) {
	g, err := NewPayFmGateway("http://api.example", "88888888", "testsecret", "aloop", "https://algo.zhiyuansofts.cn/v1/payment/notify")
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"state":           "1",
		"merchantNum":     "88888888",
		"orderNo":         "N001",
		"amount":          "2.00",
		"platformOrderNo": "633718715472560128",
		"sign":            signMD5("1" + "88888888" + "N001" + "2.00" + "testsecret"),
	}
	cases := []struct {
		name   string
		mutate func(m map[string]string) map[string]string
	}{
		{"坏签名", func(m map[string]string) map[string]string { m["sign"] = "0bad"; return m }},
		{"商户号不匹配", func(m map[string]string) map[string]string { m["merchantNum"] = "hacker"; return m }},
		{"state非1", func(m map[string]string) map[string]string { m["state"] = "0"; return m }},
		{"缺orderNo", func(m map[string]string) map[string]string { delete(m, "orderNo"); return m }},
		{"缺sign", func(m map[string]string) map[string]string { delete(m, "sign"); return m }},
	}
	for _, c := range cases {
		if _, err := g.ParseNotification(c.mutate(copyMap(base))); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
