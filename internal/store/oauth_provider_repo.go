// 本文件是 model.OAuthProviderRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	把"去哪刷新令牌、用什么 client 凭据"落到数据库，使开源使用者无需改代码
//	即可接入任意 OAuth2 平台。ClientSecret 属于密钥类配置，落库加密、读取解密。
//
// 流转（Flow）：
//
//	NewOAuthProviderRepository(db, cipher)
//	  ├─ 后台维护：Create / Update / Delete / List
//	  └─ 刷新令牌：GetByName → relay.OAuthRefresher 使用
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 columns / scan / insert / update 四处。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// oauthProviderColumns 集中定义查询列，顺序必须与 scanOAuthProvider 严格一致。
const oauthProviderColumns = `id, name, token_url, client_id, client_secret_enc, scope, remark, enabled, created_at, updated_at`

// oauthProviderRepository 是 model.OAuthProviderRepository 的 SQL 实现。
type oauthProviderRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewOAuthProviderRepository 创建 OAuth 提供方仓储。
func NewOAuthProviderRepository(db *sql.DB, cipher *crypto.Cipher) model.OAuthProviderRepository {
	return &oauthProviderRepository{db: db, cipher: cipher}
}

// Create 新增提供方配置。
func (r *oauthProviderRepository) Create(ctx context.Context, provider *model.OAuthProvider) error {
	if err := provider.Validate(); err != nil {
		return fmt.Errorf("store: OAuth 提供方配置非法: %w", err)
	}

	secretEnc, err := r.encryptSecret(provider.ClientSecret)
	if err != nil {
		return err
	}

	now := time.Now()
	provider.Name = strings.ToLower(strings.TrimSpace(provider.Name))
	provider.TokenURL = strings.TrimSpace(provider.TokenURL)
	provider.CreatedAt = now
	provider.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO oauth_providers (name, token_url, client_id, client_secret_enc, scope, remark, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		provider.Name, provider.TokenURL, strings.TrimSpace(provider.ClientID), secretEnc,
		strings.TrimSpace(provider.Scope), provider.Remark, boolToInt(provider.Enabled),
		provider.CreatedAt.Unix(), provider.UpdatedAt.Unix(),
	)
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return fmt.Errorf("store: OAuth 提供方 %q 已存在", provider.Name)
		}
		return fmt.Errorf("store: 新增 OAuth 提供方失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增提供方的 ID 失败: %w", err)
	}
	provider.ID = uint64(id)
	return nil
}

// GetByName 按名称查询。
func (r *oauthProviderRepository) GetByName(ctx context.Context, name string) (*model.OAuthProvider, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+oauthProviderColumns+" FROM oauth_providers WHERE name = ?",
		strings.ToLower(strings.TrimSpace(name)))

	provider, err := r.scanOAuthProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrOAuthProviderNotFound
		}
		return nil, err
	}
	return provider, nil
}

// List 返回全部提供方配置（按名称升序）。
func (r *oauthProviderRepository) List(ctx context.Context) ([]*model.OAuthProvider, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+oauthProviderColumns+" FROM oauth_providers ORDER BY name ASC")
	if err != nil {
		return nil, fmt.Errorf("store: 查询 OAuth 提供方列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	providers := make([]*model.OAuthProvider, 0, 8)
	for rows.Next() {
		provider, err := r.scanOAuthProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 OAuth 提供方失败: %w", err)
	}
	return providers, nil
}

// Update 按 ID 更新配置。
//
// ClientSecret 语义与其他密钥字段一致：传空字符串表示"不修改"，
// 避免管理员只改备注却把密钥清空。
func (r *oauthProviderRepository) Update(ctx context.Context, provider *model.OAuthProvider) error {
	if provider.ID == 0 {
		return errors.New("store: 更新 OAuth 提供方时 ID 不能为 0")
	}
	if err := provider.Validate(); err != nil {
		return fmt.Errorf("store: OAuth 提供方配置非法: %w", err)
	}

	provider.UpdatedAt = time.Now()
	provider.Name = strings.ToLower(strings.TrimSpace(provider.Name))
	provider.TokenURL = strings.TrimSpace(provider.TokenURL)

	var res sql.Result
	var err error
	if strings.TrimSpace(provider.ClientSecret) != "" {
		secretEnc, encErr := r.encryptSecret(provider.ClientSecret)
		if encErr != nil {
			return encErr
		}
		res, err = r.db.ExecContext(ctx, `
			UPDATE oauth_providers SET
				name = ?, token_url = ?, client_id = ?, client_secret_enc = ?,
				scope = ?, remark = ?, enabled = ?, updated_at = ?
			WHERE id = ?`,
			provider.Name, provider.TokenURL, strings.TrimSpace(provider.ClientID), secretEnc,
			strings.TrimSpace(provider.Scope), provider.Remark, boolToInt(provider.Enabled),
			provider.UpdatedAt.Unix(), provider.ID)
	} else {
		res, err = r.db.ExecContext(ctx, `
			UPDATE oauth_providers SET
				name = ?, token_url = ?, client_id = ?,
				scope = ?, remark = ?, enabled = ?, updated_at = ?
			WHERE id = ?`,
			provider.Name, provider.TokenURL, strings.TrimSpace(provider.ClientID),
			strings.TrimSpace(provider.Scope), provider.Remark, boolToInt(provider.Enabled),
			provider.UpdatedAt.Unix(), provider.ID)
	}
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return fmt.Errorf("store: OAuth 提供方 %q 已存在", provider.Name)
		}
		return fmt.Errorf("store: 更新 OAuth 提供方 %d 失败: %w", provider.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrOAuthProviderNotFound
	}
	return nil
}

// Delete 按 ID 删除配置。
func (r *oauthProviderRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM oauth_providers WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除 OAuth 提供方 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrOAuthProviderNotFound
	}
	return nil
}

// encryptSecret 加密客户端密钥；空串返回空串。
func (r *oauthProviderRepository) encryptSecret(secret string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", nil
	}
	encrypted, err := r.cipher.Encrypt(secret)
	if err != nil {
		return "", fmt.Errorf("store: 加密 OAuth 客户端密钥失败: %w", err)
	}
	return encrypted, nil
}

// scanOAuthProvider 把一行数据映射为提供方对象并完成解密。
func (r *oauthProviderRepository) scanOAuthProvider(sc rowScanner) (*model.OAuthProvider, error) {
	var (
		id        uint64
		name      string
		tokenURL  string
		clientID  string
		secretEnc string
		scope     string
		remark    string
		enabled   int
		createdAt int64
		updatedAt int64
	)

	if err := sc.Scan(&id, &name, &tokenURL, &clientID, &secretEnc, &scope,
		&remark, &enabled, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取 OAuth 提供方字段失败: %w", err)
	}

	secret := ""
	if secretEnc != "" {
		plain, err := r.cipher.Decrypt(secretEnc)
		if err != nil {
			return nil, fmt.Errorf("store: 解密提供方 %d 的客户端密钥失败: %w", id, err)
		}
		secret = plain
	}

	return &model.OAuthProvider{
		ID:           id,
		Name:         name,
		TokenURL:     tokenURL,
		ClientID:     clientID,
		ClientSecret: secret,
		Scope:        scope,
		Remark:       remark,
		Enabled:      enabled != 0,
		CreatedAt:    time.Unix(createdAt, 0),
		UpdatedAt:    time.Unix(updatedAt, 0),
	}, nil
}
