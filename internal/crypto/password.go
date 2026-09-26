// 本文件提供用户口令的哈希与校验能力。
//
// 意图（Why）：
//
//	用户口令绝不能明文存储，也【不能】用可逆加密——后者一旦泄露即可直接还原。
//	正确做法是「带盐的慢哈希」：每个口令用独立随机盐，且计算代价可调，
//	使得攻击者拿到数据库后既无法用彩虹表批量破解，也无法高速穷举。
//
// 流转（Flow）：
//
//	注册/改密：HashPassword(明文) → 存入 users.password_hash
//	登录校验：VerifyPassword(明文, 存储的哈希) → 是否匹配
//
// 扩展（Extend）：
//
//	未来更换算法时，保持这两个函数的签名不变；
//	VerifyPassword 需能识别旧格式并在登录成功后透明升级为新格式
//	（做法：在哈希串前加版本前缀，如 "v2:"，此处目前隐式依赖 bcrypt 的自描述格式）。
package crypto

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 是 bcrypt 的计算代价参数。
//
// 取值 11 的权衡（这是一个「安全性 vs 可用性」的经典取舍，说明如下）：
//   - 下限：OWASP 建议 bcrypt 代价不低于 10；11 已高于该基线；
//   - 上限受「登录接口的 DoS 风险」约束：每次校验都是实打实的 CPU 开销，
//     若单次耗时接近 1 秒，攻击者用少量并发请求即可打满 CPU
//     （我们的生产实例只有 2 核，且与转发服务共享 CPU）。
//     实测代价 12 时单次约 0.8s，代价 11 约 0.4s——后者在安全性与抗打满之间更均衡。
//   - 配套措施：登录接口必须叠加频率限制，不能只依赖哈希代价来防暴力破解。
const bcryptCost = 11

// 口令长度约束。
//
// 设计原则（为什么不再限制最小长度与字符类型）：
//
//	口令策略应由站点运营者按场景决定，而不是由代码替使用者做主。
//	因此本实现【不限制】口令的最小长度，也不要求任何字符类型：
//	1 位数字、纯中文、含空格的短语、任意特殊符号都合法。
//	真正的安全边界由「登录接口限流 + bcrypt 慢哈希」共同保证；
//	而"必须 8 位且含大小写数字"这类规则并不能提升实际安全性，
//	反而会催生 "Passw0rd!" 这种高度可预测的口令。
//
// 为什么仍保留一个上限：
//
//	这不是对"密码能有多长"的业务限制，而是一道 DoS 护栏——
//	口令在交给 bcrypt 之前要先过一遍 SHA-256，输入越长 CPU 与内存拷贝代价越高，
//	没有上限时攻击者可用超长口令放大服务端开销。
//	1024 字节已远超任何真实口令（最长的中文口令也只需几十字节）。
const maxPasswordBytes = 1024

// ErrPasswordEmpty 表示口令为空。
//
// 唯一保留的规则：空口令等同于"无鉴权"，任何长度与字符要求都取消了，
// 但"完全不放口令"不应被允许。
var ErrPasswordEmpty = fmt.Errorf("crypto: 口令不能为空")

// ErrPasswordTooLong 表示口令超过技术上限（DoS 护栏，非业务规则）。
var ErrPasswordTooLong = fmt.Errorf("crypto: 口令过长（最多 %d 字节）", maxPasswordBytes)

// HashPassword 计算口令哈希，返回可直接存库的字符串（bcrypt 自描述格式，含盐与代价参数）。
func HashPassword(plain string) (string, error) {
	if err := validatePassword(plain); err != nil {
		return "", err
	}

	// 关键处理：先用 SHA-256 摘要再交给 bcrypt。
	//
	// 原因：bcrypt 只取输入的前 72 字节，超出部分被静默忽略。
	// 若不处理，"24 个中文汉字的口令"与"在此基础上再加任意后缀"会算出同一个哈希——
	// 也就是不同口令可以互相登录，属于严重安全缺陷。
	// 先做 SHA-256 后输入长度恒为固定长度（再用 Base64 转为可打印字符），从而彻底规避该限制。
	digest := sha256.Sum256([]byte(plain))
	prepared := base64.StdEncoding.EncodeToString(digest[:])

	hashed, err := bcrypt.GenerateFromPassword([]byte(prepared), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("crypto: 计算口令哈希失败: %w", err)
	}
	return string(hashed), nil
}

// VerifyPassword 校验明文口令与存储的哈希是否匹配。
//
// 安全性说明：
//   - 使用 bcrypt 的恒定时间比较，避免通过响应时间差推断口令；
//   - 任何异常（格式非法、空值）都返回 false，不区分"格式错误"与"口令错误"，
//     避免向攻击者泄露有效账号信息。
func VerifyPassword(plain, hashed string) bool {
	if plain == "" || hashed == "" {
		return false
	}
	// 校验时同样需要先做 SHA-256，与 HashPassword 的处理保持一致
	digest := sha256.Sum256([]byte(plain))
	prepared := base64.StdEncoding.EncodeToString(digest[:])

	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(prepared)) == nil
}

// validatePassword 校验口令是否可接受。
//
// 只有两项判断：非空、不超过技术上限；其余一律放行（设计原则见上方常量注释）。
//
// 注意：这里【不做】任何字符类型检查，因此中文、emoji、空白、控制字符都会通过——
// 这不会带来安全问题，因为口令不参与任何解析（只做哈希），
// 而"允许特殊字符"恰恰是让使用者能选到高强度口令的前提。
func validatePassword(plain string) error {
	if plain == "" {
		return ErrPasswordEmpty
	}
	if len(plain) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// ValidatePasswordStrength 对外暴露口令规则校验，供注册/改密接口在入库前预检。
//
// 与 HashPassword 内部校验共用同一套规则，避免"接口放行但入库失败"的不一致。
func ValidatePasswordStrength(plain string) error {
	return validatePassword(plain)
}
