package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"time"

	"cwxu-algo/app/common/conf"

	"gopkg.in/gomail.v2"
)

// sendTimeout SMTP 发送整体上限：gomail DialAndSend 无读写 deadline，
// 服务器半开时会永久阻塞，必须由外层兜底超时。
const sendTimeout = 30 * time.Second

// dialAndSendTimeout 带超时的 DialAndSend：超时即返回错误。
// 超时后后台 goroutine 会随 TCP 层自身超时退出，不会无限累积。
func dialAndSendTimeout(d *gomail.Dialer, m *gomail.Message, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.DialAndSend(m) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("发送邮件超时（%v）", timeout)
	}
}

// Sender SMTP 邮件发送
type Sender struct {
	host     string
	port     int
	username string
	password string
	from     string
}

// NewSender 从 conf.SMTP 创建发送器；smtp 为 nil 或未配置时仍返回可用对象，Send 会报错
func NewSender(smtp *conf.SMTP) *Sender {
	if smtp == nil {
		return &Sender{}
	}
	return &Sender{
		host:     smtp.Host,
		port:     int(smtp.Port),
		username: smtp.Username,
		password: smtp.Password,
		from:     smtp.From,
	}
}

// Configured 是否已配置 SMTP
func (s *Sender) Configured() bool {
	return s != nil && s.host != ""
}

// Attachment 邮件附件
type Attachment struct {
	// Filename 展示文件名
	Filename string
	// Path 本地文件路径（优先）；与 Content 二选一
	Path string
	// Content 内存内容
	Content []byte
	// ContentType 如 application/pdf；空则由 gomail 推断
	ContentType string
}

// Send 发送 HTML 邮件（自动附带 text/plain 备选）
func (s *Sender) Send(to, subject, body string) error {
	return s.SendWithAttachments(to, subject, body, nil)
}

// SendWithAttachments 发送 HTML 邮件并附带附件
func (s *Sender) SendWithAttachments(to, subject, body string, attachments []Attachment) error {
	if s == nil || s.host == "" {
		return fmt.Errorf("SMTP服务器未配置")
	}
	if to == "" {
		return fmt.Errorf("收件人地址不能为空")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("邮件正文为空")
	}

	m := gomail.NewMessage()
	from := s.from
	if from == "" {
		from = s.username
	}
	m.SetHeader("From", from)
	m.SetHeader("To", to)
	m.SetHeader("Subject", subject)

	// multipart/alternative: SetBody first part, AddAlternative last (clients prefer the last part).
	// Put plain first, HTML last so rich clients render HTML instead of plain text.
	if plain := PlainFromHTML(body); plain != "" {
		m.SetBody("text/plain; charset=UTF-8", plain)
		m.AddAlternative("text/html; charset=UTF-8", body)
	} else {
		m.SetBody("text/html; charset=UTF-8", body)
	}

	for _, a := range attachments {
		name := a.Filename
		if name == "" {
			name = "attachment"
		}
		if a.Path != "" {
			if a.ContentType != "" {
				m.Attach(a.Path, gomail.Rename(name), gomail.SetHeader(map[string][]string{
					"Content-Type": {a.ContentType},
				}))
			} else {
				m.Attach(a.Path, gomail.Rename(name))
			}
			continue
		}
		if len(a.Content) == 0 {
			continue
		}
		settings := []gomail.FileSetting{gomail.Rename(name)}
		if a.ContentType != "" {
			settings = append(settings, gomail.SetHeader(map[string][]string{
				"Content-Type": {a.ContentType},
			}))
		}
		m.Attach(name, append(settings, gomail.SetCopyFunc(func(w io.Writer) error {
			_, err := w.Write(a.Content)
			return err
		}))...)
	}

	d := gomail.NewDialer(s.host, s.port, s.username, s.password)
	d.TLSConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.host,
	}

	if err := dialAndSendTimeout(d, m, sendTimeout); err != nil {
		reportStatus(false, err.Error())
		return fmt.Errorf("发送邮件失败: %w", err)
	}
	reportStatus(true, "")
	return nil
}

// StatusReporter 邮件发送结果回调（由各服务注册，写入站点 SMTP 状态）。
type StatusReporter func(ok bool, errMsg string)

var statusReporter StatusReporter

// SetStatusReporter 注册 SMTP 状态回调；每个服务启动时调用一次。
func SetStatusReporter(fn StatusReporter) {
	statusReporter = fn
}

func reportStatus(ok bool, errMsg string) {
	if statusReporter != nil {
		statusReporter(ok, errMsg)
	}
}
