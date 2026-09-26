// Package i18n 提供「面向终端用户的错误消息」多语言支持。
//
// 意图（Why）：
//
//	网关错误响应里的 error.code 是稳定的机器可读标识（前端据此做逻辑判断），
//	而 error.message 是展示给人看的。把 message 硬编码为中文，会让非中文
//	使用者难以自助排障；因此引入一个轻量消息目录，按请求的 Accept-Language
//	选择语言，命中则返回对应文案。
//
//	为什么默认中文而不是英文：本项目主要用户群为中文使用者，且改动前
//	所有用户可见错误均为中文。以中文为默认，可保证「未携带 Accept-Language」
//	的存量客户端与既有测试行为完全不变（零回归）——这正是本次改造的底线。
//
// 流转（Flow）：
//
//	请求
//	  └─ middleware.Locale（i18n.Parse 解析 Accept-Language）
//	       └─ reqctx.WithLocale 写入请求 context
//	            └─ 处理器 / 中间件出错时 oai.WriteErrorKey(key, locale)
//	                 └─ Bundle.Message 查目录：命中返回对应语言，缺失回退中文
//
// 扩展（Extend）：
//
//	新增词条：在 catalog.go 里为「同一语义键的六种语言」同时补齐翻译；
//	  缺任何一门都会让界面出现"半翻译"（中文与目标语言混杂），
//	  i18n_test.go 的键集合一致性用例会强制这一点。
//	新增语言：在 Locale 常量、Parse 的匹配分支、以及每条词条三处同步补充，
//	  同样别忘了把新语言加入 supported。
package i18n

import (
	"sort"
	"strconv"
	"strings"
)

// Locale 是受支持的语言标识（BCP 47 的简化子集）。
type Locale string

// 受支持的语言。取值刻意采用 BCP 47 风格标识，便于与 Accept-Language 直接比较。
const (
	ZhCN Locale = "zh-CN" // 简体中文
	En   Locale = "en"    // 英语
	Fr   Locale = "fr"    // 法语
	Ru   Locale = "ru"    // 俄语
	Es   Locale = "es"    // 西班牙语
	Ar   Locale = "ar"    // 阿拉伯语
)

// Default 是缺省语言。
//
// 说明：见文件头「为什么默认中文」。凡无法识别的 Accept-Language 一律回退到它，
// 从而保证「不带头部 → 中文」与改动前完全一致。
const Default = ZhCN

// supported 是全部受支持语言，供一致性校验与遍历使用。
var supported = []Locale{ZhCN, En, Fr, Ru, Es, Ar}

// Locales 返回全部受支持语言（副本，调用方不应修改）。
func Locales() []Locale {
	out := make([]Locale, len(supported))
	copy(out, supported)
	return out
}

// Catalog 是消息目录：语义化键 →（语言 → 文案）。
//
// 键必须使用语义化英文名（如 channel.no_available、quota.insufficient），
// 切忌用中文或某一条具体文案当键——否则改文案就会牵动所有调用点。
type Catalog map[string]map[Locale]string

// Bundle 是已加载的消息目录。
type Bundle struct {
	catalog Catalog
}

// New 加载内置消息目录。
func New() *Bundle {
	return &Bundle{catalog: catalog}
}

// Message 按语言取词条。
//
// 回退策略（顺序很重要）：
//  1. 命中该语言的词条 → 直接返回；
//  2. 该语言缺词条 → 回退中文（避免用户看到空白或英文键名）；
//  3. 键本身不存在 → 返回 key 本身（便于在界面上一眼发现漏配的键）。
func (b *Bundle) Message(locale Locale, key string) string {
	if b == nil {
		return key
	}
	entry, ok := b.catalog[key]
	if !ok {
		return key
	}
	if msg := entry[locale]; msg != "" {
		return msg
	}
	if msg := entry[Default]; msg != "" {
		return msg
	}
	return key
}

// Parse 解析 Accept-Language 头，返回最匹配的语言。
//
// 规则：
//   - 支持多值（逗号分隔）与权重（;q=0.9），按权重从高到低匹配；
//   - 权重相同则保持出现顺序（先出现者优先）；
//   - 语言标签按主语言前缀归一：zh / zh-CN / zh-Hans / zh-Hans-CN 均归到 zh-CN；
//     fr-CA 归到 fr；其余同理；
//   - 无法识别（空串、全为未知语言）时一律回退 Default。
func Parse(acceptLanguage string) Locale {
	header := strings.TrimSpace(acceptLanguage)
	if header == "" {
		return Default
	}

	type candidate struct {
		locale Locale
		weight float64
	}
	candidates := make([]candidate, 0, 4)

	for _, part := range strings.Split(header, ",") {
		tag, weight := splitQuality(part)
		if tag == "" {
			continue
		}
		locale, ok := matchLocale(tag)
		if !ok || weight <= 0 {
			// 未知语言、或显式排除（q=0）的条目一律跳过
			continue
		}
		candidates = append(candidates, candidate{locale: locale, weight: weight})
	}
	if len(candidates) == 0 {
		return Default
	}

	// 稳定排序：权重从高到低；权重相同则保持原有顺序
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].weight > candidates[j].weight
	})
	return candidates[0].locale
}

// splitQuality 拆分单个 "语言;q=权重" 片段。
//
// 返回归一化后的语言标签（小写、下划线转连字符）与权重；
// 权重缺省或格式非法时按 1.0 处理——采取宽松策略，避免因个别客户端
// 格式不严谨（如 "en;q=abc"）而整体判为不可用。
func splitQuality(part string) (tag string, weight float64) {
	weight = 1.0
	segments := strings.SplitN(strings.TrimSpace(part), ";", 2)
	tag = normalizeTag(segments[0])
	if len(segments) < 2 {
		return tag, weight
	}

	q := strings.TrimSpace(segments[1])
	q = strings.TrimPrefix(q, "q=")
	q = strings.TrimPrefix(q, "Q=")
	if parsed, err := strconv.ParseFloat(strings.TrimSpace(q), 64); err == nil {
		weight = parsed
	}
	return tag, weight
}

// normalizeTag 把语言标签归一为小写、连字符分隔的形式。
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.ReplaceAll(tag, "_", "-")
	return strings.ToLower(tag)
}

// matchLocale 把语言标签按主语言前缀映射到受支持语言。
//
// 为什么按前缀而不是精确匹配：客户端会发送 zh-Hans-CN、fr-CA 等
// 带书写系统/地区后缀的标签，精确匹配几乎必然失配；按主语言前缀更稳健。
func matchLocale(tag string) (Locale, bool) {
	primary := tag
	if idx := strings.IndexByte(tag, '-'); idx >= 0 {
		primary = tag[:idx]
	}
	switch primary {
	case "zh":
		return ZhCN, true
	case "en":
		return En, true
	case "fr":
		return Fr, true
	case "ru":
		return Ru, true
	case "es":
		return Es, true
	case "ar":
		return Ar, true
	default:
		return "", false
	}
}
