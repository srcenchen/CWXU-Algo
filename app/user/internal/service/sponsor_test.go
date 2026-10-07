package service

import (
	"testing"

	"cwxu-algo/app/user/internal/data/model"
)

func TestSponsorGiftTier(t *testing.T) {
	cases := []struct {
		amount int64
		tier   string
	}{
		{100, "plus"},
		{999, "plus"},
		{1000, "pro"},
		{1001, "pro"},
		{5000, "pro"},
	}
	for _, c := range cases {
		if got := model.SponsorGiftTier(c.amount); got != c.tier {
			t.Fatalf("amount %d: got %s want %s", c.amount, got, c.tier)
		}
	}
}

func TestSponsorDonateAmountBounds(t *testing.T) {
	cases := []struct {
		amount int64
		ok     bool
	}{
		{-100, false},
		{0, false},
		{99, false},
		{100, true},
		{2000, true},
		{1000000, true},
		{1000001, false},
	}
	for _, c := range cases {
		if got := sponsorDonateAmount(c.amount); got != c.ok {
			t.Fatalf("amount %d: got %v want %v", c.amount, got, c.ok)
		}
	}
}

func TestSponsorNotifyURL(t *testing.T) {
	t.Setenv("PAYMENT_NOTIFY_URL", "https://example.com/api/payment/notify")
	got := sponsorNotifyURL()
	want := "https://example.com/api/payment/sponsor-notify"
	if got != want {
		t.Fatalf("sponsorNotifyURL = %q, want %q", got, want)
	}
}
