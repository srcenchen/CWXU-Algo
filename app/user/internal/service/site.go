package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/api/user/v1/site"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/ojlogin"
	"cwxu-algo/app/common/opsmetrics"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/clientip"
	secretutil "cwxu-algo/app/common/utils/secret"
	"cwxu-algo/app/user/internal/biz/dormancy"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
)

type SiteService struct {
	site.UnimplementedSiteServer
	data     *data.Data
	yamlSMTP *conf.SMTP
}

func NewSiteService(d *data.Data, smtp *conf.SMTP) *SiteService {
	return &SiteService{data: d, yamlSMTP: smtp}
}

func (s *SiteService) ensureRow(ctx context.Context) (*model.SiteConfig, error) {
	var row model.SiteConfig
	err := s.data.DB.WithContext(ctx).First(&row, 1).Error
	if err == nil {
		s.protectStoredSecrets(ctx, &row)
		if s.backfillFromYaml(ctx, &row) {
			s.publish(ctx, &row)
		}
		return &row, nil
	}
	row = model.SiteConfig{
		ID:        1,
		SiteTitle: "GoAlgo",
		FooterIcp: "苏ICP备2025217901号",
		SMTPPort:  465,
	}
	if s.yamlSMTP != nil {
		row.SMTPHost = s.yamlSMTP.Host
		row.SMTPPort = int(s.yamlSMTP.Port)
		if row.SMTPPort <= 0 {
			row.SMTPPort = 465
		}
		row.SMTPUsername = s.yamlSMTP.Username
		if encrypted, encryptErr := secretutil.Encrypt(s.yamlSMTP.Password); encryptErr == nil {
			row.SMTPPassword = encrypted
		}
		row.SMTPFrom = s.yamlSMTP.From
	}
	if e := s.data.DB.WithContext(ctx).Create(&row).Error; e != nil {
		if e2 := s.data.DB.WithContext(ctx).First(&row, 1).Error; e2 == nil {
			return &row, nil
		}
		return nil, e
	}
	s.publish(ctx, &row)
	return &row, nil
}

func (s *SiteService) backfillFromYaml(ctx context.Context, row *model.SiteConfig) bool {
	if s.yamlSMTP == nil || row == nil {
		return false
	}
	if strings.TrimSpace(row.SMTPHost) != "" {
		return false
	}
	if strings.TrimSpace(s.yamlSMTP.Host) == "" {
		return false
	}
	port := int(s.yamlSMTP.Port)
	if port <= 0 {
		port = 465
	}
	encryptedPassword, err := secretutil.Encrypt(s.yamlSMTP.Password)
	if err != nil {
		log.Errorf("encrypt smtp configuration: %v", err)
		return false
	}
	updates := map[string]interface{}{
		"smtp_host":     s.yamlSMTP.Host,
		"smtp_port":     port,
		"smtp_username": s.yamlSMTP.Username,
		"smtp_password": encryptedPassword,
		"smtp_from":     s.yamlSMTP.From,
	}
	if e := s.data.DB.WithContext(ctx).Model(&model.SiteConfig{}).Where("id = ?", 1).Updates(updates).Error; e != nil {
		return false
	}
	row.SMTPHost = s.yamlSMTP.Host
	row.SMTPPort = port
	row.SMTPUsername = s.yamlSMTP.Username
	row.SMTPPassword = encryptedPassword
	row.SMTPFrom = s.yamlSMTP.From
	return true
}

// protectStoredSecrets performs a rolling, idempotent migration from legacy
// plaintext values once the deployment encryption key is available.
func (s *SiteService) protectStoredSecrets(ctx context.Context, row *model.SiteConfig) {
	if row == nil || !secretutil.Configured() {
		return
	}
	values := map[string]*string{
		"smtp_password":     &row.SMTPPassword,
		"agent_secret":      &row.AgentSecret,
		"ai_analyze_secret": &row.AiAnalyzeSecret,
		"upyun_password":    &row.UpyunPassword,
		"oj_luogu_password": &row.OjLuoguPassword,
		"oj_qoj_password":   &row.OjQojPassword,
		"payfm_secret":      &row.PayFmSecret,
	}
	updates := make(map[string]interface{})
	for column, value := range values {
		encrypted, err := secretutil.Encrypt(*value)
		if err != nil {
			log.Errorf("encrypt stored site secret %s: %v", column, err)
			continue
		}
		if encrypted != *value {
			*value = encrypted
			updates[column] = encrypted
		}
	}
	if len(updates) > 0 {
		if err := s.data.DB.WithContext(ctx).Model(&model.SiteConfig{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			log.Errorf("persist encrypted site secrets: %v", err)
		}
	}
}

func (s *SiteService) publish(ctx context.Context, row *model.SiteConfig) {
	rt := rowToRuntime(row)
	if err := sitesettings.PublishRedis(ctx, s.data.RDB, rt); err != nil {
		log.Warnf("publish site settings redis: %v", err)
	}
}

func rowToRuntime(row *model.SiteConfig) *sitesettings.Runtime {
	if row == nil {
		return &sitesettings.Runtime{SiteTitle: "GoAlgo"}
	}
	port := row.SMTPPort
	if port <= 0 {
		port = 465
	}
	title := strings.TrimSpace(row.SiteTitle)
	if title == "" {
		title = "GoAlgo"
	}
	decrypt := func(value string) string {
		plain, err := secretutil.Decrypt(value)
		if err != nil {
			log.Errorf("decrypt site secret: %v", err)
			return ""
		}
		return plain
	}
	return &sitesettings.Runtime{
		SiteTitle:         title,
		SMTPHost:          strings.TrimSpace(row.SMTPHost),
		SMTPPort:          port,
		SMTPUsername:      strings.TrimSpace(row.SMTPUsername),
		SMTPPassword:      decrypt(row.SMTPPassword),
		SMTPFrom:          strings.TrimSpace(row.SMTPFrom),
		AgentModel:        strings.TrimSpace(row.AgentModel),
		AgentEndpoint:     strings.TrimSpace(row.AgentEndpoint),
		AgentSecret:       decrypt(row.AgentSecret),
		ConfigVersion:     row.ConfigVersion,
		AiAnalyzeEndpoint: strings.TrimSpace(row.AiAnalyzeEndpoint),
		AiAnalyzeModel:    strings.TrimSpace(row.AiAnalyzeModel),
		AiAnalyzeSecret:   decrypt(row.AiAnalyzeSecret),
		OjLuoguUsername:   strings.TrimSpace(row.OjLuoguUsername),
		OjLuoguPassword:   decrypt(row.OjLuoguPassword),
		OjQojUsername:     strings.TrimSpace(row.OjQojUsername),
		OjQojPassword:     decrypt(row.OjQojPassword),
		OpsNotifyEmails:   strings.TrimSpace(row.OpsNotifyEmails),
		DataDiskPath:      strings.TrimSpace(row.DataDiskPath),
		PayFmApiBase:    strings.TrimSpace(row.PayFmApiBase),
		PayFmMerchantNo: strings.TrimSpace(row.PayFmMerchantNo),
		PayFmSecret:     decrypt(row.PayFmSecret),
		PayFmPayType:    strings.TrimSpace(row.PayFmPayType),
	}
}

func (s *SiteService) effectiveRuntime(row *model.SiteConfig) *sitesettings.Runtime {
	return rowToRuntime(row).MergeFallback(s.yamlSMTP, nil, nil)
}

func (s *SiteService) GetConfig(ctx context.Context, _ *site.GetConfigReq) (*site.GetConfigRes, error) {
	row, err := s.ensureRow(ctx)
	if err != nil {
		return nil, errors.InternalServer("site config", "服务暂时不可用")
	}
	return &site.GetConfigRes{
		Code:            0,
		Message:         "success",
		SiteTitle:       row.SiteTitle,
		SiteLogo:        row.SiteLogo,
		Favicon:         row.Favicon,
		FooterIcp:       row.FooterIcp,
		PayfmConfigured: strings.TrimSpace(row.PayFmApiBase) != "" && strings.TrimSpace(row.PayFmMerchantNo) != "" && strings.TrimSpace(decryptSiteSecret(row.PayFmSecret)) != "",
	}, nil
}

func (s *SiteService) GetAdminConfig(ctx context.Context, _ *site.GetAdminConfigReq) (*site.GetAdminConfigRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigRead) {
		return &site.GetAdminConfigRes{Code: 1, Message: "需要查看站点配置权限"}, nil
	}
	row, err := s.ensureRow(ctx)
	if err != nil {
		return nil, errors.InternalServer("site config", "服务暂时不可用")
	}
	rt := s.effectiveRuntime(row)
	// 实时状态：agent / ai / smtp 以 Redis 为准（各服务实际调用后回写），OJ 优先 Redis（手动验证也计入）
	agentSt := sitesettings.GetServiceStatus(ctx, s.data.RDB, sitesettings.ServiceAgent)
	aiSt := sitesettings.GetServiceStatus(ctx, s.data.RDB, sitesettings.ServiceAiAnaly)
	smtpSt := sitesettings.GetServiceStatus(ctx, s.data.RDB, sitesettings.ServiceSmtp)
	lgSt := sitesettings.GetServiceStatus(ctx, s.data.RDB, sitesettings.ServiceLuoGu)
	qojSt := sitesettings.GetServiceStatus(ctx, s.data.RDB, sitesettings.ServiceQOJ)
	return &site.GetAdminConfigRes{
		Code:                   0,
		Message:                "success",
		SiteTitle:              row.SiteTitle,
		SiteLogo:               row.SiteLogo,
		Favicon:                row.Favicon,
		SmtpHost:               rt.SMTPHost,
		SmtpPort:               int32(rt.SMTPPort),
		SmtpUsername:           rt.SMTPUsername,
		SmtpPasswordMasked:     sitesettings.MaskSecret(rt.SMTPPassword),
		SmtpPasswordSet:        strings.TrimSpace(rt.SMTPPassword) != "",
		SmtpFrom:               rt.SMTPFrom,
		AgentModel:             rt.AgentModel,
		AgentEndpoint:          rt.AgentEndpoint,
		AgentSecretMasked:      sitesettings.MaskSecret(rt.AgentSecret),
		AgentSecretSet:         strings.TrimSpace(rt.AgentSecret) != "",
		ConfigVersion:          rt.ConfigVersion,
		AiAnalyzeEndpoint:      rt.AiAnalyzeEndpoint,
		AiAnalyzeModel:         rt.AiAnalyzeModel,
		AiAnalyzeSecretMasked:  sitesettings.MaskSecret(rt.AiAnalyzeSecret),
		AiAnalyzeSecretSet:     strings.TrimSpace(rt.AiAnalyzeSecret) != "",
		FooterIcp:              row.FooterIcp,
		InactiveDays:           int32(dormancy.ClampInactiveDays(row.InactiveDays)),
		AdminNotifyEmails:      row.AdminNotifyEmails,
		UpyunBucket:            strings.TrimSpace(row.UpyunBucket),
		UpyunOperator:          strings.TrimSpace(row.UpyunOperator),
		UpyunPasswordMasked:    sitesettings.MaskSecret(decryptSiteSecret(row.UpyunPassword)),
		UpyunPasswordSet:       strings.TrimSpace(decryptSiteSecret(row.UpyunPassword)) != "",
		UpyunDomain:            strings.TrimSpace(row.UpyunDomain),
		UpyunScheme:            strings.TrimSpace(row.UpyunScheme),
		OjLuoguUsername:        strings.TrimSpace(row.OjLuoguUsername),
		OjLuoguPasswordMasked:  sitesettings.MaskSecret(decryptSiteSecret(row.OjLuoguPassword)),
		OjLuoguPasswordSet:     strings.TrimSpace(decryptSiteSecret(row.OjLuoguPassword)) != "",
		OjQojUsername:          strings.TrimSpace(row.OjQojUsername),
		OjQojPasswordMasked:    sitesettings.MaskSecret(decryptSiteSecret(row.OjQojPassword)),
		OjQojPasswordSet:       strings.TrimSpace(decryptSiteSecret(row.OjQojPassword)) != "",
		OjLuoguStatus:          lgSt.Status,
		OjLuoguStatusAt:        lgSt.At,
		OjLuoguErrMsg:          lgSt.ErrMsg,
		OjQojStatus:            qojSt.Status,
		OjQojStatusAt:          qojSt.At,
		OjQojErrMsg:            qojSt.ErrMsg,
		AgentStatus:            agentSt.Status,
		AgentStatusAt:          agentSt.At,
		AgentErrMsg:            agentSt.ErrMsg,
		AiAnalyzeStatus:        aiSt.Status,
		AiAnalyzeStatusAt:      aiSt.At,
		AiAnalyzeErrMsg:        aiSt.ErrMsg,
		SmtpStatus:             smtpSt.Status,
		SmtpStatusAt:           smtpSt.At,
		SmtpErrMsg:             smtpSt.ErrMsg,
		OpsNotifyEmails:        row.OpsNotifyEmails,
		DataDiskPath:           strings.TrimSpace(row.DataDiskPath),
		PayfmApiBase:      strings.TrimSpace(row.PayFmApiBase),
		PayfmMerchantNo:   strings.TrimSpace(row.PayFmMerchantNo),
		PayfmSecretMasked: sitesettings.MaskSecret(decryptSiteSecret(row.PayFmSecret)),
		PayfmSecretSet:    strings.TrimSpace(decryptSiteSecret(row.PayFmSecret)) != "",
		PayfmPayType:      strings.TrimSpace(row.PayFmPayType),
		PayfmNotifyUrl:    notifyURL(),
	}, nil
}

func decryptSiteSecret(value string) string {
	plain, err := secretutil.Decrypt(value)
	if err != nil {
		return ""
	}
	return plain
}

func encryptSiteSecret(value string) (string, error) {
	enc, err := secretutil.Encrypt(value)
	if err != nil {
		return "", err
	}
	return enc, nil
}

func (s *SiteService) UpdateConfig(ctx context.Context, req *site.UpdateConfigReq) (*site.UpdateConfigRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigWrite) {
		return &site.UpdateConfigRes{Code: 1, Message: "需要修改站点配置权限"}, nil
	}
	row, err := s.ensureRow(ctx)
	if err != nil {
		return nil, errors.InternalServer("site config", "服务暂时不可用")
	}

	section := normalizeSection(req.GetSection())
	if !validSections[section] {
		return &site.UpdateConfigRes{Code: 1, Message: "未知配置分区"}, nil
	}

	updates, err := buildSectionUpdates(section, row, req)
	if err != nil {
		return &site.UpdateConfigRes{Code: 1, Message: err.Error()}, nil
	}

	// 版本乐观锁：>0 时校验，不匹配返回冲突
	if req.GetExpectedConfigVersion() > 0 && row.ConfigVersion != req.GetExpectedConfigVersion() {
		return &site.UpdateConfigRes{Code: 409, Message: "配置已被其他管理员修改，请刷新后重试"}, nil
	}
	updates["config_version"] = row.ConfigVersion + 1

	// 状态重置：Agent 配置实际变化时重置 agent；AI 分析配置变化时重置 ai_analyze
	if isSection(section, "ai") {
		agentChanged := strings.TrimSpace(req.AgentEndpoint) != strings.TrimSpace(row.AgentEndpoint) ||
			strings.TrimSpace(req.AgentModel) != strings.TrimSpace(row.AgentModel) ||
			req.ClearAgentSecret || isRealSecret(req.AgentSecret)
		if agentChanged {
			updates["agent_status"] = sitesettings.StatusUnchecked
			updates["agent_status_at"] = 0
			updates["agent_err_msg"] = ""
		}
		aiChanged := strings.TrimSpace(req.AiAnalyzeEndpoint) != strings.TrimSpace(row.AiAnalyzeEndpoint) ||
			strings.TrimSpace(req.AiAnalyzeModel) != strings.TrimSpace(row.AiAnalyzeModel) ||
			req.ClearAiAnalyzeSecret || isRealSecret(req.AiAnalyzeSecret)
		if aiChanged {
			updates["ai_analyze_status"] = sitesettings.StatusUnchecked
			updates["ai_analyze_status_at"] = 0
			updates["ai_analyze_err_msg"] = ""
		}
	}

	if e := s.data.DB.WithContext(ctx).Model(&model.SiteConfig{}).Where("id = ?", 1).Updates(updates).Error; e != nil {
		return nil, errors.InternalServer("site config update", e.Error())
	}
	if e := s.data.DB.WithContext(ctx).First(row, 1).Error; e != nil {
		return nil, errors.InternalServer("site config", e.Error())
	}
	s.publish(ctx, row)

	return &site.UpdateConfigRes{
		Code:      0,
		Message:   "success",
		SiteTitle: row.SiteTitle,
		SiteLogo:  row.SiteLogo,
		Favicon:   row.Favicon,
		FooterIcp: row.FooterIcp,
	}, nil
}

func isRealSecret(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s == "••••••••" || s == "********" || s == "****" {
		return false
	}
	return true
}

// verifyOjCredentialsBeforeSave 在写库前实测 OJ 登录；失败返回用户可读错误文案。
func verifyOjCredentialsBeforeSave(row *model.SiteConfig, req *site.UpdateConfigReq) string {
	if req == nil {
		return ""
	}
	// 洛谷
	lgUser := strings.TrimSpace(req.OjLuoguUsername)
	if lgUser != "" {
		if req.ClearOjLuoguPassword {
			return "洛谷账号已填写，不能清空密码"
		}
		lgPass := ""
		if isRealSecret(req.OjLuoguPassword) {
			lgPass = strings.TrimSpace(req.OjLuoguPassword)
		} else {
			lgPass = decryptSiteSecret(row.OjLuoguPassword)
		}
		if strings.TrimSpace(lgPass) == "" {
			return "请填写洛谷密码"
		}
		if err := ojlogin.VerifyLuogu(lgUser, lgPass); err != nil {
			log.Warnf("oj login verify luogu: %v", err)
			return "洛谷账号验证失败：" + err.Error()
		}
	}
	// QOJ
	qojUser := strings.TrimSpace(req.OjQojUsername)
	if qojUser != "" {
		if req.ClearOjQojPassword {
			return "QOJ 账号已填写，不能清空密码"
		}
		qojPass := ""
		if isRealSecret(req.OjQojPassword) {
			qojPass = strings.TrimSpace(req.OjQojPassword)
		} else {
			qojPass = decryptSiteSecret(row.OjQojPassword)
		}
		if strings.TrimSpace(qojPass) == "" {
			return "请填写 QOJ 密码"
		}
		if err := ojlogin.VerifyQOJ(qojUser, qojPass); err != nil {
			log.Warnf("oj login verify qoj: %v", err)
			return "QOJ 账号验证失败：" + err.Error()
		}
	}
	return ""
}

func (s *SiteService) TestEmail(ctx context.Context, req *site.TestEmailReq) (*site.TestEmailRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigWrite) {
		return &site.TestEmailRes{Code: 1, Message: "需要修改站点配置权限", Success: false}, nil
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		return &site.TestEmailRes{Code: 1, Message: "请填写收件人邮箱", Success: false}, nil
	}

	row, err := s.ensureRow(ctx)
	if err != nil {
		return nil, errors.InternalServer("site config", "服务暂时不可用")
	}
	rt := s.effectiveRuntime(row)

	host := strings.TrimSpace(req.SmtpHost)
	if host == "" {
		host = rt.SMTPHost
	}
	port := int(req.SmtpPort)
	if port <= 0 {
		port = rt.SMTPPort
	}
	if port <= 0 {
		port = 465
	}
	user := strings.TrimSpace(req.SmtpUsername)
	if user == "" {
		user = rt.SMTPUsername
	}
	pass := req.SmtpPassword
	if !isRealSecret(pass) {
		pass = rt.SMTPPassword
	}
	from := strings.TrimSpace(req.SmtpFrom)
	if from == "" {
		from = rt.SMTPFrom
	}
	if from == "" {
		from = user
	}

	sender := mail.NewSender(&conf.SMTP{
		Host: host, Port: int32(port), Username: user, Password: pass, From: from,
	})
	if !sender.Configured() {
		return &site.TestEmailRes{Code: 1, Message: "SMTP 未配置完整（需要主机）", Success: false}, nil
	}
	title := rt.SiteTitle
	if title == "" {
		title = "GoAlgo"
	}
	subject := fmt.Sprintf("【%s】邮件配置测试", title)
	inner := fmt.Sprintf(`
<p style="margin:0 0 12px;">你好，</p>
<p style="margin:0 0 12px;">这是一封来自 <strong>%s</strong> 的测试邮件。</p>
<p style="margin:0 0 12px;">若你收到此信，说明 SMTP 配置可用，邮件 HTML 布局正常。</p>
<p style="margin:0;font-size:12px;color:#737373;">发送时间：%s</p>
`, mail.Escape(title), time.Now().Format("2006-01-02 15:04:05"))
	body := mail.Wrap(mail.LayoutOpts{Brand: title, Title: "邮件配置测试", Preheader: "SMTP 测试邮件"}, inner)
	if err := sender.Send(to, subject, body); err != nil {
		log.Errorf("test email: %v", err)
		return &site.TestEmailRes{Code: 1, Message: "发送失败：" + err.Error(), Success: false}, nil
	}
	return &site.TestEmailRes{Code: 0, Message: "测试邮件已发送", Success: true}, nil
}

func (s *SiteService) VisitPing(ctx context.Context, req *site.VisitPingReq) (*site.VisitPingRes, error) {
	path := "/"
	visitorID := ""
	if req != nil {
		path = req.Path
		visitorID = req.VisitorId
	}
	var userID uint
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		userID = pd.UserID
	}
	// 已登录：节流更新 last_login_at，避免长会话被误判休眠（不触发全量爬）
	if userID > 0 && s.data.RDB != nil {
		key := fmt.Sprintf("active:touch:%d", userID)
		ok, err := s.data.RDB.SetNX(ctx, key, "1", time.Hour).Result()
		if err == nil && ok {
			now := time.Now()
			if e := s.data.DB.WithContext(ctx).Model(&model.User{}).
				Where("id = ?", userID).Update("last_login_at", now).Error; e != nil {
				log.Warnf("visit touch last_login user=%d: %v", userID, e)
			}
		}
	}
	clientIP := clientip.FromContext(ctx)
	rec, err := s.data.RecordVisit(ctx, userID, visitorID, clientIP, path)
	if err != nil {
		log.Warnf("visit ping: %v", err)
		return &site.VisitPingRes{Code: 0, Message: "ok", Counted: false}, nil
	}
	counted := false
	if rec != nil {
		counted = rec.Counted
	}
	return &site.VisitPingRes{Code: 0, Message: "ok", Counted: counted}, nil
}

func (s *SiteService) VerifyOjCredential(ctx context.Context, req *site.VerifyOjCredentialReq) (*site.VerifyOjCredentialRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigWrite) {
		return &site.VerifyOjCredentialRes{Code: 1, Message: "需要修改站点配置权限"}, nil
	}
	row, err := s.ensureRow(ctx)
	if err != nil {
		return nil, errors.InternalServer("site config", "服务暂时不可用")
	}

	plat := strings.TrimSpace(req.GetPlatform())
	username := strings.TrimSpace(req.GetUsername())
	password := req.GetPassword()

	report := func(status, errMsg string) {
		svc := ""
		switch plat {
		case "LuoGu":
			svc = sitesettings.ServiceLuoGu
		case "QOJ":
			svc = sitesettings.ServiceQOJ
		}
		if svc != "" && s.data != nil && s.data.RDB != nil {
			sitesettings.SetServiceStatus(ctx, s.data.RDB, svc, status, errMsg)
		}
	}

	switch plat {
	case "LuoGu":
		if username == "" {
			username = strings.TrimSpace(row.OjLuoguUsername)
		}
		if !isRealSecret(password) {
			password = decryptSiteSecret(row.OjLuoguPassword)
		}
		if strings.TrimSpace(password) == "" {
			return &site.VerifyOjCredentialRes{Code: 1, Message: "请先填写洛谷密码", Ok: false}, nil
		}
		if err := ojlogin.VerifyLuogu(username, password); err != nil {
			report(sitesettings.StatusFail, err.Error())
			return &site.VerifyOjCredentialRes{Code: 0, Message: "验证失败", Ok: false, ErrorDetail: err.Error()}, nil
		}
		report(sitesettings.StatusOK, "")
		return &site.VerifyOjCredentialRes{Code: 0, Message: "ok", Ok: true}, nil
	case "QOJ":
		if username == "" {
			username = strings.TrimSpace(row.OjQojUsername)
		}
		if !isRealSecret(password) {
			password = decryptSiteSecret(row.OjQojPassword)
		}
		if strings.TrimSpace(password) == "" {
			return &site.VerifyOjCredentialRes{Code: 1, Message: "请先填写 QOJ 密码", Ok: false}, nil
		}
		if err := ojlogin.VerifyQOJ(username, password); err != nil {
			report(sitesettings.StatusFail, err.Error())
			return &site.VerifyOjCredentialRes{Code: 0, Message: "验证失败", Ok: false, ErrorDetail: err.Error()}, nil
		}
		report(sitesettings.StatusOK, "")
		return &site.VerifyOjCredentialRes{Code: 0, Message: "ok", Ok: true}, nil
	default:
		return &site.VerifyOjCredentialRes{Code: 1, Message: "不支持的平台: " + plat}, nil
	}
}

func (s *SiteService) GetAccessStats(ctx context.Context, req *site.GetAccessStatsReq) (*site.GetAccessStatsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteStatsRead) {
		return &site.GetAccessStatsRes{Code: 1, Message: "需要站点访问统计权限"}, nil
	}
	days := int32(30)
	ipLimit := 200
	pathLimit := 20
	if req != nil {
		if req.Days > 0 {
			days = req.Days
		}
		if req.IpLimit > 0 {
			ipLimit = int(req.IpLimit)
		}
		if req.PathLimit > 0 {
			pathLimit = int(req.PathLimit)
		}
	}
	series := s.data.ListVisitSeries(ctx, int(days))
	today := s.data.GetDayVisitStat(ctx, time.Now())
	yesterday := s.data.GetDayVisitStat(ctx, time.Now().AddDate(0, 0, -1))
	topPaths := s.data.ListTopPaths(ctx, time.Now(), pathLimit)
	categories := s.data.ListCategoryStats(ctx, time.Now())
	ips := s.data.ListIPItems(ctx, time.Now(), ipLimit)
	regByDay := s.data.CountRegistrationsByDay(ctx, int(days))
	ops := opsmetrics.ReadSnapshot(ctx, s.data.RDB)

	toPB := func(st data.DayVisitStat) *site.AccessDayStat {
		return &site.AccessDayStat{
			Date:     st.Date,
			Pv:       st.PV,
			Dau:      st.DAU,
			Uv:       st.UV,
			UniqueIp: st.UniqueIP,
			NewUsers: regByDay[st.Date],
		}
	}
	pbSeries := make([]*site.AccessDayStat, 0, len(series))
	var totalPV, totalDAU int64
	for _, st := range series {
		pbSeries = append(pbSeries, toPB(st))
		totalPV += st.PV
		totalDAU += st.DAU
	}
	pbPaths := make([]*site.AccessPathStat, 0, len(topPaths))
	for _, p := range topPaths {
		pbPaths = append(pbPaths, &site.AccessPathStat{
			Path: p.Path, Category: p.Category, Pv: p.PV, Share: p.Share,
		})
	}
	pbCats := make([]*site.AccessCategoryStat, 0, len(categories))
	for _, c := range categories {
		pbCats = append(pbCats, &site.AccessCategoryStat{
			Category: c.Category, Pv: c.PV, Share: c.Share,
		})
	}
	pbIPs := make([]*site.AccessIpItem, 0, len(ips))
	for _, ip := range ips {
		pbIPs = append(pbIPs, &site.AccessIpItem{
			Ip: ip.IP, Pv: ip.PV, LastPath: ip.LastPath, LastSeen: ip.LastSeen,
		})
	}
	ipOK := clientip.FromContext(ctx) != ""
	registered := s.data.CountRegisteredUsers(ctx)
	mau := s.data.CountMAU(ctx)
	if mau == 0 {
		mau = ops.MAU
	}
	note := "自建统计：PV=页面浏览（同页约30秒节流）；DAU=当日登录用户访问去重；MAU=当月登录用户访问去重；" +
		"新增注册=按上海时区自然日统计账号创建时间；独立IP取自 CF-Connecting-IP / X-Real-IP / XFF；" +
		"API 请求量与并发峰值为网关/服务侧精确日计数；爬虫数据量为当日新写入提交记录条数。"
	return &site.GetAccessStatsRes{
		Code:                0,
		Message:             "success",
		Today:               toPB(today),
		Yesterday:           toPB(yesterday),
		Series:              pbSeries,
		ClientIpAvailable:   ipOK,
		TotalPv:             totalPV,
		TotalDauSum:         totalDAU,
		TopPaths:            pbPaths,
		Categories:          pbCats,
		Ips:                 pbIPs,
		MetricNote:          note,
		RegisteredUsers:     registered,
		Mau:                 mau,
		ApiRequestsToday:    ops.APIRequestsToday,
		ApiPeakConcurrent:   ops.APIPeakToday,
		ApiInflight:         ops.APIInflight,
		SpiderEnqueuedToday: ops.SpiderEnqueued,
		SpiderOkToday:       ops.SpiderOK,
		SpiderFailToday:     ops.SpiderFail,
		SpiderRowsToday:     ops.SpiderRows,
	}, nil
}
