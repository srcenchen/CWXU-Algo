package data

import (
	"fmt"

	"cwxu-algo/app/common/utils/legacysecret"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type legacySecretDecryptor func(string) (string, error)

type LegacySiteConfig struct {
	ConfigEncryptionKey                                string
	SMTPHost, SMTPUsername, SMTPPassword, SMTPFrom     string
	SMTPPort                                           int
	AgentEndpoint, AgentModel, AgentSecret             string
	AiAnalyzeEndpoint, AiAnalyzeModel, AiAnalyzeSecret string
}

func migrateLegacySiteSecrets(db *gorm.DB, decrypt legacySecretDecryptor) error {
	if db == nil {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var row model.SiteConfig
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, 1).Error
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load site_configs for legacy secret migration: %w", err)
		}
		values := map[string]string{
			"smtp_password": row.SMTPPassword, "agent_secret": row.AgentSecret,
			"ai_analyze_secret": row.AiAnalyzeSecret, "upyun_password": row.UpyunPassword,
			"oj_luogu_password": row.OjLuoguPassword, "oj_qoj_password": row.OjQojPassword,
			"payfm_secret": row.PayFmSecret,
		}
		updates := make(map[string]interface{})
		for column, value := range values {
			if !legacysecret.IsEncrypted(value) {
				continue
			}
			plain, decryptErr := decrypt(value)
			if decryptErr != nil {
				return fmt.Errorf("migrate site_configs.%s from enc:v1: %w", column, decryptErr)
			}
			updates[column] = plain
		}
		if len(updates) == 0 {
			return nil
		}
		if err := tx.Model(&model.SiteConfig{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("atomically persist plaintext site secrets: %w", err)
		}
		return nil
	})
}

func migrateLegacySiteSecretsAtStartup(db *gorm.DB, legacy LegacySiteConfig) error {
	key := legacysecret.ResolveKey(legacy.ConfigEncryptionKey)
	return migrateLegacySiteSecrets(db, func(value string) (string, error) {
		return legacysecret.DecryptWithKey(value, key)
	})
}

func migrateLegacyBootstrapSiteConfig(db *gorm.DB, legacy LegacySiteConfig) error {
	if db == nil {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var row model.SiteConfig
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, 1).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil
			}
			return err
		}
		updates := map[string]interface{}{}
		fill := func(column, current, value string) {
			if current == "" && value != "" {
				updates[column] = value
			}
		}
		fill("smtp_host", row.SMTPHost, legacy.SMTPHost)
		if (row.SMTPPort == 0 || row.SMTPHost == "") && legacy.SMTPPort > 0 {
			updates["smtp_port"] = legacy.SMTPPort
		}
		fill("smtp_username", row.SMTPUsername, legacy.SMTPUsername)
		fill("smtp_password", row.SMTPPassword, legacy.SMTPPassword)
		fill("smtp_from", row.SMTPFrom, legacy.SMTPFrom)
		fill("agent_endpoint", row.AgentEndpoint, legacy.AgentEndpoint)
		fill("agent_model", row.AgentModel, legacy.AgentModel)
		fill("agent_secret", row.AgentSecret, legacy.AgentSecret)
		fill("ai_analyze_endpoint", row.AiAnalyzeEndpoint, legacy.AiAnalyzeEndpoint)
		fill("ai_analyze_model", row.AiAnalyzeModel, legacy.AiAnalyzeModel)
		fill("ai_analyze_secret", row.AiAnalyzeSecret, legacy.AiAnalyzeSecret)
		if len(updates) == 0 {
			return nil
		}
		return tx.Model(&model.SiteConfig{}).Where("id = ?", 1).Updates(updates).Error
	})
}
