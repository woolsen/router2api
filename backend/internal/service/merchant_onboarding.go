package service

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
)

const (
	// merchantUpstreamGroupPriority 是商户上游账号在 account_groups 中的绑定优先级。
	merchantUpstreamGroupPriority = 50
	// merchantAccountNameMaxLen 与 accounts.name 的 schema 长度保持一致。
	merchantAccountNameMaxLen = 100
)

var (
	ErrMerchantNotActive      = infraerrors.Forbidden("MERCHANT_NOT_ACTIVE", "merchant is not active")
	ErrPlatformMismatch       = infraerrors.BadRequest("PLATFORM_MISMATCH", "merchant group platform is unsupported or does not match the upstream platform")
	ErrMerchantGroupOwnership = infraerrors.Forbidden("MERCHANT_GROUP_OWNERSHIP", "group does not belong to this merchant")
	// ErrMerchantGroupOccupied 表示分组上已有其他商户或平台自营账号；
	// 必须由管理员先摘除旧账号，不能静默共用（否则用 B 的 key 打 B 的上游却按 A 结算）。
	ErrMerchantGroupOccupied = infraerrors.Conflict("MERCHANT_GROUP_OCCUPIED", "group already has another merchant or platform-owned account; detach it before attaching")
	// ErrMerchantGroupModelsRequired 表示未提供模型；货架只展示有模型的分组。
	ErrMerchantGroupModelsRequired = infraerrors.BadRequest("MERCHANT_GROUP_MODELS_REQUIRED", "models must not be empty: the market shelf only lists groups with models")
	// ErrMerchantAccountDisabled 表示凭证已轮换，但账号仍被管理员禁用。
	// 调用方必须显式提示，否则接口成功却永远不会被调度。
	ErrMerchantAccountDisabled = infraerrors.Conflict("MERCHANT_ACCOUNT_DISABLED", "merchant upstream account is disabled; re-enable it before attaching")
)

// merchantOnboardingPlatforms 是允许接入的商户上游平台。
var merchantOnboardingPlatforms = map[string]struct{}{
	PlatformOpenAI:    {},
	PlatformAnthropic: {},
}

// AttachGroupInput 是商户分组接入上游账号的入参。
type AttachGroupInput struct {
	MerchantID int64
	GroupID    int64
	BaseURL    string
	APIKey     string
	Platform   string
}

// MerchantOnboardingAccountRepository 是接入流程所需的最小账号仓储能力。
type MerchantOnboardingAccountRepository interface {
	// GetMerchantGroupAccount 按 (merchant_id, group_id) 查已有账号，不按状态过滤。
	GetMerchantGroupAccount(ctx context.Context, merchantID, groupID int64) (*Account, error)
	// ListGroupAccounts 返回分组上所有未删除账号（不按状态过滤），用于拒绝跨商户共用。
	ListGroupAccounts(ctx context.Context, groupID int64) ([]Account, error)
	CreateWithAccountGroups(ctx context.Context, account *Account, groups []AccountGroup) error
	Update(ctx context.Context, account *Account) error
	// Delete 移除失败申请中已创建的账号；用于回滚补偿。
	Delete(ctx context.Context, accountID int64) error
	// WithGroupOnboardingLock 以 groupID 为键持有事务级锁，串行化“认领归属 + 查重 + 创建”，
	// 避免两个商户并发接入同一无主分组。锁键只含 groupID：按商户分键会给每个商户各一把锁，
	// 无法互斥。
	WithGroupOnboardingLock(ctx context.Context, groupID int64, fn func(context.Context) error) error
}

// MerchantOnboardingGroupRepository 是接入流程所需的最小分组仓储能力。
type MerchantOnboardingGroupRepository interface {
	GetByID(ctx context.Context, id int64) (*Group, error)
	// ClaimMerchantOwnership 条件认领分组：仅当 merchant_id IS NULL 时写入。
	// 返回 claimed=true 表示本次认领成功；claimed=false 时 ownerID 是当前归属
	// （nil 表示分组不存在/已删除或仍为空，调用方按未归属处理）。
	ClaimMerchantOwnership(ctx context.Context, merchantID, groupID int64) (claimed bool, ownerID *int64, err error)
}

// MerchantOnboardingService 负责把商户分组接入为一个可调度的上游账号。
type MerchantOnboardingService struct {
	merchants   *MerchantService
	accountRepo MerchantOnboardingAccountRepository
	groupRepo   MerchantOnboardingGroupRepository
	channels    *ChannelService
	authCache   APIKeyAuthCacheInvalidator
}

func NewMerchantOnboardingService(
	merchants *MerchantService,
	accountRepo MerchantOnboardingAccountRepository,
	groupRepo MerchantOnboardingGroupRepository,
	channels *ChannelService,
	authCache APIKeyAuthCacheInvalidator,
) *MerchantOnboardingService {
	return &MerchantOnboardingService{
		merchants:   merchants,
		accountRepo: accountRepo,
		groupRepo:   groupRepo,
		channels:    channels,
		authCache:   authCache,
	}
}

// AttachGroup 为商户分组创建或更新唯一的上游账号，并绑定到该分组。
//
// 归属语义：首次接入必须认领无主分组（groups.merchant_id IS NULL）；认领在
// 与账号创建相同的事务里完成，失败时一并回滚。已属于本商户的分组可重复接入
// （幂等轮换凭证），属于其他商户则返回 ErrMerchantGroupOwnership。
//
// 幂等语义：同一 (merchant_id, group_id) 再次调用时原地轮换
// Credentials["base_url"] 与 Credentials["api_key"]，不会新建账号。
// error 账号在轮换凭证时一并恢复为 active 且可调度；disabled 账号保持禁用，
// 但返回非 nil 的 ErrMerchantAccountDisabled（账号对象仍返回，凭证已更新），
// 避免调用方看到成功却永远调度不到流量。
func (s *MerchantOnboardingService) AttachGroup(ctx context.Context, in AttachGroupInput) (*Account, error) {
	merchant, err := s.merchants.Get(ctx, in.MerchantID)
	if err != nil {
		return nil, err
	}
	if merchant.Status != MerchantStatusActive {
		return nil, ErrMerchantNotActive
	}

	if _, ok := merchantOnboardingPlatforms[in.Platform]; !ok {
		return nil, ErrPlatformMismatch
	}

	group, err := s.groupRepo.GetByID(ctx, in.GroupID)
	if err != nil {
		return nil, err
	}
	if group.Platform != in.Platform {
		return nil, ErrPlatformMismatch
	}
	// 快速失败：已归属其他商户时不再进入事务。真正的归属判定在锁内用条件更新完成，
	// 这里只是避免无谓的锁等待。
	if group.MerchantID != nil && *group.MerchantID != in.MerchantID {
		return nil, ErrMerchantGroupOwnership
	}

	// 保留 disabled 场景下返回的账号对象（凭证已更新），错误本身是显式信号。
	return s.attachLocked(ctx, merchant, group, in)
}

// attachLocked 在 groupID 事务级锁内完成归属认领、查重与创建/更新。
//
// 锁键必须是 groupID：两个商户并发争抢同一个无主分组时，只有以分组为键的锁
// 才能互斥；按 (merchant_id, group_id) 分键会给每个商户各一把锁，两边都能通过
// 归属检查并各自建账号。
func (s *MerchantOnboardingService) attachLocked(
	ctx context.Context,
	merchant *Merchant,
	group *Group,
	in AttachGroupInput,
) (*Account, error) {
	var result *Account
	accountDisabled := false
	claimed := false
	err := s.accountRepo.WithGroupOnboardingLock(ctx, group.ID, func(lockCtx context.Context) error {
		// 分组上不允许同时存在其他商户账号或平台自营账号。先检查再认领：
		// 冲突时分组归属不会被半途改掉，也避免依赖数据库事务回滚。
		if err := s.ensureGroupExclusive(lockCtx, in.MerchantID, group.ID); err != nil {
			return err
		}

		claimedNow, ownerID, err := s.groupRepo.ClaimMerchantOwnership(lockCtx, in.MerchantID, group.ID)
		if err != nil {
			return fmt.Errorf("claim merchant group ownership: %w", err)
		}
		if !claimedNow {
			if ownerID == nil || *ownerID != in.MerchantID {
				return ErrMerchantGroupOwnership
			}
			// 已属于本商户：幂等重试，继续轮换凭证。
		}

		account, err := s.accountRepo.GetMerchantGroupAccount(lockCtx, in.MerchantID, group.ID)
		if err != nil {
			return fmt.Errorf("get merchant upstream account: %w", err)
		}
		if account != nil {
			if account.Credentials == nil {
				account.Credentials = make(map[string]any, 2)
			}
			account.Credentials["base_url"] = in.BaseURL
			account.Credentials["api_key"] = in.APIKey
			// error 表示上游凭证失效，轮换 key 本身就是一次修复：清除错误并恢复调度。
			// disabled 是管理员意图，保持禁用，但返回显式信号而不是静默成功。
			switch account.Status {
			case StatusError:
				account.Status = StatusActive
				account.ErrorMessage = ""
				account.Schedulable = true
			case StatusDisabled:
				accountDisabled = true
			}
			if err := s.accountRepo.Update(lockCtx, account); err != nil {
				return fmt.Errorf("update merchant upstream account: %w", err)
			}
			result = account
			claimed = claimedNow
			return nil
		}

		merchantID := in.MerchantID
		account = &Account{
			Name:        merchantAccountName(merchant.Name),
			Platform:    in.Platform,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": in.BaseURL, "api_key": in.APIKey},
			Status:      StatusActive,
			Schedulable: true,
			MerchantID:  &merchantID,
		}
		// 账号倍率保持默认 1.0：用户侧扣费只认 group.rate_multiplier，
		// 商户抽成在结算时按 usage_logs 快照计算。
		if err := s.accountRepo.CreateWithAccountGroups(lockCtx, account, []AccountGroup{
			{GroupID: group.ID, Priority: merchantUpstreamGroupPriority},
		}); err != nil {
			return fmt.Errorf("create merchant upstream account: %w", err)
		}
		result = account
		claimed = claimedNow
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 归属变化会改变认证快照里的 merchant_id；必须在事务提交后失效缓存，
	// 否则已在 L1/L2 里缓存的无主分组仍会按平台自营扣费。
	if claimed && s.authCache != nil {
		s.authCache.InvalidateAuthCacheByGroupID(ctx, group.ID)
	}
	if accountDisabled {
		return result, ErrMerchantAccountDisabled
	}
	return result, nil
}

// ensureGroupExclusive 拒绝分组上已存在的其他商户账号或平台自营账号。
// 不按状态过滤：disabled/error 的旧账号仍绑定着该分组，共用会混淆结算归属。
func (s *MerchantOnboardingService) ensureGroupExclusive(ctx context.Context, merchantID, groupID int64) error {
	accounts, err := s.accountRepo.ListGroupAccounts(ctx, groupID)
	if err != nil {
		return fmt.Errorf("list group accounts: %w", err)
	}
	for i := range accounts {
		accountMerchantID := accounts[i].MerchantID
		if accountMerchantID == nil || *accountMerchantID != merchantID {
			return ErrMerchantGroupOccupied
		}
	}
	return nil
}

// ConfigureGroupModels 为分组建立/复用渠道并写入模型，使 /model-plaza 与货架
// 能枚举到该分组。models 为空时返回 ErrMerchantGroupModelsRequired：
// 无模型分组会被 ModelPlazaService.ListGroups 丢弃，接入成功但货架永远看不到。
func (s *MerchantOnboardingService) ConfigureGroupModels(
	ctx context.Context,
	groupID int64,
	platform string,
	models []string,
) error {
	if len(models) == 0 {
		return ErrMerchantGroupModelsRequired
	}
	if s.channels == nil {
		return fmt.Errorf("merchant onboarding channel service is not configured")
	}
	return s.channels.EnsureGroupModels(ctx, groupID, platform, models)
}

// RollbackGroup 删除失败申请为该分组创建的渠道与上游账号，供调用方继续删除
// 分组和商户。每一步独立执行，尽量清理干净；返回的错误只用于日志。
func (s *MerchantOnboardingService) RollbackGroup(ctx context.Context, groupID, accountID int64) error {
	var errs []error
	if s.channels != nil && s.channels.repo != nil && groupID > 0 {
		channelID, err := s.channels.repo.GetChannelIDByGroupID(ctx, groupID)
		if err != nil {
			errs = append(errs, fmt.Errorf("find channel for group %d: %w", groupID, err))
		} else if channelID > 0 {
			if err := s.channels.Delete(ctx, channelID); err != nil && !errors.Is(err, ErrChannelNotFound) {
				errs = append(errs, fmt.Errorf("delete channel %d: %w", channelID, err))
			}
		}
	}
	if s.accountRepo != nil && accountID > 0 {
		if err := s.accountRepo.Delete(ctx, accountID); err != nil {
			errs = append(errs, fmt.Errorf("delete account %d: %w", accountID, err))
		}
	}
	return errors.Join(errs...)
}

func merchantAccountName(merchantName string) string {
	name := "merchant:" + merchantName
	if utf8.RuneCountInString(name) <= merchantAccountNameMaxLen {
		return name
	}
	return string([]rune(name)[:merchantAccountNameMaxLen])
}
