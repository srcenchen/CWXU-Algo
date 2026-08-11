package dal

import (
	"testing"
	"time"

	"cwxu-algo/app/user/internal/data/model"
)

func at(now time.Time, days int) *time.Time {
	t := now.AddDate(0, 0, days)
	return &t
}

func daysBetween(a, b time.Time) int {
	diff := b.Sub(a)
	return int(diff.Hours()/24) + 1
}

func TestApplyPurchase(t *testing.T) {
	now := time.Now()

	t.Run("无订阅直接开通", func(t *testing.T) {
		u := &model.User{}
		applyPurchase(u, "plus", 30, "payfm", now)
		if u.SubTier != "plus" || u.SubExpireAt == nil {
			t.Fatalf("期望开通 plus，实际 tier=%s expire=%v", u.SubTier, u.SubExpireAt)
		}
		if u.SubPendingTier != "" || u.SubPendingDays != 0 {
			t.Fatalf("不应有排队档: %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
		if u.SubReminded != 0 {
			t.Fatalf("提醒标记应重置为0: %d", u.SubReminded)
		}
	})

	t.Run("同档续费叠加", func(t *testing.T) {
		u := &model.User{SubTier: "plus", SubExpireAt: at(now, 10), SubSource: "payfm"}
		before := *u.SubExpireAt
		applyPurchase(u, "plus", 30, "payfm", now)
		if u.SubTier != "plus" {
			t.Fatalf("tier=%s", u.SubTier)
		}
		want := before.AddDate(0, 0, 30)
		if daysBetween(now, *u.SubExpireAt) != daysBetween(now, want) {
			t.Fatalf("期望到期 %v 实际 %v", want, *u.SubExpireAt)
		}
	})

	t.Run("Pro有效买Plus排队", func(t *testing.T) {
		u := &model.User{SubTier: "pro", SubExpireAt: at(now, 20), SubSource: "payfm"}
		proExpire := *u.SubExpireAt
		applyPurchase(u, "plus", 30, "payfm", now)
		if u.SubTier != "pro" || u.SubExpireAt == nil || !u.SubExpireAt.Equal(proExpire) {
			t.Fatalf("Pro 档不应变化: tier=%s", u.SubTier)
		}
		if u.SubPendingTier != "plus" || u.SubPendingDays != 30 {
			t.Fatalf("期望排队 plus 30 天，实际 %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
		if u.SubPendingSource != "payfm" {
			t.Fatalf("pending source=%s", u.SubPendingSource)
		}
	})

	t.Run("Pro有效再买Plus叠加排队天数", func(t *testing.T) {
		u := &model.User{SubTier: "pro", SubExpireAt: at(now, 20), SubSource: "payfm",
			SubPendingTier: "plus", SubPendingDays: 30, SubPendingSource: "payfm"}
		applyPurchase(u, "plus", 30, "payfm", now)
		if u.SubPendingTier != "plus" || u.SubPendingDays != 60 {
			t.Fatalf("期望排队 60 天，实际 %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
	})

	t.Run("Plus升级Pro暂停Plus", func(t *testing.T) {
		u := &model.User{SubTier: "plus", SubExpireAt: at(now, 20), SubSource: "payfm"}
		applyPurchase(u, "pro", 30, "payfm", now)
		if u.SubTier != "pro" {
			t.Fatalf("升级后 tier=%s", u.SubTier)
		}
		// Plus 剩余 20 天暂停排队
		if u.SubPendingTier != "plus" || u.SubPendingDays != 20 {
			t.Fatalf("期望排队 plus 20 天，实际 %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
		if u.SubExpireAt == nil {
			t.Fatal("Pro 到期为空")
		}
		want := now.AddDate(0, 0, 30)
		if daysBetween(now, *u.SubExpireAt) != daysBetween(now, want) {
			t.Fatalf("Pro 到期 %v, want %v", *u.SubExpireAt, want)
		}
	})

	t.Run("Plus已过期再买直接开通并清空排队", func(t *testing.T) {
		u := &model.User{SubTier: "plus", SubExpireAt: at(now, -5), SubSource: "payfm",
			SubPendingTier: "plus", SubPendingDays: 10, SubPendingSource: "manager"}
		applyPurchase(u, "pro", 30, "payfm", now)
		if u.SubTier != "pro" {
			t.Fatalf("tier=%s", u.SubTier)
		}
		if u.SubPendingTier != "" || u.SubPendingDays != 0 {
			t.Fatalf("过期后开通应清空排队: %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
	})
}

func TestPromoteInPlace(t *testing.T) {
	now := time.Now()

	t.Run("未过期不晋升", func(t *testing.T) {
		u := &model.User{SubTier: "pro", SubExpireAt: at(now, 3), SubSource: "payfm",
			SubPendingTier: "plus", SubPendingDays: 30, SubPendingSource: "payfm"}
		if promoteInPlace(u, now) {
			t.Fatal("未过期不应晋升")
		}
		if u.SubTier != "pro" {
			t.Fatalf("tier=%s", u.SubTier)
		}
	})

	t.Run("过期晋升Plus并重置提醒", func(t *testing.T) {
		u := &model.User{SubTier: "pro", SubExpireAt: at(now, -1), SubSource: "payfm", SubReminded: 3,
			SubPendingTier: "plus", SubPendingDays: 30, SubPendingSource: "manager"}
		if !promoteInPlace(u, now) {
			t.Fatal("应晋升")
		}
		if u.SubTier != "plus" || u.SubSource != "manager" {
			t.Fatalf("晋升后 tier=%s source=%s", u.SubTier, u.SubSource)
		}
		if u.SubPendingTier != "" || u.SubPendingDays != 0 {
			t.Fatalf("晋升后应清空排队: %s/%d", u.SubPendingTier, u.SubPendingDays)
		}
		if u.SubReminded != 0 {
			t.Fatalf("晋升后提醒应重置: %d", u.SubReminded)
		}
		want := now.AddDate(0, 0, 30)
		if daysBetween(now, *u.SubExpireAt) != daysBetween(now, want) {
			t.Fatalf("晋升到期 %v, want %v", *u.SubExpireAt, want)
		}
	})

	t.Run("无排队不晋升", func(t *testing.T) {
		u := &model.User{SubTier: "pro", SubExpireAt: at(now, -1), SubSource: "payfm"}
		if promoteInPlace(u, now) {
			t.Fatal("无排队不应晋升")
		}
	})
}
