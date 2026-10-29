package service

import (
	"context"
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
	CreateWithAccountGroups(ctx context.Context, account *Account, groups []AccountGroup) error
	Update(ctx context.Context, account *Account) error
	// WithMerchantGroupLock 在 (merchant_id, group_id) 的事务级锁内执行 fn，
	// 串行化“查重 + 创建”，避免并发接入产生重复账号。
	WithMerchantGroupLock(ctx context.Context, merchantID, groupID int64, fn func(context.Context) error) error
}

// MerchantOnboardingGroupRepository 是接入流程所需的最小分组仓储能力。
type MerchantOnboardingGroupRepository interface {
	GetByID(ctx context.Context, id int64) (*Group, error)
}

// MerchantOnboardingService 负责把商户分组接入为一个可调度的上游账号。
type MerchantOnboardingService struct {
	merchants   *MerchantService
	accountRepo MerchantOnboardingAccountRepository
	groupRepo   MerchantOnboardingGroupRepository
}

func NewMerchantOnboardingService(
	merchants *MerchantService,
	accountRepo MerchantOnboardingAccountRepository,
	groupRepo MerchantOnboardingGroupRepository,
) *MerchantOnboardingService {
	return &MerchantOnboardingService{
		merchants:   merchants,
		accountRepo: accountRepo,
		groupRepo:   groupRepo,
	}
}

// AttachGroup 为商户分组创建或更新唯一的上游账号，并绑定到该分组。
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
	if group.MerchantID != nil && *group.MerchantID != in.MerchantID {
		return nil, ErrMerchantGroupOwnership
	}

	// 保留 disabled 场景下返回的账号对象（凭证已更新），错误本身是显式信号。
	return s.attachLocked(ctx, merchant, group, in)
}

// attachLocked 在 (merchant_id, group_id) 事务级锁内完成查重与创建/更新，
// 避免并发接入产生第二个账号。查重不按账号状态过滤：error/disabled 的旧账号
// 仍必须复用，否则商户轮换 key 时会新建账号并把上游流量翻倍。
func (s *MerchantOnboardingService) attachLocked(
	ctx context.Context,
	merchant *Merchant,
	group *Group,
	in AttachGroupInput,
) (*Account, error) {
	var result *Account
	accountDisabled := false
	err := s.accountRepo.WithMerchantGroupLock(ctx, in.MerchantID, group.ID, func(lockCtx context.Context) error {
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	if accountDisabled {
		return result, ErrMerchantAccountDisabled
	}
	return result, nil
}

func merchantAccountName(merchantName string) string {
	name := "merchant:" + merchantName
	if utf8.RuneCountInString(name) <= merchantAccountNameMaxLen {
		return name
	}
	return string([]rune(name)[:merchantAccountNameMaxLen])
}
