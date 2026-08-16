package service

import (
	"fmt"
	"strings"

	site "cwxu-algo/api/user/v1/site"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/user/internal/biz/dormancy"
	"cwxu-algo/app/user/internal/data/model"
)

type secretEncryptor func(string) (string, error)

var validSections = map[string]bool{
	"basic": true, "email": true, "ai": true,
	"upyun": true, "oj": true, "payment": true, "backup": true, "all": true,
}

func normalizeSection(section string) string {
	sec := strings.ToLower(strings.TrimSpace(section))
	if sec == "" {
		return "all"
	}
	return sec
}

func isSection(section, target string) bool {
	return section == target || section == "all"
}

func buildSectionUpdates(section string, row *model.SiteConfig, req *site.UpdateConfigReq) (map[string]interface{}, error) {
	return buildSectionUpdatesWith(row, req, func(v string) (string, error) { return v, nil }, section)
}

func buildSectionUpdatesWith(row *model.SiteConfig, req *site.UpdateConfigReq, encrypt secretEncryptor, section string) (map[string]interface{}, error) {
	section = normalizeSection(section)
	if !validSections[section] {
		return nil, fmt.Errorf("未知配置分区: %s", section)
	}
	updates := map[string]interface{}{}
	applySecret := func(column string, draft string, clear bool) error {
		if clear {
			if isRealSecret(draft) {
				return fmt.Errorf("不能同时填写新密钥与清除密钥")
			}
			updates[column] = ""
			return nil
		}
		if isRealSecret(draft) {
			enc, err := encrypt(draft)
			if err != nil {
				return err
			}
			updates[column] = enc
		}
		return nil
	}

	if isSection(section, "basic") {
		title := strings.TrimSpace(req.SiteTitle)
		if title == "" {
			title = row.SiteTitle
			if title == "" {
				title = "GoAlgo"
			}
		}
		updates["site_title"] = title
		updates["site_logo"] = strings.TrimSpace(req.SiteLogo)
		updates["favicon"] = strings.TrimSpace(req.Favicon)
		updates["footer_icp"] = strings.TrimSpace(req.FooterIcp)
		if req.SetInactiveDays {
			updates["inactive_days"] = dormancy.ClampInactiveDays(int(req.InactiveDays))
		}
	}
	if isSection(section, "email") {
		port := int(req.SmtpPort)
		if port <= 0 {
			port = 465
		}
		updates["smtp_host"] = strings.TrimSpace(req.SmtpHost)
		updates["smtp_port"] = port
		updates["smtp_username"] = strings.TrimSpace(req.SmtpUsername)
		updates["smtp_from"] = strings.TrimSpace(req.SmtpFrom)
		updates["admin_notify_emails"] = strings.TrimSpace(req.AdminNotifyEmails)
		updates["ops_notify_emails"] = strings.TrimSpace(req.OpsNotifyEmails)
		updates["data_disk_path"] = strings.TrimSpace(req.DataDiskPath)
		if err := applySecret("smtp_password", req.SmtpPassword, req.ClearSmtpPassword); err != nil {
			return nil, err
		}
	}
	if isSection(section, "ai") {
		if err := sitesettings.ValidateEndpoint(req.AgentEndpoint); err != nil {
			return nil, err
		}
		if err := sitesettings.ValidateEndpoint(req.AiAnalyzeEndpoint); err != nil {
			return nil, err
		}
		updates["agent_endpoint"] = strings.TrimSpace(req.AgentEndpoint)
		updates["agent_model"] = strings.TrimSpace(req.AgentModel)
		updates["ai_analyze_endpoint"] = strings.TrimSpace(req.AiAnalyzeEndpoint)
		updates["ai_analyze_model"] = strings.TrimSpace(req.AiAnalyzeModel)
		if err := applySecret("agent_secret", req.AgentSecret, req.ClearAgentSecret); err != nil {
			return nil, err
		}
		if err := applySecret("ai_analyze_secret", req.AiAnalyzeSecret, req.ClearAiAnalyzeSecret); err != nil {
			return nil, err
		}
	}
	if isSection(section, "upyun") {
		updates["upyun_bucket"] = strings.TrimSpace(req.UpyunBucket)
		updates["upyun_operator"] = strings.TrimSpace(req.UpyunOperator)
		updates["upyun_domain"] = strings.TrimSpace(req.UpyunDomain)
		updates["upyun_scheme"] = strings.TrimSpace(req.UpyunScheme)
		if err := applySecret("upyun_password", req.UpyunPassword, req.ClearUpyunPassword); err != nil {
			return nil, err
		}
	}
	if isSection(section, "oj") {
		updates["oj_luogu_username"] = strings.TrimSpace(req.OjLuoguUsername)
		updates["oj_qoj_username"] = strings.TrimSpace(req.OjQojUsername)
		if err := applySecret("oj_luogu_password", req.OjLuoguPassword, req.ClearOjLuoguPassword); err != nil {
			return nil, err
		}
		if err := applySecret("oj_qoj_password", req.OjQojPassword, req.ClearOjQojPassword); err != nil {
			return nil, err
		}
	}
	if isSection(section, "payment") {
		updates["payfm_api_base"] = strings.TrimSpace(req.PayfmApiBase)
		updates["payfm_merchant_no"] = strings.TrimSpace(req.PayfmMerchantNo)
		updates["payfm_pay_type"] = strings.TrimSpace(req.PayfmPayType)
		if err := applySecret("payfm_secret", req.PayfmSecret, req.ClearPayfmSecret); err != nil {
			return nil, err
		}
	}
	if isSection(section, "backup") {
		backupTime := strings.TrimSpace(req.BackupTime)
		if backupTime == "" {
			backupTime = "02:00"
		}
		if err := sitesettings.ValidateBackupTime(backupTime); err != nil {
			return nil, err
		}
		updates["backup_enabled"] = req.BackupEnabled
		updates["backup_time"] = backupTime
		updates["backup_prefix"] = strings.Trim(strings.TrimSpace(req.BackupPrefix), "/")
	}
	return updates, nil
}
