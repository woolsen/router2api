package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
	"github.com/woolsen/router2api/internal/pkg/logger"
)

const (
	merchantApplyDefaultRateMultiplier = 1.0
	// merchantApplyModelsLimit 限制一次申请可上架的模型数量，避免超大请求拖垮渠道写入。
	merchantApplyModelsLimit = 200
	// merchantApplyCleanupTimeout 是失败补偿的独立预算；请求 ctx 可能已取消。
	merchantApplyCleanupTimeout = 10 * time.Second
)

var (
	// ErrMerchantAlreadyBound 表示该用户已经绑定商户，不能重复申请。
	ErrMerchantAlreadyBound = infraerrors.Conflict("MERCHANT_ALREADY_BOUND", "user is already bound to a merchant")
	// ErrMerchantApplyForbidden 表示当前角色（如管理员）不允许走商户自助申请。
	ErrMerchantApplyForbidden = infraerrors.Forbidden("MERCHANT_APPLY_FORBIDDEN", "only regular users can apply for a merchant account")
	// ErrMerchantRateInvalid 表示申请中的分组倍率非法。
	ErrMerchantRateInvalid = infraerrors.BadRequest("MERCHANT_RATE_INVALID", "rate_multiplier must be a positive number")
	// ErrMerchantModelsMismatch 表示申请者勾选的模型与探测结果没有交集。
	ErrMerchantModelsMismatch = infraerrors.BadRequest("MERCHANT_MODELS_MISMATCH", "selected models must be a non-empty subset of the models returned by the upstream probe")
	// ErrMerchantModelsTooMany 表示一次申请勾选的上架模型超过上限。
	ErrMerchantModelsTooMany = infraerrors.BadRequest("MERCHANT_MODELS_TOO_MANY", "at most 200 models can be selected")
)

// ApplyInput 是商户自助入驻申请。Models 与 RateMultiplier 来自申请者在前端
// 探测结果上的确认；Models 会与探测结果取交集，为空时回落到探测返回的模型列表。
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
	// WithMerchantApplyLock 按 userID 串行化落库阶段，避免并发申请各自通过前置检查。
	WithMerchantApplyLock(ctx context.Context, userID int64, fn func(context.Context) error) error
	// PromoteToMerchant 条件更新：仅当用户仍是未绑定商户的普通用户时生效。
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

// merchantApplyArtifacts 记录本次申请已写入的资源 ID，失败时按此补偿。
type merchantApplyArtifacts struct {
	merchantID int64
	groupID    int64
	accountID  int64
}

// Submit 执行自助入驻：探测 → 建商户 → 建分组 → 接入上游账号 → 写货架模型
// → 把用户升级为 merchant 并绑定 merchant_id。
//
// 顺序不能调换：AttachGroup 要求分组已存在，且只有 ConfigureGroupModels 写入
// 渠道模型后，ModelPlazaService.ListGroups（货架与 /model-plaza 的数据源）
// 才能枚举到这个分组。
//
// 落库阶段按 userID 持有 advisory lock：并发申请会串行复查用户状态，只有第一个
// 能继续；最后一步 PromoteToMerchant 仍是条件更新，兜住锁外被管理员绑定的情况。
// 任一步失败都会补偿本次新建的资源，保证同名重试可以继续。
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
	models, err := resolveMerchantApplyModels(in.Models, probeResult.Models)
	if err != nil {
		return nil, err
	}

	var merchant *Merchant
	var groupID int64
	err = s.users.WithMerchantApplyLock(ctx, userID, func(lockCtx context.Context) error {
		// 锁内复查：等待期间用户可能已完成申请或被管理员绑定。
		current, getErr := s.users.GetByID(lockCtx, userID)
		if getErr != nil {
			return getErr
		}
		if current.Role == RoleMerchant || current.MerchantID != nil {
			return ErrMerchantAlreadyBound
		}
		if current.Role != RoleUser {
			return ErrMerchantApplyForbidden
		}

		created := merchantApplyArtifacts{}
		merchant, getErr = s.createArtifacts(lockCtx, userID, name, platform, in.BaseURL, in.APIKey, rate, models, &created)
		if getErr != nil {
			s.compensate(lockCtx, created)
			return getErr
		}
		groupID = created.groupID
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.LegacyPrintf("service.merchant_apply",
		"audit: merchant application approved user_id=%d merchant_id=%d group_id=%d platform=%s",
		userID, merchant.ID, groupID, platform)
	return merchant, nil
}

func (s *MerchantApplyService) createArtifacts(
	ctx context.Context,
	userID int64,
	name, platform, baseURL, apiKey string,
	rate float64,
	models []string,
	created *merchantApplyArtifacts,
) (*Merchant, error) {
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
	created.merchantID = merchant.ID

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
	created.groupID = group.ID

	account, err := s.onboarding.AttachGroup(ctx, AttachGroupInput{
		MerchantID: merchant.ID,
		GroupID:    group.ID,
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Platform:   platform,
	})
	// AttachGroup 在“账号已禁用”等场景会同时返回账号和错误，账号 ID 仍需记录以便回滚。
	if account != nil {
		created.accountID = account.ID
	}
	if err != nil {
		return nil, err
	}

	if err := s.onboarding.ConfigureGroupModels(ctx, group.ID, platform, models); err != nil {
		return nil, err
	}

	if err := s.users.PromoteToMerchant(ctx, userID, merchant.ID); err != nil {
		return nil, err
	}
	return merchant, nil
}

func resolveMerchantApplyModels(requested, probed []string) ([]string, error) {
	probed = normalizeChannelModelNames(probed)
	requested = normalizeChannelModelNames(requested)
	if len(requested) == 0 {
		if len(probed) > merchantApplyModelsLimit {
			probed = probed[:merchantApplyModelsLimit]
		}
		return probed, nil
	}
	if len(requested) > merchantApplyModelsLimit {
		return nil, ErrMerchantModelsTooMany
	}

	allowed := make(map[string]struct{}, len(probed))
	for _, model := range probed {
		allowed[strings.ToLower(model)] = struct{}{}
	}
	selected := make([]string, 0, len(requested))
	for _, model := range requested {
		if _, ok := allowed[strings.ToLower(model)]; ok {
			selected = append(selected, model)
		}
	}
	if len(selected) == 0 {
		return nil, ErrMerchantModelsMismatch
	}
	return selected, nil
}

func (s *MerchantApplyService) compensate(ctx context.Context, created merchantApplyArtifacts) {
	if created.merchantID == 0 && created.groupID == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), merchantApplyCleanupTimeout)
	defer cancel()

	if created.groupID > 0 {
		if err := s.onboarding.RollbackGroup(cleanupCtx, created.groupID, created.accountID); err != nil {
			logger.LegacyPrintf("service.merchant_apply",
				"rollback: merchant application cleanup failed group_id=%d account_id=%d err=%v",
				created.groupID, created.accountID, err)
		}
		if err := s.groups.Delete(cleanupCtx, created.groupID); err != nil && !errors.Is(err, ErrGroupNotFound) {
			logger.LegacyPrintf("service.merchant_apply",
				"rollback: delete group failed group_id=%d err=%v", created.groupID, err)
		}
	}
	if created.merchantID > 0 {
		if err := s.merchants.Delete(cleanupCtx, created.merchantID); err != nil && !errors.Is(err, ErrMerchantNotFound) {
			logger.LegacyPrintf("service.merchant_apply",
				"rollback: delete merchant failed merchant_id=%d err=%v", created.merchantID, err)
		}
	}
	logger.LegacyPrintf("service.merchant_apply",
		"audit: merchant application rolled back merchant_id=%d group_id=%d account_id=%d",
		created.merchantID, created.groupID, created.accountID)
}
