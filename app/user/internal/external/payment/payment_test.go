package payment

import (
	"testing"
)

// 分 → 元字符串（回调金额比对基础）
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

// 元字符串 → 分：支付宝回调 total_amount 与订单金额比对
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

// 回调成功状态判定：TRADE_SUCCESS / TRADE_FINISHED 才成功
func TestTradeStatusSuccess(t *testing.T) {
	if !TradeStatusSuccess("TRADE_SUCCESS") {
		t.Fatal("TRADE_SUCCESS should be success")
	}
	if !TradeStatusSuccess("TRADE_FINISHED") {
		t.Fatal("TRADE_FINISHED should be success")
	}
	if TradeStatusSuccess("WAIT_BUYER_PAY") || TradeStatusSuccess("TRADE_CLOSED") {
		t.Fatal("non-success statuses must not pass")
	}
}
