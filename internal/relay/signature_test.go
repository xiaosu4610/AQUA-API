// AWS SigV4 签名与 Bedrock 接线的单元测试。
//
// 意图（Why）：
//
//	SigV4 的签名覆盖「方法 + 路径 + 查询 + 参与签名的头 + 载荷摘要」，任何一处算错
//	都会让 AWS 侧验签失败，表现为难以定位的 403。因此这里分两层验证：
//	  1) 用 AWS 官方文档给出的示例向量交叉验证通用算法（规范请求、签名密钥、最终签名）；
//	  2) 用固定凭据/时间/请求体验证 Bedrock 接线：路径编码、x-amz-* 头与 Authorization。
//
// 流转（Flow）：
//
//	go test ./internal/relay/
//	  ├─ sigV4CanonicalRequest / sigV4DeriveSigningKey / sigV4Authorization（纯函数）
//	  └─ buildUpstreamRequest（Bedrock 渠道的完整组装）
//
// 扩展（Extend）：
//
//	新增需要 SigV4 的渠道时，在此追加"该渠道路径编码与头集合"的断言；
//	通用算法若改动，务必保证官方向量用例仍然通过。
package relay

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// awsDocExample 是 AWS 官方文档「Create a signed request」给出的 IAM ListUsers 示例向量。
//
// 来源：AWS Identity and Access Management 用户指南（Task 1/2/3 的示例）。
// 这些是公开的测试向量（非代码），用它交叉验证我们的实现与 AWS 侧一致。
var awsDocExample = struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Service         string
	At              time.Time
	Method          string
	CanonicalURI    string
	CanonicalQuery  string
	Headers         map[string]string
	PayloadHash     string
	CanonicalReq    string
	SigningKeyHex   string
	Signature       string
}{
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	Region:          "us-east-1",
	Service:         "iam",
	At:              time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC),
	Method:          "GET",
	CanonicalURI:    "/",
	CanonicalQuery:  "Action=ListUsers&Version=2010-05-08",
	Headers: map[string]string{
		"content-type": "application/x-www-form-urlencoded; charset=utf-8",
		"host":         "iam.amazonaws.com",
		"x-amz-date":   "20150830T123600Z",
	},
	PayloadHash: sha256EmptyPayload,
	CanonicalReq: strings.Join([]string{
		"GET",
		"/",
		"Action=ListUsers&Version=2010-05-08",
		"content-type:application/x-www-form-urlencoded; charset=utf-8",
		"host:iam.amazonaws.com",
		"x-amz-date:20150830T123600Z",
		// 规范头整段以 '\n' 结尾，随后与 SignedHeaders 之间还有一个分隔 '\n'，
		// 因此这里必须有一个空行——AWS 文档的排版会把它显示成"连在一起"，
		// 但官方给出的规范请求哈希正是对含空行的字符串计算的。
		"",
		"content-type;host;x-amz-date",
		sha256EmptyPayload,
	}, "\n"),
	SigningKeyHex: "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9",
	Signature:     "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7",
}

// TestSigV4_官方示例向量 用 AWS 文档的示例向量交叉验证签名实现。
//
// 断言到最后一个中间量（规范请求字符串、规范请求哈希、签名密钥、最终签名），
// 这样一旦哪一步实现偏离规范，失败信息能直接指出是哪一步。
func TestSigV4_官方示例向量(t *testing.T) {
	in := sigV4SigningInput{
		Method:          awsDocExample.Method,
		CanonicalURI:    awsDocExample.CanonicalURI,
		CanonicalQuery:  awsDocExample.CanonicalQuery,
		Headers:         awsDocExample.Headers,
		PayloadHash:     awsDocExample.PayloadHash,
		Region:          awsDocExample.Region,
		Service:         awsDocExample.Service,
		AccessKeyID:     awsDocExample.AccessKeyID,
		SecretAccessKey: awsDocExample.SecretAccessKey,
		Time:            awsDocExample.At,
	}

	if got := sigV4CanonicalRequest(in); got != awsDocExample.CanonicalReq {
		t.Fatalf("规范请求与 AWS 文档不一致:\n--- got ---\n%s\n--- want ---\n%s", got, awsDocExample.CanonicalReq)
	}

	canonicalHash := sha256Hex([]byte(awsDocExample.CanonicalReq))
	const wantCanonicalHash = "f536975d06c0309214f805bb90ccff089219ecd68b2577efef23edd43b7e1a59"
	if canonicalHash != wantCanonicalHash {
		t.Errorf("规范请求哈希 = %s，期望 %s", canonicalHash, wantCanonicalHash)
	}

	signingKey := sigV4DeriveSigningKey(
		awsDocExample.SecretAccessKey, "20150830", awsDocExample.Region, awsDocExample.Service)
	if got := hex.EncodeToString(signingKey); got != awsDocExample.SigningKeyHex {
		t.Errorf("签名密钥 = %s，期望 %s", got, awsDocExample.SigningKeyHex)
	}

	if got := sigV4SignatureHex(in); got != awsDocExample.Signature {
		t.Errorf("签名 = %s，期望 %s", got, awsDocExample.Signature)
	}

	wantAuth := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=" + awsDocExample.Signature
	if got := sigV4Authorization(in); got != wantAuth {
		t.Errorf("Authorization = %q，期望 %q", got, wantAuth)
	}
}

// TestSigV4_uri编码符合AWS规则 验证路径段编码保留/转义字符集与 AWS 一致。
//
// 关键差异：AWS 要求对 ':' 编码为 %3A、对 '/' 视场景处理，而 net/url.PathEscape
// 会漏编码冒号——这正是该函数必须自己实现的原因。
func TestSigV4_uri编码符合AWS规则(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", false, "anthropic.claude-3-5-sonnet-20241022-v2%3A0"},
		{"meta/llama-3-70b", false, "meta/llama-3-70b"},
		{"meta/llama-3-70b", true, "meta%2Fllama-3-70b"},
		{"a b+c~d", false, "a%20b%2Bc~d"},
	}
	for _, tc := range cases {
		if got := awsURIEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("awsURIEncode(%q, %v) = %q，期望 %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

// bedrockTestCreds 是 Bedrock 用例使用的固定 AWS 凭据 JSON（沿用 AWS 文档示例密钥）。
const bedrockTestCreds = `{"access_key_id":"AKIDEXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}`

// bedrockTestTime 是 Bedrock 用例的固定签名时刻。
var bedrockTestTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

// TestBedrock_签名头与路径 验证 Bedrock 渠道组装出的路径、x-amz-* 头与 Authorization。
//
// 用固定凭据 / 固定时间 / 固定请求体，断言到完整字符串，任何一处接线改动都会暴露。
func TestBedrock_签名头与路径(t *testing.T) {
	spec := mustType(t, "bedrock")
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey:  bedrockTestCreds,
		Model:   "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"region": "us-east-1"},
		Body:    body,
		now:     bedrockTestTime,
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	// 路径：模型 ID 里的冒号必须编码为 %3A（与签名覆盖的规范路径一致）。
	wantURL := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/invoke"
	if built.URL != wantURL {
		t.Errorf("URL = %q，期望 %q", built.URL, wantURL)
	}

	// 日期头由固定时钟给出，可独立断言。
	if got := built.Header.Get("x-amz-date"); got != "20150830T123600Z" {
		t.Errorf("x-amz-date = %q，期望 20150830T123600Z", got)
	}

	// 载荷摘要应等于请求体的 SHA256，可独立计算断言。
	wantPayloadHash := sha256Hex(body)
	if got := built.Header.Get("x-amz-content-sha256"); got != wantPayloadHash {
		t.Errorf("x-amz-content-sha256 = %q，期望 %q", got, wantPayloadHash)
	}

	// 未使用临时凭据时不得携带 x-amz-security-token。
	if got := built.Header.Get("x-amz-security-token"); got != "" {
		t.Errorf("无 session_token 时不应携带 x-amz-security-token，实际 = %q", got)
	}

	// 完整 Authorization 字符串（含凭据作用域、参与签名的头列表与签名）。
	// 该值由 AWS SigV4 算法对上述固定输入生成；算法本身由 TestSigV4_官方示例向量
	// 用 AWS 官方示例向量交叉验证，因此这里锚定完整串可作为回归保护。
	const wantAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/bedrock/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0983f001e1e96b28df668c15cbbd86f4337c5645b7cf7cf8a098532fa85708a"
	if auth := built.Header.Get("Authorization"); auth != wantAuthorization {
		t.Errorf("Authorization = %q，期望 %q", auth, wantAuthorization)
	}
	if !built.authInjected {
		t.Error("Bedrock 请求应标记为已注入凭据")
	}
}

// TestBedrock_流式路径 验证流式请求走 invoke-with-response-stream 端点。
func TestBedrock_流式路径(t *testing.T) {
	spec := mustType(t, "bedrock")

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey:  bedrockTestCreds,
		Model:   "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"region": "us-east-1"},
		Stream:  true,
		Body:    []byte(`{}`),
		now:     bedrockTestTime,
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	if !strings.HasSuffix(built.URL, "/invoke-with-response-stream") {
		t.Errorf("流式 URL = %q，期望以 /invoke-with-response-stream 结尾", built.URL)
	}
}

// TestBedrock_临时凭据需带会话令牌 验证 session_token 存在时写入 x-amz-security-token。
func TestBedrock_临时凭据需带会话令牌(t *testing.T) {
	spec := mustType(t, "bedrock")
	creds := `{"access_key_id":"AKIDEXAMPLE","secret_access_key":"secret","session_token":"SESSION-TOKEN-X"}`

	built, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey:  creds,
		Model:   "m",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"region": "us-east-1"},
		Body:    []byte(`{}`),
		now:     bedrockTestTime,
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}
	if got := built.Header.Get("x-amz-security-token"); got != "SESSION-TOKEN-X" {
		t.Errorf("x-amz-security-token = %q，期望 SESSION-TOKEN-X", got)
	}
	// 会话令牌必须参与签名，因此会出现在 SignedHeaders 列表里。
	if !strings.Contains(built.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("使用临时凭据时，x-amz-security-token 必须参与签名（出现在 SignedHeaders）")
	}
}

// TestBedrock_凭据缺失给出明确中文错误 验证未配置凭据时不静默放行。
func TestBedrock_凭据缺失给出明确中文错误(t *testing.T) {
	spec := mustType(t, "bedrock")

	cases := []struct {
		name    string
		apiKey  string
		extra   map[string]string
		wantMsg string
	}{
		{
			name:    "空密钥",
			apiKey:  "",
			extra:   map[string]string{"region": "us-east-1"},
			wantMsg: "AWS 凭据",
		},
		{
			name:    "密钥不是合法JSON",
			apiKey:  "AKIA:secret",
			extra:   map[string]string{"region": "us-east-1"},
			wantMsg: "JSON",
		},
		{
			name:    "缺少区域",
			apiKey:  bedrockTestCreds,
			extra:   map[string]string{},
			wantMsg: "区域",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUpstreamRequest(upstreamRequestInput{
				Type:    spec,
				BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
				APIKey:  tc.apiKey,
				Model:   "m",
				Path:    "/v1/chat/completions",
				Extra:   tc.extra,
				Body:    []byte(`{}`),
				now:     bedrockTestTime,
			})
			if err == nil {
				t.Fatal("应报错，实际未报错")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestBedrock_无凭据不注入鉴权头 反向确认失败路径没有被静默放过。
//
// 与上一条互补：若实现退化成"签不出来就直接发"，此用例断言它必须失败而非带空鉴权头。
func TestBedrock_无凭据不注入鉴权头(t *testing.T) {
	spec := mustType(t, "bedrock")
	_, err := buildUpstreamRequest(upstreamRequestInput{
		Type:    spec,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey:  "",
		Model:   "m",
		Path:    "/v1/chat/completions",
		Extra:   map[string]string{"region": "us-east-1"},
		Body:    []byte(`{}`),
		now:     bedrockTestTime,
	})
	if err == nil {
		t.Fatal("未配置凭据时应返回错误，而不是组装出无鉴权头的请求")
	}
}
