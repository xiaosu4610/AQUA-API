// 本文件实现 AWS Signature Version 4（SigV4）请求签名，供 Bedrock 渠道使用。
//
// 意图（Why）：
//
//	AWS 的服务端不使用静态的密钥头，而是要求客户端对「方法 + 规范化路径 + 规范化查询
//	+ 参与签名的头 + 载荷摘要」整体做一次 HMAC 链式签名，再把结果放进 Authorization。
//	正因为签名覆盖了几乎整条请求，「换个鉴权头」这种改写方式行不通，必须专门实现。
//	这里刻意只用标准库（crypto/hmac、crypto/sha256、encoding/hex）实现，不引入
//	aws-sdk-go —— 本仓库要求零 CGO、依赖精简，而 SigV4 的算法本身并不复杂。
//
//	算法分四步（严格按 AWS 规范，顺序不可换）：
//	  1) 构造规范请求（canonical request）；
//	  2) 对规范请求做 SHA256，得到「待签字符串」（string to sign）；
//	  3) 用「AK4+SK → 日期 → 区域 → 服务 → aws4_request」逐级 HMAC 派生出签名密钥；
//	  4) 用签名密钥对「待签字符串」做 HMAC-SHA256，十六进制结果即签名。
//
//	凭据表达方式（重要，约定，改了就所有渠道都得跟着改）：
//	  渠道密钥字段放 AWS 凭据 **JSON**：
//	    {"access_key_id":"...","secret_access_key":"...","session_token":"..."}
//	  · 密钥与区域分开：密钥（可能含临时会话令牌）属敏感信息，放加密的密钥字段；
//	    区域（region）是非敏感配置，放渠道 extra_config，便于后台表单填写与校验。
//	  · 之所以用 JSON 而不是 "AK:SK" 这种分隔串：secret 里可能含 ':'（官方示例密钥就含
//	    斜杠与加号），分隔串会歧义；JSON 还能无损容纳可选的 session_token（STS 临时凭据）。
//	  · session_token 存在时必须随请求发送且参与签名（见 AWS 对临时凭据的要求）。
//
// 流转（Flow）：
//
//	buildUpstreamRequest
//	  └─ signUpstreamRequest（鉴权方式 = sigv4 时）
//	       └─ signBedrockRequest
//	            ├─ parseAWSCredentials      解析渠道密钥里的凭据 JSON
//	            ├─ sigV4CanonicalRequest     规范请求（可单测的纯函数）
//	            ├─ sigV4DeriveSigningKey     派生签名密钥（可单测的纯函数）
//	            └─ sigV4Authorization        生成 Authorization 头值
//
// 扩展（Extend）：
//
//	接入其它需要 SigV4 的 AWS 服务（如 Bedrock 之外的托管服务）时，只需换 service
//	与路径模板；通用签名逻辑（sigV4* 系列）与服务无关，不要往里塞渠道判断。
//	若要支持「查询串签名（presigned URL）」，在此新增一个出口函数，复用下方纯函数。
package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/channeltype"
)

// SigV4 与 Bedrock 相关的常量。
const (
	// awsSigV4Algorithm 是 Authorization 头里的算法标识。
	awsSigV4Algorithm = "AWS4-HMAC-SHA256"
	// awsAmzDateFormat 是 x-amz-date 的格式（ISO8601 基本格式，UTC）。
	awsAmzDateFormat = "20060102T150405Z"
	// awsDateOnlyFormat 是派生签名密钥与凭据作用域里用的日期格式。
	awsDateOnlyFormat = "20060102"
	// awsTerminationString 是派生签名密钥链的固定收尾串。
	awsTerminationString = "aws4_request"
	// bedrockServiceName 是 SigV4 里 Bedrock 的服务标识（用于作用域与派生）。
	bedrockServiceName = "bedrock"
	// sha256EmptyPayload 是空载荷的 SHA256 十六进制值（AWS 文档给定，勿手改）。
	sha256EmptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// sigV4UnsignedHeaders 是不参与签名的请求头。
//
// 为什么排除 user-agent：AWS 规范明确建议不要把易被中间件改写的「挥发头」
// （user-agent、connection、transfer-encoding 等）纳入签名——纳入了反而会因为
// 代理改一个字就让整条签名失效。我们唯一会透传的挥发头就是调用方的 User-Agent。
var sigV4UnsignedHeaders = map[string]bool{
	"user-agent": true,
}

// awsCredentials 是一条 AWS 凭据（长期密钥或 STS 临时密钥）。
type awsCredentials struct {
	// AccessKeyID 是访问密钥 ID（AK）。
	AccessKeyID string
	// SecretAccessKey 是访问密钥（SK），属敏感信息，绝不写入日志。
	SecretAccessKey string
	// SessionToken 是 STS 临时凭据的会话令牌；非空时必须随请求发送并参与签名。
	SessionToken string
}

// awsCredentialsJSON 是渠道密钥字段里凭据 JSON 的形状。
//
// 字段名与 AWS 惯例保持一致，避免站长在文档之间来回对照。
type awsCredentialsJSON struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

// parseAWSCredentials 解析渠道密钥字段里的 AWS 凭据 JSON。
//
// 未配置或字段缺失时返回明确的中文错误：绝不能静默"不带签名"地发出去，
// 那只会以 403 的形式在上游暴露，让人误以为是密钥写错。
func parseAWSCredentials(apiKey string) (awsCredentials, error) {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return awsCredentials{}, fmt.Errorf("Bedrock 渠道需要配置 AWS 凭据（access_key_id / secret_access_key）")
	}

	var payload awsCredentialsJSON
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return awsCredentials{}, fmt.Errorf(
			"Bedrock 渠道密钥必须是含 access_key_id / secret_access_key 的 JSON: %w", err)
	}
	if strings.TrimSpace(payload.AccessKeyID) == "" || strings.TrimSpace(payload.SecretAccessKey) == "" {
		return awsCredentials{}, fmt.Errorf("Bedrock 渠道需要配置 AWS 凭据（access_key_id / secret_access_key）")
	}
	return awsCredentials{
		AccessKeyID:     strings.TrimSpace(payload.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(payload.SecretAccessKey),
		SessionToken:    strings.TrimSpace(payload.SessionToken),
	}, nil
}

// sigV4SigningInput 是生成 SigV4 签名所需的规范输入（与具体渠道、服务无关）。
//
// 把"输入"显式建模的好处：通用签名逻辑可以脱离 HTTP 请求对象单独测试，
// 也便于用 AWS 官方文档给出的示例向量做交叉验证（见 signature_test.go）。
type sigV4SigningInput struct {
	// Method 是 HTTP 方法（大写）。
	Method string
	// CanonicalURI 是已按 AWS 规则 URI 编码的绝对路径（以 '/' 开头）。
	CanonicalURI string
	// CanonicalQuery 是已按 AWS 规则编码并排序的查询串（无参数时为空串）。
	CanonicalQuery string
	// Headers 是参与签名的头：键为小写名，值为已做空白规范化的值。
	// 必须包含 host；使用临时凭据时还必须包含 x-amz-security-token。
	Headers map[string]string
	// PayloadHash 是请求体的 SHA256 十六进制摘要（空载荷用 sha256EmptyPayload）。
	PayloadHash string
	// Region 是 AWS 区域（作用域与派生都用它）。
	Region string
	// Service 是 AWS 服务标识（Bedrock 为 "bedrock"）。
	Service string
	// AccessKeyID 是访问密钥 ID。
	AccessKeyID string
	// SecretAccessKey 是访问密钥（敏感）。
	SecretAccessKey string
	// Time 是签名时刻（UTC）。
	Time time.Time
}

// sigV4CanonicalRequest 按 AWS 规范拼出规范请求字符串。
//
// 空载荷摘要兜底：调用方若未给出 PayloadHash，按"空请求体"处理，
// 避免拼出空行导致签名与 AWS 侧不一致。
func sigV4CanonicalRequest(in sigV4SigningInput) string {
	payloadHash := in.PayloadHash
	if payloadHash == "" {
		payloadHash = sha256EmptyPayload
	}
	canonicalHeaders, signedHeaders := sigV4CanonicalHeaders(in.Headers)
	return strings.Join([]string{
		in.Method,
		in.CanonicalURI,
		in.CanonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
}

// sigV4CanonicalHeaders 规范化参与签名的头，并返回 SignedHeaders 列表。
//
// 规范化规则（AWS 要求，逐条对应）：
//   - 头名转小写、按字典序排序；
//   - 每条形如 "name:value\n"（注意末尾有换行，因此整段以 '\n' 结尾）；
//   - 值去掉首尾空白、把连续空白折叠为单个空格；多值用逗号连接。
func sigV4CanonicalHeaders(headers map[string]string) (canonical string, signed string) {
	names := make([]string, 0, len(headers))
	normalized := make(map[string]string, len(headers))
	for name, value := range headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" || sigV4UnsignedHeaders[lower] {
			continue
		}
		names = append(names, lower)
		normalized[lower] = strings.Join(strings.Fields(value), " ")
	}
	sort.Strings(names)

	var builder strings.Builder
	for _, name := range names {
		builder.WriteString(name)
		builder.WriteByte(':')
		builder.WriteString(normalized[name])
		builder.WriteByte('\n')
	}
	return builder.String(), strings.Join(names, ";")
}

// sigV4DeriveSigningKey 用 "AWS4"+SK 逐级 HMAC 派生签名密钥。
//
// 为什么分四级：AWS 把「日期 → 区域 → 服务 → aws4_request」逐级混入密钥，
// 使密钥与作用域强绑定，从而限制一处密钥泄露的影响范围。顺序与取值不可调整。
func sigV4DeriveSigningKey(secretAccessKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretAccessKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(awsTerminationString))
}

// sigV4SignatureHex 计算最终签名（十六进制小写）。
func sigV4SignatureHex(in sigV4SigningInput) string {
	canonicalRequest := sigV4CanonicalRequest(in)
	stringToSign := strings.Join([]string{
		awsSigV4Algorithm,
		in.Time.UTC().Format(awsAmzDateFormat),
		sigV4CredentialScope(in.Time, in.Region, in.Service),
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := sigV4DeriveSigningKey(
		in.SecretAccessKey, in.Time.UTC().Format(awsDateOnlyFormat), in.Region, in.Service)
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
}

// sigV4Authorization 生成完整的 Authorization 头值。
func sigV4Authorization(in sigV4SigningInput) string {
	_, signedHeaders := sigV4CanonicalHeaders(in.Headers)
	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		awsSigV4Algorithm,
		in.AccessKeyID,
		sigV4CredentialScope(in.Time, in.Region, in.Service),
		signedHeaders,
		sigV4SignatureHex(in),
	)
}

// sigV4CredentialScope 返回凭据作用域：YYYYMMDD/region/service/aws4_request。
func sigV4CredentialScope(at time.Time, region, service string) string {
	return strings.Join([]string{
		at.UTC().Format(awsDateOnlyFormat), region, service, awsTerminationString,
	}, "/")
}

// hmacSHA256 计算 HMAC-SHA256。
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// sha256Hex 计算数据的 SHA256 十六进制小写摘要。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---- SigV4 与 Bedrock 渠道的接线 ----

// signUpstreamRequest 分派「需要完整请求上下文」的鉴权方式。
//
// 与 applyUpstreamAuth 的分工：后者只把凭据写进头/查询参数，不需要知道 URL 与请求体；
// 本函数处理 SigV4（签名覆盖 URL 与载荷摘要）与服务账号（要换取 access_token），
// 它们在 buildUpstreamRequest 里必须等到最终 URL 拼好之后才能执行。
func signUpstreamRequest(in upstreamRequestInput, baseURL, path, rawQuery string, header http.Header) error {
	switch in.Type.AuthMode {
	case channeltype.AuthSigV4:
		return signBedrockRequest(in, baseURL, path, rawQuery, header)
	case channeltype.AuthServiceAccount:
		return applyVertexServiceAccountAuth(in, header)
	default:
		return fmt.Errorf("relay: 鉴权方式 %q 尚未实现，无法组装上游请求", in.Type.AuthMode)
	}
}

// signBedrockRequest 为 Bedrock 请求生成 SigV4 签名并写入请求头。
//
// 需要 baseURL/path/rawQuery 而非单一 URL 字符串，是为了让「签名用的规范路径」
// 与「实际发出的路径」逐字节一致——两者一旦不一致，AWS 侧验签必然失败。
func signBedrockRequest(in upstreamRequestInput, baseURL, path, rawQuery string, header http.Header) error {
	creds, err := parseAWSCredentials(in.APIKey)
	if err != nil {
		return err
	}
	region := strings.TrimSpace(extraValue(in, "region"))
	if region == "" {
		return fmt.Errorf("Bedrock 渠道需要配置 AWS 区域（extra_config.region）")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("relay: 解析 Bedrock 上游地址失败: %w", err)
	}
	host := parsed.Host

	payloadHash := sha256Hex(in.Body)
	if len(in.Body) == 0 {
		// 空载荷用官方给定的常量，保证与 AWS 侧一致（也便于测试对照文档）。
		payloadHash = sha256EmptyPayload
	}

	at := in.clock()
	header.Set("x-amz-date", at.UTC().Format(awsAmzDateFormat))
	header.Set("x-amz-content-sha256", payloadHash)
	if creds.SessionToken != "" {
		// 临时凭据必须携带会话令牌，否则 AWS 会拒绝。
		header.Set("x-amz-security-token", creds.SessionToken)
	}

	signingHeaders := sigV4HeadersFromHTTP(host, header)
	header.Set("Authorization", sigV4Authorization(sigV4SigningInput{
		Method:          http.MethodPost,
		CanonicalURI:    path,
		CanonicalQuery:  canonicalizeSigV4Query(rawQuery),
		Headers:         signingHeaders,
		PayloadHash:     payloadHash,
		Region:          region,
		Service:         bedrockServiceName,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		Time:            at,
	}))
	return nil
}

// sigV4HeadersFromHTTP 把 http.Header 拍平成「小写名 → 值」并补上 host。
//
// 为什么要手工补 host：net/http 发送时从 URL 自动填 Host，不会经过 Header 映射，
// 因此它不在 header 里；而 SigV4 要求 host 必须参与签名。
func sigV4HeadersFromHTTP(host string, header http.Header) map[string]string {
	result := make(map[string]string, len(header)+1)
	for name, values := range header {
		if len(values) == 0 {
			continue
		}
		result[strings.ToLower(name)] = strings.Join(values, ",")
	}
	result["host"] = host
	return result
}

// canonicalizeSigV4Query 把已编码的查询串规范化为 AWS 形式。
//
// 唯一需要修正的是空格：url.Values.Encode 用 '+' 表示空格，而 SigV4 要求 '%20'。
// 目前 Bedrock 的 invoke 路径不带查询参数，这里做规范化是为了将来加参数时不出错。
func canonicalizeSigV4Query(rawQuery string) string {
	return strings.ReplaceAll(rawQuery, "+", "%20")
}

// awsURIEncode 按 AWS 规范对路径段做 URI 编码。
//
// 为什么不用 net/url 的 PathEscape：AWS 要求对 ':' 等字符也编码（Bedrock 的模型 ID
// 形如 anthropic.claude-3-5-sonnet-20241022-v2:0，冒号必须变成 %3A），而 PathEscape
// 的保留字符集更宽松，会漏编码冒号，导致签名与实际路径不一致。
// 规则：仅保留 A-Z a-z 0-9 与 - _ . ~，其余字节一律 %XX（大写十六进制）。
func awsURIEncode(value string, encodeSlash bool) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			builder.WriteByte(c)
		case c == '/':
			if encodeSlash {
				builder.WriteString("%2F")
			} else {
				builder.WriteByte(c)
			}
		default:
			builder.WriteByte('%')
			builder.WriteByte(hexUpper[c>>4])
			builder.WriteByte(hexUpper[c&0x0f])
		}
	}
	return builder.String()
}

// hexUpper 是大写十六进制字符表，供 awsURIEncode 使用（编码必须大写）。
const hexUpper = "0123456789ABCDEF"
