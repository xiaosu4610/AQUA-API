// 安装向导与超管入口（仅密码登录）的单元测试。
//
// 测试重点（都是"装错了会出事"的性质）：
//   - 未安装时安装向导可用；安装成功后安装接口必须【永久自锁】（409）；
//   - 超管入口只凭密码即可登录，密码错误一律 401；
//   - 未安装时超管入口也返回 401（不泄露"该站点是否已初始化"）。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/config"
	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
)

// newInstallTestServer 装配一个"未安装"的测试服务（库里没有任何管理员）。
func newInstallTestServer(t *testing.T) *Server {
	t.Helper()
	gin.DefaultWriter = io.Discard

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "install_test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	cipher, err := crypto.New(testEncryptionKey)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}

	userRepo := store.NewUserRepository(st.DB())
	cfg := config.Default()
	cfg.Server.Mode = "test"
	cfg.Server.Listen = "127.0.0.1:0"

	return New(Deps{
		Config:   cfg,
		Store:    st,
		Channels: store.NewChannelRepository(st.DB(), cipher),
		Users:    userRepo,
		Sessions: store.NewSessionRepository(st.DB()),
		Tokens:   store.NewTokenRepository(st.DB(), cipher),
		Settings: store.NewSettingRepository(st.DB(), st.Dialect()),
	})
}

// postJSON 发送一次 JSON 请求并返回状态码与解析后的响应体。
func postJSON(t *testing.T, srv *Server, method, path string, payload any) (int, map[string]any) {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body
}

// TestInstall_安装后接口自锁 验证"装完即关闭"这一关键安全属性。
func TestInstall_安装后接口自锁(t *testing.T) {
	srv := newInstallTestServer(t)

	code, body := postJSON(t, srv, http.MethodGet, "/api/install/status", nil)
	if code != http.StatusOK {
		t.Fatalf("未安装时 status 状态码 = %d，期望 200", code)
	}
	if installed, _ := body["installed"].(bool); installed {
		t.Fatal("空库应报告未安装")
	}

	code, body = postJSON(t, srv, http.MethodPost, "/api/install", map[string]any{
		"username":         "admin",
		"password":         "Aqua-Install-2026",
		"confirm_password": "Aqua-Install-2026",
		"site_name":        "测试站点",
	})
	if code != http.StatusOK {
		t.Fatalf("安装状态码 = %d，期望 200（响应 %v）", code, body)
	}

	if code, body = postJSON(t, srv, http.MethodGet, "/api/install/status", nil); code != http.StatusOK {
		t.Fatalf("安装后 status 状态码 = %d", code)
	}
	if installed, _ := body["installed"].(bool); !installed {
		t.Fatal("安装后应报告已安装")
	}
	if name, _ := body["site_name"].(string); name != "测试站点" {
		t.Errorf("站点名称 = %q，期望「测试站点」", name)
	}

	// 第二次安装必须被拒绝，否则任何人都能重装并接管站点
	code, _ = postJSON(t, srv, http.MethodPost, "/api/install", map[string]any{
		"username": "hacker",
		"password": "Aqua-Install-2026",
	})
	if code != http.StatusConflict {
		t.Fatalf("重复安装状态码 = %d，期望 409", code)
	}
}

// TestInstall_密码强度不足被拒 验证向导复用注册的同一套口令规则。
func TestInstall_密码强度不足被拒(t *testing.T) {
	srv := newInstallTestServer(t)

	code, _ := postJSON(t, srv, http.MethodPost, "/api/install", map[string]any{
		"password": "",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("空密码状态码 = %d，期望 400", code)
	}

	code, _ = postJSON(t, srv, http.MethodPost, "/api/install", map[string]any{
		"password":         "Aqua-Install-2026",
		"confirm_password": "Aqua-Install-2027",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("两次密码不一致状态码 = %d，期望 400", code)
	}
}

// TestAdminLogin_仅密码登录 验证超管入口不需要用户名。
func TestAdminLogin_仅密码登录(t *testing.T) {
	srv := newInstallTestServer(t)

	// 未安装：必须与"密码错误"返回完全相同的状态码，不泄露站点是否已初始化
	if code, _ := postJSON(t, srv, http.MethodPost, "/api/auth/admin-login", map[string]any{"password": "whatever"}); code != http.StatusUnauthorized {
		t.Fatalf("未安装时 admin-login 状态码 = %d，期望 401", code)
	}

	if code, body := postJSON(t, srv, http.MethodPost, "/api/install", map[string]any{
		"password": "Aqua-Install-2026",
	}); code != http.StatusOK {
		t.Fatalf("安装失败，状态码 = %d（%v）", code, body)
	}

	code, body := postJSON(t, srv, http.MethodPost, "/api/auth/admin-login", map[string]any{
		"password": "wrong-password",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误密码状态码 = %d，期望 401", code)
	}

	code, body = postJSON(t, srv, http.MethodPost, "/api/auth/admin-login", map[string]any{
		"password": "Aqua-Install-2026",
	})
	if code != http.StatusOK {
		t.Fatalf("正确密码状态码 = %d，期望 200（%v）", code, body)
	}
	token, _ := body["session_token"].(string)
	if token == "" {
		t.Fatal("仅密码登录应返回会话令牌")
	}
	user, _ := body["user"].(map[string]any)
	if role, _ := user["role"].(float64); int(role) != int(model.UserRoleAdmin) {
		t.Errorf("登录身份 role = %v，期望管理员(%d)", user["role"], model.UserRoleAdmin)
	}
}
