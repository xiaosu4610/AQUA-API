// 本文件实现「安装向导」与「超管独立入口」两类接口。
//
// 意图（Why）：
//
//	自托管网关的第一次使用门槛在"怎么把第一个人管起来"：新部署的实例里
//	一个账号都没有，站长必须先建出管理员才能进后台配渠道。此前只能靠
//	命令行 `aqua -init-admin <用户名>`（随机密码打印在终端），对不熟悉
//	SSH 的使用者过于苛刻。安装向导把这一步搬到浏览器里：
//
//	  1) 系统未安装（尚无任何管理员）时，访问 /install 即可设置管理员与站点名；
//	  2) 系统已安装后，/install 与安装接口【永久关闭】（409），
//	     这是本文件最重要的安全属性——否则任何人都能"重装"并接管站点；
//	  3) 后台入口只要求密码（不要求用户名）：单管理员部署下多输一个用户名
//	     没有任何安全增益，却让"进后台"这件事多一步。为避免密码比对成为
//	     CPU 放大器，管理员数量超过上限时改要求用户名（见 maxAdminLookupLimit）。
//
// 流转（Flow）：
//
//	GET  /api/install/status    → handleInstallStatus → 是否已安装 + 站点名（未安装时附数据库信息）
//	POST /api/install           → handleInstall       → 建管理员（可选写站点名）→ 之后接口自锁
//	POST /api/auth/admin-login  → handleAdminLogin    → 按密码匹配管理员 → 签发会话
//
//	安装判定唯一依据：users 表中是否存在 role=管理员 的记录。不额外落"已安装"标记，
//	避免标记与真实数据不一致（例如管理员被删光后标记仍为已安装，站点将无法再初始化）。
//
// 扩展（Extend）：
//
//	向导若要增加步骤（如测试数据库连通性）：在 installStatusResponse 加字段
//	告知前端"还差哪一步"，并在此文件补对应的只读/写入处理器；
//	所有安装类接口都必须复用 installedAlready 做自锁，切勿只在前端隐藏入口。
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/version"
)

// 安装向导中管理员用户名的默认值与长度上限。
const (
	defaultAdminUsername = "admin"
	// adminUsernameMaxLength 与用户表列宽相匹配，避免超长写入被数据库截断。
	adminUsernameMaxLength = 64
)

// installStatusResponse 是安装状态响应。
//
// 字段设计说明：数据库信息【只在未安装时】返回。安装完成后前端只需要知道
// "已完成过安装"，此时连驱动类型都不必暴露（减少无意义的信息面）。
type installStatusResponse struct {
	Installed bool   `json:"installed"`
	SiteName  string `json:"site_name"`
	Version   string `json:"version"`
	// DatabaseDriver 仅在未安装时返回（如 sqlite）。
	DatabaseDriver string `json:"database_driver,omitempty"`
	// SQLiteZeroConfig 表示当前使用免配置的 SQLite（未安装时返回）。
	SQLiteZeroConfig bool `json:"sqlite_zero_config,omitempty"`
}

// handleInstallStatus 处理 GET /api/install/status。
//
// 该接口必须公开：安装向导在登录之前就要能判断"该装还是该登录"。
func (s *Server) handleInstallStatus(c *gin.Context) {
	ctx := c.Request.Context()

	installed, err := s.installedAlready(ctx)
	if err != nil {
		s.respondInternalError(c, "读取安装状态失败")
		return
	}

	resp := installStatusResponse{
		Installed: installed,
		Version:   version.Get().Version,
	}

	// 站点名始终返回：它本来就是对公众可见的信息（见 /api/status），
	// 且已安装后前端仍需要它来渲染"这是哪个站点"的引导文案。
	settings, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil {
		s.respondInternalError(c, "读取站点设置失败")
		return
	}
	resp.SiteName = settings.SiteName

	// 数据库形态只在未安装时返回：安装完成后前端不再需要，
	// 少暴露一点运行环境信息就少一点被针对性利用的可能。
	if !installed && s.deps.Config != nil {
		resp.DatabaseDriver = s.deps.Config.Database.Driver
		resp.SQLiteZeroConfig = s.deps.Config.Database.Driver == "sqlite"
	}
	c.JSON(http.StatusOK, resp)
}

// installRequest 是安装请求体。
type installRequest struct {
	// Username 留空时使用 admin（单管理员部署下用户名不是需要用户思考的事）。
	Username string `json:"username"`
	Password string `json:"password"`
	// ConfirmPassword 仅在前端填写时校验一致性；不填则跳过（服务端不强制）。
	ConfirmPassword string `json:"confirm_password"`
	// SiteName 可选：写入站点名称设置，留空则保持默认。
	SiteName string `json:"site_name"`
}

// handleInstall 处理 POST /api/install（首次安装）。
//
// 幂等与并发：安装判定与写入之间没有事务保护，但用户表的用户名唯一索引
// 会兜住"两次并发安装"——后到的那个创建失败，返回明确错误，不会产生两个管理员。
func (s *Server) handleInstall(c *gin.Context) {
	ctx := c.Request.Context()

	installed, err := s.installedAlready(ctx)
	if err != nil {
		s.respondInternalError(c, "读取安装状态失败")
		return
	}
	if installed {
		// 已安装即永久关闭：这是防止"任意人重装并接管站点"的关键闸门
		oai.WriteError(c.Writer, http.StatusConflict,
			"系统已完成安装，安装入口已关闭。如需重置管理员密码，请在服务器上执行 aqua -reset-password",
			oai.TypePermission, "already_installed")
		return
	}

	var req installRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = defaultAdminUsername
	}
	if len(username) > adminUsernameMaxLength {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"管理员用户名过长", oai.TypeInvalidRequest, "invalid_username")
		return
	}
	if strings.ContainsAny(username, " \t\r\n") {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"管理员用户名不能包含空白字符", oai.TypeInvalidRequest, "invalid_username")
		return
	}

	password := req.Password
	if strings.TrimSpace(password) == "" {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"请设置管理员密码", oai.TypeInvalidRequest, "missing_password")
		return
	}
	// 复用注册/改密同一套强度规则，避免出现"向导设的密码反而注册不合规"的不一致
	if err := crypto.ValidatePasswordStrength(password); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "weak_password")
		return
	}
	if req.ConfirmPassword != "" && req.ConfirmPassword != password {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"两次输入的密码不一致", oai.TypeInvalidRequest, "password_mismatch")
		return
	}

	hash, err := crypto.HashPassword(password)
	if err != nil {
		s.respondInternalError(c, "计算口令哈希失败")
		return
	}

	admin := &model.User{
		Username:     username,
		PasswordHash: hash,
		Role:         model.UserRoleAdmin,
		Status:       model.UserStatusEnabled,
		// 管理员不限额度：其自身调用不应因额度耗尽被拒绝
		Quota: model.QuotaUnlimited,
	}
	if err := s.deps.Users.Create(ctx, admin); err != nil {
		if errors.Is(err, model.ErrUsernameTaken) {
			// 并发安装或用户名被占用：仍返回 409，语义是"不能重复安装"
			oai.WriteError(c.Writer, http.StatusConflict,
				"该用户名已存在，或系统已完成安装", oai.TypePermission, "already_installed")
			return
		}
		s.respondInternalError(c, "创建管理员失败")
		return
	}

	// 站点名称是可选步骤：写失败不影响安装完成（管理员已建好，站点已可用），
	// 但必须留下日志——否则站长会发现"向导里填的名称没生效"却查不到原因。
	if name := strings.TrimSpace(req.SiteName); name != "" {
		if err := s.deps.Settings.Set(ctx, model.SettingKeySiteName, name); err != nil {
			slog.Warn("安装向导写入站点名称失败（不影响安装完成）", "error", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":               true,
		"admin_username":   admin.Username,
		"admin_login_path": "/admin/login",
	})
}

// adminLoginRequest 是超管入口的登录请求体：只要密码。
type adminLoginRequest struct {
	Password string `json:"password"`
}

// handleAdminLogin 处理 POST /api/auth/admin-login（超管入口，仅密码）。
//
// 为什么允许只输密码：自托管站点通常只有一个管理员，多输一个用户名
// 只增加操作成本，不带来安全增益。代价是服务端必须枚举管理员逐个比对
// 口令哈希（bcrypt 是刻意昂贵的操作），因此：
//   - 枚举数量有硬上限（maxAdminLookupLimit，仓储层再夹一次）；
//   - 超过上限时明确拒绝并提示改用"用户名 + 密码"登录（见 handleLogin）；
//   - 接口本身与普通登录共用限流器（在 router.go 中挂载）。
func (s *Server) handleAdminLogin(c *gin.Context) {
	ctx := c.Request.Context()

	var req adminLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUserError(c, http.StatusBadRequest,
			"request.invalid_json", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		writeUserError(c, http.StatusBadRequest,
			"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}

	total, err := s.deps.Users.CountAdmins(ctx)
	if err != nil {
		s.respondInternalError(c, "读取管理员信息失败")
		return
	}
	if total == 0 {
		// 尚未安装：执行一次比对抹平耗时，再返回与"口令错误"完全相同的提示，
		// 避免未鉴权接口被用来探测"该站点是否已初始化"。
		// （前端应当先查 /api/install/status 并引导到安装向导。）
		crypto.VerifyPassword(req.Password, dummyPasswordHash)
		writeUserError(c, http.StatusUnauthorized,
			"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}
	if total > maxAdminLookupLimitForLogin {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"本站管理员数量较多，请改用「用户名 + 密码」的方式登录",
			oai.TypeInvalidRequest, "too_many_admins")
		return
	}

	admins, err := s.deps.Users.ListAdmins(ctx, total)
	if err != nil {
		s.respondInternalError(c, "读取管理员信息失败")
		return
	}

	for _, admin := range admins {
		if !crypto.VerifyPassword(req.Password, admin.PasswordHash) {
			continue
		}
		if !admin.IsActive() {
			writeUserError(c, http.StatusForbidden,
				"auth.account_disabled", oai.TypePermission, oai.CodeTokenDisabled)
			return
		}
		s.issueSession(c, admin, http.StatusOK)
		return
	}

	writeUserError(c, http.StatusUnauthorized,
		"auth.invalid_credentials", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
}

// maxAdminLookupLimitForLogin 是本层接受"仅密码登录"的管理员数量上限。
//
// 与仓储层 maxAdminLookupLimit 保持同一量级：仓储负责"绝不返回过多数据"，
// 这一层负责"给出可读的拒绝理由"，两者都不可省。
const maxAdminLookupLimitForLogin = 5

// installedAlready 判断系统是否已完成安装。
//
// 判据：是否存在任何管理员账号。刻意不落"已安装"设置项——
// 设置项与真实数据可能不一致（管理员被删光后设置仍为已安装，站点就再也无法初始化）。
func (s *Server) installedAlready(ctx context.Context) (bool, error) {
	count, err := s.deps.Users.CountAdmins(ctx)
	if err != nil {
		return false, fmt.Errorf("server: 统计管理员数量失败: %w", err)
	}
	return count > 0, nil
}
