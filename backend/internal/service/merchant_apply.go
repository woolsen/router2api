package service

import (
	"context"
	"math"
	"strings"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
	"github.com/woolsen/router2api/internal/pkg/logger"
)

const merchantApplyDefaultRateMultiplier = 1.0

var (
	// ErrMerchantAlreadyBound 表示该用户已经绑定商户，不能重复申请。
	ErrMerchantAlreadyBound = infraerrors.Conflict("MERCHANT_ALREADY_BOUND", "user is already bound to a merchant")
	// ErrMerchantApplyForbidden 表示当前角色（如管理员）不允许走商户自助申请。
	ErrMerchantApplyForbidden = infraerrors.Forbidden("MERCHANT_APPLY_FORBIDDEN", "only regular users can apply for a merchant account")
	// ErrMerchantRateInvalid 表示申请中的分组倍率非法。
	ErrMerchantRateInvalid = infraerrors.BadRequest("MERCHANT_RATE_INVALID", "rate_multiplier must be a positive number")
)

// ApplyInput 是商户自助入驻申请。Models 与 RateMultiplier 来自申请者在前端
// 探测结果上的确认；Models 为空时回落到探测返回的模型列表。
type ApplyInput struct {
	MerchantName   string
	BaseURL        string
	APIKey         string
	Platform       string
	RateMultiplier float64
	Models         []string
}

// MerchantApplyUserRepository 是申请流程所需的用户读取与角色绑定能力。
type MerchantApplyUserRepository interface {
	GetByID(ctx context.Context, id int64) (*User, error)
	PromoteToMerchant(ctx context.Context, userID, merchantID int64) error
}

// MerchantApplyService 把一次通过的探测变成商户、分组、上游账号与货架模型。
type MerchantApplyService struct {
	probe      *ProbeService
	merchants  *MerchantService
	groups     GroupRepository
	onboarding *MerchantOnboardingService
	users      MerchantApplyUserRepository
}

func NewMerchantApplyService(
	probe *ProbeService,
	merchants *MerchantService,
	groups GroupRepository,
	onboarding *MerchantOnboardingService,
	users MerchantApplyUserRepository,
) *MerchantApplyService {
	return &MerchantApplyService{
		probe:      probe,
		merchants:  merchants,
		groups:     groups,
		onboarding: onboarding,
		users:      users,
	}
}

// Submit 执行自助入驻：探测 → 建商户 → 建分组 → 接入上游账号 → 写货架模型
// → 把用户升级为 merchant 并绑定 merchant_id。
//
// 顺序不能调换：AttachGroup 要求分组已存在，且只有 ConfigureGroupModels 写入
// 渠道模型后，ModelPlazaService.ListGroups（货架与 /model-plaza 的数据源）
// 才能枚举到这个分组。
func (s *MerchantApplyService) Submit(ctx context.Context, userID int64, in ApplyInput) (*Merchant, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user.Role == RoleMerchant || user.MerchantID != nil {
		return nil, ErrMerchantAlreadyBound
	}
	if user.Role != RoleUser {
		return nil, ErrMerchantApplyForbidden
	}

	name := strings.TrimSpace(in.MerchantName)
	platform := strings.ToLower(strings.TrimSpace(in.Platform))
	if platform != PlatformOpenAI && platform != PlatformAnthropic {
		return nil, ErrPlatformMismatch
	}
	rate := in.RateMultiplier
	if rate == 0 {
		rate = merchantApplyDefaultRateMultiplier
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, ErrMerchantRateInvalid
	}

	// 先探测再落库：探测失败时不允许留下孤儿商户/分组。
	probeResult, err := s.probe.Probe(ctx, in.BaseURL, in.APIKey, platform)
	if err != nil {
		return nil, err
	}
	models := normalizeChannelModelNames(in.Models)
	if len(models) == 0 {
		models = normalizeChannelModelNames(probeResult.Models)
	}
	if len(models) == 0 {
		return nil, ErrProbeModelsEmpty
	}

	exists, err := s.groups.ExistsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrGroupExists
	}

	merchant, err := s.merchants.Create(ctx, CreateMerchantInput{Name: name})
	if err != nil {
		return nil, err
	}

	group := &Group{
		Name:             name,
		Platform:         platform,
		RateMultiplier:   rate,
		Status:           StatusActive,
		SubscriptionType: SubscriptionTypeStandard,
	}
	if err := s.groups.Create(ctx, group); err != nil {
		return nil, err
	}

	if _, err := s.onboarding.AttachGroup(ctx, AttachGroupInput{
		MerchantID: merchant.ID,
		GroupID:    group.ID,
		BaseURL:    in.BaseURL,
		APIKey:     in.APIKey,
		Platform:   platform,
	}); err != nil {
		return nil, err
	}

	if err := s.onboarding.ConfigureGroupModels(ctx, group.ID, platform, models); err != nil {
		return nil, err
	}

	if err := s.users.PromoteToMerchant(ctx, userID, merchant.ID); err != nil {
		return nil, err
	}

	logger.LegacyPrintf("service.merchant_apply",
		"audit: merchant application approved user_id=%d merchant_id=%d group_id=%d platform=%s",
		userID, merchant.ID, group.ID, platform)
	return merchant, nil
}
