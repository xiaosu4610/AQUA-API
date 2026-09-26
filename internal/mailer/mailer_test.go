// 本文件是邮件构造逻辑的单元测试。
//
// 测试重点（为什么测这些）：
//   - 头部结构：缺 MIME-Version / Content-Type 会导致部分客户端乱码甚至拒收；
//   - 中文编码：主题与发件人名含中文时必须做 RFC 2047 编码，否则显示为乱码；
//   - 正文可解码：base64 出错会让用户收到一封"看不懂"的邮件，且难以发现；
//   - 换行规范：SMTP 要求 CRLF，写成 LF 会被部分服务端拒收。
package mailer

import (
	"bytes"
	"encoding/base64"
	"mime"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
)

func testSender() *Sender {
	// 构造一个"完整配置"的发送器，仅用于测试消息构造，不会真的发信
	return New(config.SMTPConfig{
		Host:     "smtpdm.aliyun.com",
		Port:     465,
		Username: "aqua@ltzy.top",
		From:     "aqua@ltzy.top",
		FromName: "AQUA 网关",
		Password: "not-a-real-password",
	})
}

func TestConfigured_缺口令_应判为未配置(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.SMTPConfig
		want bool
	}{
		{"完整配置", config.SMTPConfig{Host: "h", Port: 465, Username: "u", From: "f", Password: "p"}, true},
		{"缺口令", config.SMTPConfig{Host: "h", Port: 465, Username: "u", From: "f"}, false},
		{"缺发件人", config.SMTPConfig{Host: "h", Port: 465, Username: "u", Password: "p"}, false},
		{"缺主机", config.SMTPConfig{Port: 465, Username: "u", From: "f", Password: "p"}, false},
		{"全空", config.SMTPConfig{}, false},
	}

	for _, tc := range cases {
		if got := New(tc.cfg).Configured(); got != tc.want {
			t.Errorf("%s：Configured() = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildMessage_中文主题与HTML正文_应可正确解码(t *testing.T) {
	sender := testSender()
	subject, body := RegisterCodeEmail("AQUA 网关", "482913", 5*time.Minute)

	raw := sender.buildMessage("user@example.com", subject, body)

	// 1) 头部与正文之间必须以 CRLF+CRLF 分隔
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		t.Fatal("邮件缺少头部与正文的分隔空行（CRLF CRLF）")
	}
	headerPart := string(raw[:sep])
	bodyPart := raw[sep+4:]

	// 2) 必需的 MIME 头部必须齐全
	for _, expected := range []string{
		"From: ", "To: ", "Subject: ", "MIME-Version: 1.0",
		`Content-Type: text/html; charset="UTF-8"`, "Content-Transfer-Encoding: base64",
	} {
		if !strings.Contains(headerPart, expected) {
			t.Errorf("邮件头部缺少 %q", expected)
		}
	}

	// 3) 收件人应原样出现在 To 头
	if !strings.Contains(headerPart, "user@example.com") {
		t.Error("To 头未包含收件人地址")
	}

	// 4) 主题必须经过 RFC 2047 编码，解码后应还原为原始中文
	decoder := new(mime.WordDecoder)
	var encodedSubject string
	for _, line := range strings.Split(headerPart, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			encodedSubject = strings.TrimPrefix(line, "Subject: ")
		}
	}
	decodedSubject, err := decoder.DecodeHeader(encodedSubject)
	if err != nil {
		t.Fatalf("主题解码失败: %v", err)
	}
	if decodedSubject != subject {
		t.Errorf("主题解码结果 = %q，期望 %q", decodedSubject, subject)
	}

	// 5) 正文 base64 解码后应与原始 HTML 完全一致
	cleaned := strings.ReplaceAll(string(bodyPart), "\r\n", "")
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		t.Fatalf("正文 base64 解码失败: %v", err)
	}
	if string(decoded) != body {
		t.Error("正文解码结果与原始 HTML 不一致")
	}
	if !strings.Contains(string(decoded), "482913") {
		t.Error("正文中未包含验证码")
	}
}

func TestBuildMessage_未配置发件人显示名_不应出现尖括号包裹(t *testing.T) {
	sender := New(config.SMTPConfig{From: "aqua@ltzy.top"})
	raw := string(sender.buildMessage("u@e.com", "主题", "<p>hi</p>"))

	if !strings.Contains(raw, "From: aqua@ltzy.top\r\n") {
		t.Errorf("无显示名时应直接使用地址，实际头部为: %s", firstLines(raw, 3))
	}
}

func TestRegisterCodeEmail_空站点名_应回退为默认品牌名(t *testing.T) {
	subject, body := RegisterCodeEmail("   ", "123456", 5*time.Minute)
	if !strings.Contains(subject, "AQUA-API") {
		t.Errorf("站点名为空时应回退默认品牌名，实际主题: %q", subject)
	}
	if !strings.Contains(body, "AQUA-API") {
		t.Error("正文站点名为空时应回退默认品牌名")
	}
}

func TestRegisterCodeEmail_站点名含HTML字符_应被转义(t *testing.T) {
	_, body := RegisterCodeEmail("<script>alert(1)</script>", "123456", 5*time.Minute)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("站点名中的 HTML 未转义，存在注入风险")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("站点名应被 HTML 转义后再写入正文")
	}
}

// firstLines 返回文本的前 n 行，用于失败信息中给出可读的上下文。
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\r\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
