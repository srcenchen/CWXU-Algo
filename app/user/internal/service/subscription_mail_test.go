package service

import (
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/user/internal/data/dal"
)

func TestExpiryReminderContent(t *testing.T) {
	u := dal.ExpiringUser{UserID: 1, Name: "小明", Email: "x@x.com", Tier: "pro",
		ExpireAt: time.Now().Add(3 * 24 * time.Hour), Reminded: 0}

	sub3, body3 := expiryReminderContent("GoAlgo", u, 3)
	if !strings.Contains(sub3, "3 天") || !strings.Contains(sub3, "Pro") {
		t.Fatalf("3 天提醒主题不符: %s", sub3)
	}
	if !strings.Contains(body3, "去续费") || !strings.Contains(body3, "/profile") {
		t.Fatalf("3 天提醒正文缺少续费入口")
	}

	sub1, _ := expiryReminderContent("GoAlgo", u, 1)
	if !strings.Contains(sub1, "1 天") {
		t.Fatalf("1 天提醒主题不符: %s", sub1)
	}
}

func TestThankYouMailContent(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	sub, body := thankYouMailContent("GoAlgo", "Pro", 3, "小明", &exp)
	if !strings.Contains(sub, "感谢") || !strings.Contains(sub, "Pro") {
		t.Fatalf("感谢信主题不符: %s", sub)
	}
	if !strings.Contains(body, "3 个月") || !strings.Contains(body, "开始使用") {
		t.Fatalf("感谢信正文不符")
	}
}
