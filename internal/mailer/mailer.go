// Package mailer 负责出站邮件的构造与投递（当前用于注册验证码）。
//
// 意图（Why）：
//
//	把"怎么把一封邮件发给用户"收敛成一个小包，让 server 层只关心业务
//	（生成验证码、写业务文案），不关心 SMTP 会话、TLS 与 MIME 编码。
//
//	刻意只使用标准库（net/smtp、crypto/tls、mime）而不引入第三方邮件库：
//	  1) 本项目的依赖约束要求纯 Go，且越少依赖越容易审计；
//	  2) 当前只需"发一封 HTML 邮件"这一种能力，第三方库的绝大多数特性用不上。
//
// 流转（Flow）：
//
//	server（注册验证码处理器）
//	  └─ mailer.Sender.Send(ctx, to, subject, html)
//	       ├─ 465 端口：直接 TLS 建连（implicit TLS）
//	       └─ 587/25 端口：明文建连后按服务端能力升级 STARTTLS
//	       └─ AUTH PLAIN 登录 → MAIL FROM → RCPT TO → DATA → QUIT
//
// 扩展（Extend）：
//
//	新增邮件类型（找回密码、额度告警）时：在 template.go 加一个正文构造函数即可，
//	发送通道无需改动。若将来需要异步发送/重试，应在本包外新增队列层，
//	不要让 Send 变成"带重试的长耗时调用"——它现在刻意保持"一次尝试，快速失败"。
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
)

// 网络超时参数。
//
// 为什么要设超时：SMTP 服务器无响应时，默认行为会一直挂着，
// 会让用户的"发送验证码"请求一直转圈，最终表现为页面卡死。
// 宁可快速失败并提示"稍后重试"，也不要让用户无限等待。
const (
	dialTimeout = 10 * time.Second // 建连超时
	sendTimeout = 25 * time.Second // 整个会话（含认证与投递）的截止时间
)

// portImplicitTLS 是使用"直接 TLS"的端口（无需 STARTTLS 协商）。
const portImplicitTLS = 465

// ErrNotConfigured 表示邮件功能未配置。
//
// 单独定义该错误是为了让上层能给出"请先配置 SMTP"这样可操作的提示，
// 而不是把底层连接失败原样抛给用户。
var ErrNotConfigured = errors.New("mailer: SMTP 未配置（缺少 host/port/username/password/from）")

// Sender 是邮件发送器，可并发安全复用。
//
// 每次 Send 都新建一条连接而不复用：邮件是低频操作（注册验证码），
// 保持"短连接、无状态"能避免连接被中间设备静默断开后难以排查的问题。
type Sender struct {
	cfg config.SMTPConfig
}

// New 创建邮件发送器。
func New(cfg config.SMTPConfig) *Sender {
	return &Sender{cfg: cfg}
}

// Configured 返回邮件发送能力是否可用，供上层决定是否开放相关功能。
func (s *Sender) Configured() bool {
	return s.cfg.Configured()
}

// From 返回发件人地址，用于在日志或界面中展示（不包含口令）。
func (s *Sender) From() string {
	return s.cfg.From
}

// Send 投递一封 HTML 邮件。
//
// 参数：
//   - ctx：用于在建连阶段判断是否已被取消（SMTP 会话本身无法中断中段传输）；
//   - to：收件人地址；
//   - subject：邮件主题（中文会被自动编码）；
//   - htmlBody：HTML 正文。
func (s *Sender) Send(ctx context.Context, to, subject, htmlBody string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	to = strings.TrimSpace(to)
	if to == "" {
		return errors.New("mailer: 收件人不能为空")
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: dialTimeout}

	tlsConfig := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}

	var (
		conn net.Conn
		err  error
	)
	if s.cfg.Port == portImplicitTLS {
		// 465：先握手再说话（implicit TLS）
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailer: 连接 SMTP 服务器 %s 失败: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	// 整体截止时间：覆盖认证与投递，避免服务端"连上了但不回话"导致长时间挂起
	_ = conn.SetDeadline(time.Now().Add(sendTimeout))

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("mailer: 初始化 SMTP 会话失败: %w", err)
	}
	// 无论成功失败都要把会话收干净，否则服务端可能残留半开连接
	defer func() { _ = client.Close() }()

	// 非 465 端口：尝试升级 STARTTLS。
	// 注意：升级后再认证，凭据才不会以明文经过网络。
	if s.cfg.Port != portImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("mailer: STARTTLS 升级失败: %w", err)
			}
		} else if s.cfg.Port != 25 {
			// 587 等端口若服务端不支持 STARTTLS，直接发凭据不安全，应当拒绝
			return errors.New("mailer: 服务端不支持 STARTTLS，已中止发送以保护凭据")
		}
	}

	if ok, _ := client.Extension("AUTH"); ok {
		// PlainAuth 会在"非 TLS 且非本机"时主动报错，这是一道额外的安全兜底
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mailer: SMTP 认证失败（请检查账号与授权码）: %w", err)
		}
	}

	// 信封发件人使用登录账号：多数服务商（含阿里云邮件推送）要求
	// MAIL FROM 与已认证的发信地址一致，否则会被拒绝或按代发处理。
	if err := client.Mail(s.cfg.Username); err != nil {
		return fmt.Errorf("mailer: 设置发件人失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("mailer: 设置收件人失败: %w", err)
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer: 开始投递正文失败: %w", err)
	}
	if _, err := writer.Write(s.buildMessage(to, subject, htmlBody)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("mailer: 写入邮件正文失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("mailer: 提交邮件失败: %w", err)
	}

	// 主动 QUIT：让服务端立即确认接收，否则连接关闭时才知道对方是否拒收
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mailer: 结束 SMTP 会话失败: %w", err)
	}
	return nil
}

// buildMessage 构造符合 MIME 规范的邮件正文。
//
// 几个容易踩坑的点：
//   - 主题与发件人显示名含中文时【必须】做 MIME 编码（RFC 2047），
//     否则部分邮件客户端会显示成乱码；
//   - 正文【必须】显式声明 charset=UTF-8 并做 base64 编码，
//     否则出现"中文被截断"或"smtp: 行过长"错误；
//   - 头部与正文之间必须空一行，且换行统一用 CRLF（SMTP 规范要求）。
func (s *Sender) buildMessage(to, subject, htmlBody string) []byte {
	var buf bytes.Buffer

	fromHeader := s.cfg.From
	if name := strings.TrimSpace(s.cfg.FromName); name != "" {
		fromHeader = fmt.Sprintf("%s <%s>", mime.BEncoding.Encode("UTF-8", name), s.cfg.From)
	}

	writeHeader(&buf, "From", fromHeader)
	writeHeader(&buf, "To", to)
	writeHeader(&buf, "Subject", mime.BEncoding.Encode("UTF-8", subject))
	writeHeader(&buf, "MIME-Version", "1.0")
	writeHeader(&buf, "Content-Type", `text/html; charset="UTF-8"`)
	writeHeader(&buf, "Content-Transfer-Encoding", "base64")
	// 邮件客户端可能把纯 HTML 邮件判为垃圾；声明为"自动生成的通知"有助于降级判定
	writeHeader(&buf, "Auto-Submitted", "auto-generated")

	buf.WriteString("\r\n")

	// base64 每 76 字符换行（RFC 2045 要求）；Go 的 base64 编码器默认不换行，
	// 这里手工插入 CRLF，避免超长行被部分 SMTP 服务器拒绝。
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody))
	for len(encoded) > 76 {
		buf.WriteString(encoded[:76])
		buf.WriteString("\r\n")
		encoded = encoded[76:]
	}
	buf.WriteString(encoded)
	buf.WriteString("\r\n")

	return buf.Bytes()
}

// writeHeader 写入一个头部行（统一使用 CRLF 换行）。
func writeHeader(buf *bytes.Buffer, key, value string) {
	buf.WriteString(key)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}
