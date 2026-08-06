// Package notify — site-admin broadcast + configurable ops email.
package notify

import (
	"fmt"
	"net/mail"
	"strings"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/sitesettings"

	"gorm.io/gorm"
)

// AdminNotif 站管站内信字段（UserID 由 helper 填）
type AdminNotif struct {
	Type      string
	Title     string
	Body      string
	ActorID   uint
	RefType   string
	RefID     uint
	ProblemID uint
	Payload   string
	// SkipUserID 不给此人发站内信（例如举报者本人是站管）
	SkipUserID uint
}

type siteAdminRow struct {
	ID    uint   `gorm:"column:id"`
	Email string `gorm:"column:email"`
}

// ListSiteAdminIDs 全部站管 user id
func ListSiteAdminIDs(db *gorm.DB) []uint {
	if db == nil {
		return nil
	}
	var rows []siteAdminRow
	_ = db.Table("users").Select("id, email").Where("is_site_admin = ?", true).Find(&rows).Error
	out := make([]uint, 0, len(rows))
	for _, r := range rows {
		if r.ID > 0 {
			out = append(out, r.ID)
		}
	}
	return out
}

// contentModeratorRows 内容审核受众：站管 ∪ 持有内容审核权限的站点角色成员（去重）。
// 资源审核员内置身份已下线，受众改由权限点推导。
func contentModeratorRows(db *gorm.DB) []siteAdminRow {
	if db == nil {
		return nil
	}
	var rows []siteAdminRow
	_ = db.Table("users").Select("id, email").Where("is_site_admin = ?", true).Find(&rows).Error
	seen := make(map[uint]bool, len(rows))
	for _, r := range rows {
		seen[r.ID] = true
	}
	var byRole []siteAdminRow
	_ = db.Table("users AS u").
		Select("DISTINCT u.id AS id, u.email AS email").
		Joins("JOIN user_roles ur ON ur.user_id = u.id AND ur.org_id = 0").
		Joins("JOIN roles r ON r.id = ur.role_id AND r.scope = ?", rbac.ScopeSite).
		Joins("JOIN role_permissions rp ON rp.role_id = r.id").
		Where("rp.perm_code IN ?", rbac.ContentPerms()).
		Find(&byRole).Error
	for _, r := range byRole {
		if r.ID > 0 && !seen[r.ID] {
			seen[r.ID] = true
			rows = append(rows, r)
		}
	}
	return rows
}

// ListContentModeratorIDs 内容审核受众 user id（去重）
func ListContentModeratorIDs(db *gorm.DB) []uint {
	rows := contentModeratorRows(db)
	out := make([]uint, 0, len(rows))
	for _, r := range rows {
		if r.ID > 0 {
			out = append(out, r.ID)
		}
	}
	return out
}

// NotifySiteAdmins 给内容审核受众写站内信（跳过 SkipUserID）。
// 命名保留兼容；实际受众为站管 ∪ 持内容审核权限的站点角色成员。
func NotifySiteAdmins(db *gorm.DB, n AdminNotif) {
	NotifyContentModerators(db, n)
}

// NotifyContentModerators 内容审核受众站内信
func NotifyContentModerators(db *gorm.DB, n AdminNotif) {
	if db == nil || strings.TrimSpace(n.Type) == "" {
		return
	}
	rows := contentModeratorRows(db)
	for _, adm := range rows {
		if adm.ID == 0 || adm.ID == n.SkipUserID {
			continue
		}
		_ = Create(db, Row{
			UserID:    adm.ID,
			Type:      n.Type,
			Title:     n.Title,
			Body:      n.Body,
			ActorID:   n.ActorID,
			RefType:   n.RefType,
			RefID:     n.RefID,
			ProblemID: n.ProblemID,
			Payload:   n.Payload,
		})
	}
}

// ParseEmailList 解析逗号/分号/空白/换行分隔的邮箱列表，去重校验
func ParseEmailList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// 统一分隔符
	replacer := strings.NewReplacer(",", " ", ";", " ", "\n", " ", "\r", " ", "\t", " ")
	parts := strings.Fields(replacer.Replace(raw))
	seen := map[string]struct{}{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		addr, err := mail.ParseAddress(p)
		if err != nil || addr == nil || addr.Address == "" {
			// 宽松：无尖括号的纯邮箱
			if !strings.Contains(p, "@") || strings.ContainsAny(p, " <>") {
				continue
			}
			addr = &mail.Address{Address: p}
		}
		key := strings.ToLower(strings.TrimSpace(addr.Address))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// ResolveAdminNotifyEmails 可配置收件人；空配置则 fallback 内容审核受众邮箱
func ResolveAdminNotifyEmails(db *gorm.DB) []string {
	if db == nil {
		return nil
	}
	var raw string
	_ = db.Table("site_configs").Select("admin_notify_emails").Where("id = ?", 1).Scan(&raw).Error
	if list := ParseEmailList(raw); len(list) > 0 {
		return list
	}
	rows := contentModeratorRows(db)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		e := strings.ToLower(strings.TrimSpace(r.Email))
		if e == "" || !strings.Contains(e, "@") {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// ResolveOpsNotifyEmails 运维告警收件人（站点设置「运维告警邮件接收人」）。
// 未配置返回 nil（运维告警不发邮件，避免误发骚扰）。
func ResolveOpsNotifyEmails(db *gorm.DB) []string {
	if db == nil {
		return nil
	}
	var raw string
	_ = db.Table("site_configs").Select("ops_notify_emails").Where("id = ?", 1).Scan(&raw).Error
	return ParseEmailList(raw)
}

// EmailOpsRecipientsRuntime 给运维告警收件人发邮件（Runtime 由调用方提供，支持 user 服务读 DB / core_data 读 Redis）。
// 未配置收件人或 SMTP 不可用则静默跳过。
func EmailOpsRecipientsRuntime(rt *sitesettings.Runtime, subject, html string) {
	if rt == nil {
		return
	}
	emails := ParseEmailList(rt.OpsNotifyEmails)
	if len(emails) == 0 {
		return
	}
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		return
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "[GoAlgo] 运维告警"
	}
	if !strings.HasPrefix(subject, "[GoAlgo]") {
		subject = "[GoAlgo] " + subject
	}
	for _, email := range emails {
		_ = sender.Send(email, subject, html)
	}
}

// EmailOpsRecipients 给运维告警收件人发邮件；未配置收件人或 SMTP 不可用则静默跳过
func EmailOpsRecipients(db *gorm.DB, subject, html string) {
	if db == nil {
		return
	}
	rt, err := sitesettings.LoadFromDB(db)
	if err != nil || rt == nil {
		return
	}
	EmailOpsRecipientsRuntime(rt, subject, html)
}

// EmailConfiguredRecipients 按站点配置（或站管 fallback）发邮件；SMTP 未配则静默跳过
func EmailConfiguredRecipients(db *gorm.DB, subject, html string) {
	if db == nil {
		return
	}
	emails := ResolveAdminNotifyEmails(db)
	if len(emails) == 0 {
		return
	}
	rt, err := sitesettings.LoadFromDB(db)
	if err != nil || rt == nil {
		return
	}
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		return
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "[GoAlgo] 通知"
	}
	if !strings.HasPrefix(subject, "[GoAlgo]") {
		subject = "[GoAlgo] " + subject
	}
	for _, email := range emails {
		_ = sender.Send(email, subject, html)
	}
}

// NotifySiteAdminsWithEmail 站内信给站管∪资源审核员 + 邮件给可配置收件人（审核/举报）
func NotifySiteAdminsWithEmail(db *gorm.DB, n AdminNotif, emailSubject, emailHTML string) {
	NotifyContentModeratorsWithEmail(db, n, emailSubject, emailHTML)
}

// NotifyContentModeratorsWithEmail 站内信 + 配置/fallback 邮件
func NotifyContentModeratorsWithEmail(db *gorm.DB, n AdminNotif, emailSubject, emailHTML string) {
	NotifyContentModerators(db, n)
	if strings.TrimSpace(emailHTML) == "" {
		emailHTML = fmt.Sprintf("<p>%s</p>", n.Body)
	}
	subj := emailSubject
	if strings.TrimSpace(subj) == "" {
		subj = n.Title
	}
	EmailConfiguredRecipients(db, subj, emailHTML)
}

// LookupUserEmail 查 users.email；无邮箱返回空串。
func LookupUserEmail(db *gorm.DB, userID uint) string {
	if db == nil || userID == 0 {
		return ""
	}
	var email string
	_ = db.Table("users").Select("email").Where("id = ?", userID).Scan(&email).Error
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return ""
	}
	return email
}

// EmailUser 给指定用户发 HTML 邮件（读 users.email + 站点 SMTP）。
// 无邮箱 / SMTP 未配 / 发送失败均静默跳过（调用方可再打日志）。
// 返回 true 表示已成功调用 Send。
func EmailUser(db *gorm.DB, userID uint, subject, html string) bool {
	if db == nil || userID == 0 {
		return false
	}
	to := LookupUserEmail(db, userID)
	if to == "" {
		return false
	}
	rt, err := sitesettings.LoadFromDB(db)
	if err != nil || rt == nil {
		return false
	}
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		return false
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "[GoAlgo] 通知"
	}
	if !strings.HasPrefix(subject, "[GoAlgo]") {
		subject = "[GoAlgo] " + subject
	}
	if strings.TrimSpace(html) == "" {
		html = "<p></p>"
	}
	if err := sender.Send(to, subject, html); err != nil {
		return false
	}
	return true
}
