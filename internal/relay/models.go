// 本文件实现「向上游查询可用模型列表」的能力。
//
// 意图（Why）：
//
//	配置上游渠道时最费事的一步是"把这个上游支持的模型名一个个敲进去"——
//	NIM 这类平台上架了几百个模型，人工抄写既慢又必然出错（模型名通常形如
//	"meta/llama-3.1-70b-instruct"，错一个字符就整条路由失效）。
//	因此提供"从上游拉取模型列表"的能力：一次请求把真实清单取回来，
//	管理员只需勾选要开放的部分。
//
//	不把这份清单写死在代码或常量表里的原因：
//	  1) 上游模型更新频繁（每周都有新模型上线/下线），写死必然过期；
//	  2) 各家上游的模型命名与集合完全不同，无法穷举。
//
// 流转（Flow）：
//
//	后台「拉取模型」按钮
//	  └─ POST /api/admin/channels/fetch-models
//	       └─ Relay.FetchModels(ctx, baseURL, apiKey)
//	            └─ GET {base_url}/models  → 解析 → 返回模型名列表
//
// 扩展（Extend）：
//
//	若某上游的模型列表端点不是 OpenAI 约定的 GET /models，
//	在此新增一个 provider 分支（按渠道类型分发），并同步 test 用例。
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// modelsPath 是 OpenAI 兼容的模型列表端点。
	//
	// 必须带 /v1 前缀：本项目的 base_url 约定是"只填到域名根"，
	// 版本前缀由各端点常量自己带上（见 oai.ChatCompletionsPath = "/v1/chat/completions"）。
	// 曾经的坑：这里写成 "/models"，配合 base_url=https://host/v1 拉取能成功，
	// 但同一渠道转发对话请求会拼成 /v1/v1/chat/completions 而返回 404——
	// 两处路径约定必须严格一致。
	modelsPath = "/v1/models"
	// modelListTimeout 是拉取模型列表的整体超时。
	//
	// 取 20 秒：这只是列目录式的轻量请求，正常在数百毫秒内完成；
	// 超过 20 秒基本可判定上游异常，继续等待只会让管理员界面一直转圈。
	modelListTimeout = 20 * time.Second
	// maxModelListBytes 是模型列表响应的读取上限。
	//
	// 取 4MiB：NIM 平台上架上千个模型时，JSON 体积可达数百 KB；
	// 4MiB 足以覆盖，同时防止异常上游用超大响应撑爆内存。
	maxModelListBytes = 4 << 20
	// maxUpstreamErrorBytes 是失败时读取上游错误体的上限。
	maxUpstreamErrorBytes = 4 << 10
)

// FetchModels 向上游查询该渠道可用的模型名列表。
//
// 参数：
//   - baseURL：上游基础地址，只填到域名根（如 https://integrate.api.nvidia.com）；
//   - apiKey：用于鉴权的上游密钥（可为空——少数自建上游无需鉴权）。
//
// 返回的模型名已去重并排序，保证同一份上游数据每次返回顺序一致。
func (r *Relay) FetchModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, errors.New("relay: 上游地址不能为空")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("relay: 上游地址必须以 http:// 或 https:// 开头，当前为 %q", baseURL)
	}

	requestURL := strings.TrimRight(baseURL, "/") + modelsPath

	// 单独设置超时：列模型是轻量操作，不应沿用转发链路那种"等首字节 60 秒"的策略
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("relay: 构造模型列表请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		// 不回传底层错误细节，避免把内部地址与网络拓扑暴露到管理界面之外
		return nil, fmt.Errorf("relay: 连接上游失败（请检查地址与网络）: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 上游的错误体对排查很有价值（如"invalid api key"），
		// 但只截取有限长度，避免异常上游返回超大内容
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes))
		message := strings.TrimSpace(string(detail))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, truncateText(message, 300))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("relay: 读取模型列表失败: %w", err)
	}

	models, err := parseModelList(raw)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("上游返回了空的模型列表（该账号可能无权列出模型）")
	}
	return models, nil
}

// modelEntry 描述一条模型记录。
//
// 为什么同时保留三个字段名：不同上游对"模型标识"的字段命名不统一
// （OpenAI 用 id、部分平台用 name、少量自建服务用 model）。
// 逐个兜底比要求使用者改造上游更务实。
type modelEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

// identifier 返回该记录中最先在语义上成立的模型标识。
func (m modelEntry) identifier() string {
	for _, candidate := range []string{m.ID, m.Name, m.Model} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return ""
}

// parseModelList 从上游响应体中解析模型名列表。
//
// 兼容四种常见形态（实测不同上游/不同版本会返回不同结构）：
//  1. {"data":[{"id":"..."}]}       —— OpenAI 标准
//  2. {"data":["m1","m2"]}          —— 简化实现
//  3. ["m1","m2"]                   —— 顶层数组
//  4. {"models":["m1","m2"]}        —— 部分自建服务
//
// 判定要点：用"字段是否存在"（!= nil）而不是"是否非空"来决定形态匹配。
// 这样 {"data":[]} 会被识别为"格式正确但列表为空"，交由上层给出
// "该账号可能无权列出模型"的精确提示，而不是误导性的"无法识别的格式"。
func parseModelList(raw []byte) ([]string, error) {
	var obj struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Data != nil {
		names := make([]string, 0, len(obj.Data))
		for _, entry := range obj.Data {
			if id := entry.identifier(); id != "" {
				names = append(names, id)
			}
		}
		return normalizeModelNames(names), nil
	}

	var simpleData struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(raw, &simpleData); err == nil && simpleData.Data != nil {
		return normalizeModelNames(simpleData.Data), nil
	}

	var legacy struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(raw, &legacy); err == nil && legacy.Models != nil {
		return normalizeModelNames(legacy.Models), nil
	}

	var topLevel []string
	if err := json.Unmarshal(raw, &topLevel); err == nil && topLevel != nil {
		return normalizeModelNames(topLevel), nil
	}

	return nil, errors.New("无法识别的模型列表格式（期望 OpenAI 兼容的 /models 响应）")
}

// normalizeModelNames 去除空白与重复项并排序。
//
// 排序的意义：让管理界面的勾选列表稳定有序（否则每次拉取顺序都可能不同，
// 使用者会以为"模型集合变了"）。
func normalizeModelNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// truncateText 截断过长的文本，用于错误信息（避免把上游整页 HTML 灌进界面）。
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…（已截断）"
}
