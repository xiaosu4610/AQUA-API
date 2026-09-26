// 本文件的职责：构造邮件的主题与正文文案。
//
// 意图（Why）：
//
//	把"用户看到的邮件长什么样"与"邮件怎么发出去"分开。
//	前者是产品文案（会频繁调整），后者是协议细节（很少变动），
//	混在一起会让每次改文案都要动传输代码，容易引入回归。
//
// 设计取舍（为什么是纯内联样式的 HTML）：
//   - 邮件客户端对 CSS 的支持极不一致，<style> 标签常被剥离，
//     因此所有样式必须内联；
//   - 不使用外部图片与字体：外链图片是垃圾邮件评分的重要负面信号；
//   - 正文同时给出"纯文本可读"的关键信息（验证码单独成行、字号大），
//     即使样式被剥离也不影响使用。
package mailer

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// RegisterCodeEmail 构造注册验证码邮件的主题与 HTML 正文。
//
// 参数：
//   - siteName：站点显示名（来自后台设置）；
//   - code：验证码明文；
//   - ttl：有效期。
func RegisterCodeEmail(siteName, code string, ttl time.Duration) (subject, htmlBody string) {
	// 站点名来自后台设置，属于半可信输入：这里做 HTML 转义，
	// 避免管理员无意间填入的字符破坏邮件结构（或在客户端触发脚本解析）。
	name := html.EscapeString(strings.TrimSpace(siteName))
	if name == "" {
		name = "AQUA-API"
	}

	minutes := int(ttl.Minutes())
	if minutes <= 0 {
		minutes = 5
	}

	subject = fmt.Sprintf("【%s】注册验证码", name)

	// 用 fmt.Sprintf 拼装而非 html/template：结构固定且只有三个变量，
	// 引入模板引擎的复杂度不值得；转义已在上方显式完成。
	htmlBody = fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="UTF-8"><title>%[1]s</title></head>
<body style="margin:0;padding:24px;background:#f1f5f9;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;color:#0f172a;">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border:1px solid #e2e8f0;border-radius:14px;padding:28px;">
    <h1 style="margin:0 0 8px;font-size:18px;font-weight:600;color:#0f172a;">%[1]s</h1>
    <p style="margin:0 0 20px;font-size:13px;color:#64748b;">您正在注册账号，请使用以下验证码完成验证。</p>

    <div style="background:#ecfeff;border:1px solid #a5f3fc;border-radius:12px;padding:18px;text-align:center;">
      <div style="font-size:12px;color:#0e7490;letter-spacing:1px;">验证码</div>
      <div style="margin-top:8px;font-size:32px;font-weight:700;letter-spacing:8px;color:#0891b2;font-family:'SFMono-Regular',Consolas,monospace;">%[2]s</div>
    </div>

    <p style="margin:20px 0 0;font-size:13px;color:#475569;line-height:1.7;">
      验证码 <strong>%[3]d 分钟</strong>内有效，且只能使用一次。<br>
      若非本人操作，请忽略本邮件，您的账号不会受到影响。
    </p>

    <hr style="margin:22px 0 14px;border:none;border-top:1px solid #e2e8f0;">
    <p style="margin:0;font-size:12px;color:#94a3b8;">本邮件由系统自动发送，请勿直接回复。</p>
  </div>
</body>
</html>`, name, code, minutes)

	return subject, htmlBody
}
