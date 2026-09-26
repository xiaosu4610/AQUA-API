// 本文件实现「人工确认」通道。
//
// 意图（Why）：
//
//	并非所有部署都需要在线支付：自用/内网/小圈子部署往往由站长收钱后
//	手动给账号加额度。若只有在线支付通道，这类部署就只能去改数据库，
//	既容易出错，也没有任何记录可查。
//
//	人工通道把这件事变成正规流程：
//	  用户提交充值申请（生成待支付订单）→ 站长在后台核对收款 →
//	  点击"确认入账"（复用与在线支付完全相同的幂等入账路径）。
//
// 为什么不把它做成"自动检测"：
//
//	人工通道没有第三方回调，任何"自动确认"都等于"无需付款即可到账"。
//	因此本通道的 Create 只生成订单，绝不返回支付地址；
//	ParseNotify 直接拒绝——从设计上就杜绝误用的可能。
//
// 流转（Flow）：
//
//	用户下单：Create → 返回空 PayURL（界面提示"请联系管理员确认"）
//	站长入账：后台 POST /api/admin/orders/{tradeNo}/mark-paid
//	          → 与在线支付共用 MarkPaid + AddQuota 的幂等入账逻辑
//
// 扩展（Extend）：
//
//	若将来要接"线下转账 + 上传凭证"，把凭证作为一个字段加到订单 remark 即可，
//	无需改动本通道。
package payment

import (
	"context"
	"fmt"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// manualProvider 实现人工确认通道。
type manualProvider struct {
	opts Options
}

// newManualProvider 构造人工确认通道。
func newManualProvider(opts Options) Provider {
	return &manualProvider{opts: opts}
}

// Name 返回通道名。
func (p *manualProvider) Name() string { return model.PaymentMethodManual }

// Create 生成订单但不需要跳转任何支付页面。
func (p *manualProvider) Create(ctx context.Context, _ *Request) (*CreateResult, error) {
	settings, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.MethodEnabled(model.PaymentMethodManual) {
		return nil, ErrProviderDisabled
	}
	// 刻意返回空地址：界面据此提示"请按站长公布的方式付款后等待确认"，
	// 而不是跳到一个不存在的收银台。
	return &CreateResult{}, nil
}

// ParseNotify 人工通道没有回调，一律拒绝。
//
// 返回明确的错误而不是静默成功：若有人尝试伪造人工通道的回调，
// 应当在日志里留下痕迹，而不是被当成正常入账。
func (p *manualProvider) ParseNotify(_ context.Context, _ *Notify) (*NotifyResult, error) {
	return nil, fmt.Errorf("%w：人工确认通道只能由管理员在后台入账", ErrUnsupportedNotify)
}

// settings 读取当前运营参数。
func (p *manualProvider) settings(ctx context.Context) (model.PaymentSettings, error) {
	if p.opts.Settings == nil {
		return model.PaymentSettings{}, nil
	}
	return p.opts.Settings(ctx)
}
