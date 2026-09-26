// 邀请返利 / 每日签到设置项的单元测试。
//
// 为什么要单独一个文件：setting_test.go 是共享文件（其他任务也在改），
// 为避免冲突，本任务新增设置项的断言集中写在这里。
//
// 测试重点：
//   - 默认值必须"全部关闭、额度为 0"（避免升级后站点在站长不知情时开始发额度）；
//   - ToMap / LoadSiteSettings 往返一致（三处同步是否真的都补上了）；
//   - 越界/脏值必须回退默认（防止"比例 150""负额度"这类会直接资损的值进入运行期）。
package model

import (
	"context"
	"testing"
)

func TestDefaultSiteSettings_邀请与签到默认关闭(t *testing.T) {
	settings := DefaultSiteSettings()

	if settings.Referral.Enabled {
		t.Fatal("默认不应开启邀请返利（发额度的功能必须由站长显式开启）")
	}
	if settings.Referral.RegisterBonus != 0 {
		t.Fatalf("默认邀请注册奖励应为 0，实际 %d", settings.Referral.RegisterBonus)
	}
	if settings.Referral.RechargeRatio != 0 {
		t.Fatalf("默认充值返利比例应为 0，实际 %d", settings.Referral.RechargeRatio)
	}
	if settings.Referral.CheckinEnabled {
		t.Fatal("默认不应开启签到")
	}
	if settings.Referral.CheckinDailyQuota != 0 {
		t.Fatalf("默认签到额度应为 0，实际 %d", settings.Referral.CheckinDailyQuota)
	}
}

func TestSiteSettings_邀请与签到_ToMap与Load往返一致(t *testing.T) {
	original := DefaultSiteSettings()
	original.Referral = ReferralSettings{
		Enabled:           true,
		RegisterBonus:     500,
		RechargeRatio:     10,
		CheckinEnabled:    true,
		CheckinDailyQuota: 66,
	}

	repo := &fakeSettingRepo{values: original.ToMap()}
	loaded, err := LoadSiteSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	if loaded.Referral != original.Referral {
		t.Fatalf("邀请/签到设置未往返一致：\n得到 %+v\n期望 %+v", loaded.Referral, original.Referral)
	}
}

func TestLoadSiteSettings_邀请越界值回退默认(t *testing.T) {
	repo := &fakeSettingRepo{values: map[string]string{
		SettingKeyReferralEnabled:       "true",
		SettingKeyReferralRegisterBonus: "-100", // 负数非法
		SettingKeyReferralRechargeRatio: "150",  // 超过 100 非法
		SettingKeyCheckinEnabled:        "true",
		SettingKeyCheckinDailyQuota:     "9999999999999", // 超过上限非法
	}}

	loaded, err := LoadSiteSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	defaults := DefaultSiteSettings()
	if loaded.Referral.RegisterBonus != defaults.Referral.RegisterBonus {
		t.Fatalf("负数奖励应回退默认 %d，实际 %d",
			defaults.Referral.RegisterBonus, loaded.Referral.RegisterBonus)
	}
	if loaded.Referral.RechargeRatio != defaults.Referral.RechargeRatio {
		t.Fatalf("越界比例应回退默认 %d，实际 %d",
			defaults.Referral.RechargeRatio, loaded.Referral.RechargeRatio)
	}
	if loaded.Referral.CheckinDailyQuota != defaults.Referral.CheckinDailyQuota {
		t.Fatalf("越界签到额度应回退默认 %d，实际 %d",
			defaults.Referral.CheckinDailyQuota, loaded.Referral.CheckinDailyQuota)
	}
	// 合法布尔项应正常生效
	if !loaded.Referral.Enabled || !loaded.Referral.CheckinEnabled {
		t.Fatal("合法的开关项应被解析为 true")
	}
}

func TestValidateReferralQuota与ValidateRechargeRatio_边界(t *testing.T) {
	if err := ValidateReferralQuota("额度", 0); err != nil {
		t.Fatalf("0 应合法: %v", err)
	}
	if err := ValidateReferralQuota("额度", maxReferralQuota); err != nil {
		t.Fatalf("上限值应合法: %v", err)
	}
	if err := ValidateReferralQuota("额度", -1); err == nil {
		t.Fatal("负数应非法")
	}
	if err := ValidateReferralQuota("额度", maxReferralQuota+1); err == nil {
		t.Fatal("超过上限应非法")
	}

	if err := ValidateRechargeRatio(0); err != nil {
		t.Fatalf("0 应合法: %v", err)
	}
	if err := ValidateRechargeRatio(100); err != nil {
		t.Fatalf("100 应合法: %v", err)
	}
	if err := ValidateRechargeRatio(101); err == nil {
		t.Fatal("超过 100 应非法")
	}
}
