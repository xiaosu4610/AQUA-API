// 本文件实现「敏感词过滤」的后台管理接口。
//
// 意图（Why）：
//
//	过滤能力必须在后台可维护，否则等于把黑名单写死在代码里——每次调整都要发版，
//	而违规词是随时在变的，站长根本追不上。
//
//	本文件只负责"词表 CRUD + 总开关"，具体的匹配与拦截在
//	middleware.SensitiveFilter 中完成（它缓存编译好的匹配器）。
//
//	每次写操作后都会调用 Invalidate：不这样做的话，站长删掉一个词却仍被拦截
//	长达 30 秒，第一反应必然是"删了没用、这功能是坏的"。
//
// 流转（Flow）：
//
//	后台敏感词页 → GET    /api/admin/sensitive-words          列出全部词条
//	             → POST   /api/admin/sensitive-words          新增单条
//	             → POST   /api/admin/sensitive-words/import   批量导入（一行一个词）
//	             → PUT    /api/admin/sensitive-words/:id      更新（改词/分类/启停/备注）
//	             → DELETE /api/admin/sensitive-words/:id      删除
//
// 扩展（Extend）：
//
//	新增词条属性时：在 model.SensitiveWord 加字段 → 建迁移加列 → 同步 store 的
//	列清单 / INSERT / UPDATE / scan 四处 → 本文件的 DTO 与更新入参补齐。
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
	"github.com/LTZY-ACU/ltzy-api/internal/oai"
)

// maxSensitiveImportWords 是单次批量导入的词条条数上限。
//
// 设上限是为了防止误粘贴一个超大文件（例如把整个字典贴进来）：
// 一次导入上万条会长时间占用写锁，而且几乎必然是误操作。
const maxSensitiveImportWords = 2000

// sensitiveWordDTO 是敏感词的对外表示。
type sensitiveWordDTO struct {
	ID        uint64 `json:"id"`
	Word      string `json:"word"`
	Category  string `json:"category"`
	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// toSensitiveWordDTO 把领域模型转为对外 DTO。
func toSensitiveWordDTO(word *model.SensitiveWord) sensitiveWordDTO {
	if word == nil {
		return sensitiveWordDTO{}
	}
	return sensitiveWordDTO{
		ID:        word.ID,
		Word:      word.Word,
		Category:  word.Category,
		Enabled:   word.Enabled,
		Remark:    word.Remark,
		CreatedAt: unixOrZero(word.CreatedAt),
		UpdatedAt: unixOrZero(word.UpdatedAt),
	}
}

// handleListSensitiveWords 返回全部敏感词。
//
// 不做分页：词表规模在数百条以内，一次返回让前端好做本地筛选与去重提示；
// 真到了需要分页的量级，说明词表已经无法人工维护，应当改用外部词库方案。
func (s *Server) handleListSensitiveWords(c *gin.Context) {
	if s.deps.SensitiveWords == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"敏感词模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	words, err := s.deps.SensitiveWords.List(c.Request.Context(), false)
	if err != nil {
		s.respondInternalError(c, "查询敏感词失败")
		return
	}

	items := make([]sensitiveWordDTO, 0, len(words))
	enabled := 0
	for _, word := range words {
		if word.Enabled {
			enabled++
		}
		items = append(items, toSensitiveWordDTO(word))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items), "enabled_total": enabled})
}

// sensitiveWordUpsertRequest 是新增/更新敏感词的请求体。
type sensitiveWordUpsertRequest struct {
	Word     string `json:"word"`
	Category string `json:"category"`
	Enabled  *bool  `json:"enabled"`
	Remark   string `json:"remark"`
}

// handleCreateSensitiveWord 新增单条敏感词。
func (s *Server) handleCreateSensitiveWord(c *gin.Context) {
	if s.deps.SensitiveWords == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"敏感词模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req sensitiveWordUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	word := &model.SensitiveWord{
		Word:     req.Word,
		Category: strings.TrimSpace(req.Category),
		Remark:   strings.TrimSpace(req.Remark),
		// 默认启用：新增词条的目的通常就是立即生效
		Enabled: true,
	}
	if req.Enabled != nil {
		word.Enabled = *req.Enabled
	}

	if err := s.deps.SensitiveWords.Create(c.Request.Context(), word); err != nil {
		if errors.Is(err, model.ErrSensitiveWordDuplicated) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该敏感词已存在", oai.TypeInvalidRequest, "sensitive_word_duplicated")
			return
		}
		// 领域校验的错误信息（如"长度必须在 1~64"）对使用者有直接帮助
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_sensitive_word")
		return
	}

	s.invalidateSensitiveFilter()
	c.JSON(http.StatusOK, toSensitiveWordDTO(word))
}

// sensitiveWordImportRequest 是批量导入的请求体。
//
// 用"整段文本"而不是字符串数组：站长通常是从别处粘贴一份词表，
// 让服务端按换行/逗号等常见分隔符切分，比要求前端先切好再提交更省事。
type sensitiveWordImportRequest struct {
	Text     string `json:"text"`
	Category string `json:"category"`
}

// handleImportSensitiveWords 批量导入敏感词。
func (s *Server) handleImportSensitiveWords(c *gin.Context) {
	if s.deps.SensitiveWords == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"敏感词模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req sensitiveWordImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	candidates := splitSensitiveInput(req.Text)
	if len(candidates) == 0 {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"没有解析到任何词条（请每行一个词，或用逗号分隔）",
			oai.TypeInvalidRequest, "empty_sensitive_input")
		return
	}

	category := strings.TrimSpace(req.Category)
	// 单次导入上限可在后台「运行上限」页调整（默认 2000），避免一次粘贴整本字典长时间占用写锁。
	importLimit := s.limitSettingsCached(c.Request.Context()).SensitiveImportMaxWords
	words := make([]*model.SensitiveWord, 0, len(candidates))
	invalid := 0
	for _, candidate := range candidates {
		if len(words) >= importLimit {
			break
		}
		word := &model.SensitiveWord{
			Word:     candidate,
			Category: category,
			Enabled:  true,
		}
		// 校验放在这里而不是让仓储整体报错：批量导入里混入一两个超长行（例如
		// 误粘贴了一整句）是常态，为此让整批失败会让站长反复试错。
		if err := word.Validate(); err != nil {
			invalid++
			continue
		}
		words = append(words, word)
	}

	imported, err := s.deps.SensitiveWords.CreateMany(c.Request.Context(), words)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_sensitive_word")
		return
	}

	s.invalidateSensitiveFilter()
	c.JSON(http.StatusOK, gin.H{
		"imported": imported,
		// skipped：被判定为非法的行（空行已在切分时去掉，这里是长度不合法）
		"skipped_invalid": invalid,
		"total":           len(candidates),
	})
}

// handleUpdateSensitiveWord 更新敏感词。
func (s *Server) handleUpdateSensitiveWord(c *gin.Context) {
	if s.deps.SensitiveWords == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"敏感词模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req sensitiveWordUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	ctx := c.Request.Context()
	word, err := s.deps.SensitiveWords.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrSensitiveWordNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "敏感词不存在", oai.TypeInvalidRequest, "sensitive_word_not_found")
			return
		}
		s.respondInternalError(c, "查询敏感词失败")
		return
	}

	if strings.TrimSpace(req.Word) != "" {
		word.Word = req.Word
	}
	if category := strings.TrimSpace(req.Category); category != "" {
		word.Category = category
	}
	if remark := strings.TrimSpace(req.Remark); remark != "" {
		word.Remark = remark
	}
	if req.Enabled != nil {
		word.Enabled = *req.Enabled
	}

	if err := s.deps.SensitiveWords.Update(ctx, word); err != nil {
		if errors.Is(err, model.ErrSensitiveWordDuplicated) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"该敏感词已存在", oai.TypeInvalidRequest, "sensitive_word_duplicated")
			return
		}
		if errors.Is(err, model.ErrSensitiveWordNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "敏感词不存在", oai.TypeInvalidRequest, "sensitive_word_not_found")
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_sensitive_word")
		return
	}

	s.invalidateSensitiveFilter()
	c.JSON(http.StatusOK, toSensitiveWordDTO(word))
}

// handleDeleteSensitiveWord 删除敏感词。
func (s *Server) handleDeleteSensitiveWord(c *gin.Context) {
	if s.deps.SensitiveWords == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"敏感词模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	if err := s.deps.SensitiveWords.Delete(c.Request.Context(), id); err != nil {
		if errors.Is(err, model.ErrSensitiveWordNotFound) {
			oai.WriteError(c.Writer, http.StatusNotFound, "敏感词不存在", oai.TypeInvalidRequest, "sensitive_word_not_found")
			return
		}
		s.respondInternalError(c, "删除敏感词失败")
		return
	}

	s.invalidateSensitiveFilter()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// invalidateSensitiveFilter 让过滤中间件丢弃缓存的词表，使改动立即生效。
func (s *Server) invalidateSensitiveFilter() {
	if s.sensitiveFilter != nil {
		s.sensitiveFilter.Invalidate()
	}
}

// splitSensitiveInput 把一段自由文本切分为候选词条。
//
// 分隔符覆盖常见粘贴格式：换行、英文/中文逗号、分号、顿号、制表符与竖线。
// 之所以支持这么多种：站长从不同来源（txt / Excel / 聊天记录）复制粘贴时，
// 分隔符是不可控的；多认几种只是几行代码，却能把"粘进去变成一条超长词"的坑填掉。
//
// 结果已去重（同一批里重复出现的词只保留一次），顺序保持不变。
func splitSensitiveInput(text string) []string {
	normalized := strings.NewReplacer(
		"，", "\n",
		",", "\n",
		"、", "\n",
		"；", "\n",
		";", "\n",
		"|", "\n",
		"\t", "\n",
		"\r\n", "\n",
		"\r", "\n",
	).Replace(text)

	seen := make(map[string]struct{})
	result := make([]string, 0, 64)
	for _, line := range strings.Split(normalized, "\n") {
		candidate := strings.TrimSpace(line)
		if candidate == "" {
			continue
		}
		key := strings.ToLower(candidate)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, candidate)
	}
	return result
}
