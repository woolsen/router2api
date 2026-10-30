package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
	"github.com/woolsen/router2api/internal/pkg/pagination"
)

const (
	MerchantStatusActive    = "active"
	MerchantStatusSuspended = "suspended"

	// MerchantDefaultCommissionRate 是未显式指定时的平台抽成比例（8%）。
	MerchantDefaultCommissionRate = 0.08
	// MerchantNewWindow 是“新商户”标签的展示窗口，由 created_at 推导，不落布尔字段。
	MerchantNewWindow = 7 * 24 * time.Hour
)

var (
	// ErrCommissionOutOfRange 表示抽成比例不在 [0, 1) 区间内。
	ErrCommissionOutOfRange = errors.New("merchant commission rate must be in [0, 1)")

	ErrMerchantNotFound     = infraerrors.NotFound("MERCHANT_NOT_FOUND", "merchant not found")
	ErrMerchantNameRequired = infraerrors.BadRequest("MERCHANT_NAME_REQUIRED", "merchant name is required")
	ErrMerchantNameTooLong  = infraerrors.BadRequest("MERCHANT_NAME_TOO_LONG", "merchant name is too long")
	ErrMerchantContactLong  = infraerrors.BadRequest("MERCHANT_CONTACT_TOO_LONG", "merchant contact is too long")
)

// Merchant 是入驻商户（第三方中转站）。
type Merchant struct {
	ID             int64
	Name           string
	Contact        string
	Status         string
	CommissionRate float64
	CreatedAt      time.Time
}

// IsNewAt 报告商户在 now 时刻是否仍处于“新商户”窗口（created_at 起 7 天）。
// 标签完全由时间推导，7 天后自动消失。
func (m *Merchant) IsNewAt(now time.Time) bool {
	if m == nil || m.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(m.CreatedAt) < MerchantNewWindow
}

// CreateMerchantInput 是创建商户的入参；未指定抽成比例时使用默认值。
type CreateMerchantInput struct {
	Name           string
	Contact        string
	CommissionRate *float64
}

// MerchantRepository 是商户的持久化接口。
type MerchantRepository interface {
	Create(ctx context.Context, m *Merchant) error
	Get(ctx context.Context, id int64) (*Merchant, error)
	List(ctx context.Context, params pagination.PaginationParams) ([]Merchant, *pagination.PaginationResult, error)
	UpdateCommissionRate(ctx context.Context, id int64, rate float64) error
	// Delete 软删除商户，用于补偿失败的自助入驻。
	Delete(ctx context.Context, id int64) error
}

// MerchantService 负责商户的领域校验与用例编排。
type MerchantService struct {
	repo MerchantRepository
}

func NewMerchantService(repo MerchantRepository) *MerchantService {
	return &MerchantService{repo: repo}
}

func (s *MerchantService) Create(ctx context.Context, input CreateMerchantInput) (*Merchant, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, ErrMerchantNameRequired
	}
	if utf8.RuneCountInString(name) > 100 {
		return nil, ErrMerchantNameTooLong
	}

	contact := strings.TrimSpace(input.Contact)
	if utf8.RuneCountInString(contact) > 200 {
		return nil, ErrMerchantContactLong
	}

	rate := MerchantDefaultCommissionRate
	if input.CommissionRate != nil {
		rate = *input.CommissionRate
	}
	if err := validateCommissionRate(rate); err != nil {
		return nil, err
	}

	m := &Merchant{
		Name:           name,
		Contact:        contact,
		Status:         MerchantStatusActive,
		CommissionRate: rate,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *MerchantService) Get(ctx context.Context, id int64) (*Merchant, error) {
	if id <= 0 {
		return nil, ErrMerchantNotFound
	}
	return s.repo.Get(ctx, id)
}

func (s *MerchantService) List(ctx context.Context, params pagination.PaginationParams) ([]Merchant, *pagination.PaginationResult, error) {
	return s.repo.List(ctx, params)
}

func (s *MerchantService) SetCommissionRate(ctx context.Context, id int64, rate float64) error {
	if err := validateCommissionRate(rate); err != nil {
		return err
	}
	return s.repo.UpdateCommissionRate(ctx, id, rate)
}

// Delete 删除商户；仅供失败申请的回滚补偿使用。
func (s *MerchantService) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrMerchantNotFound
	}
	return s.repo.Delete(ctx, id)
}

func validateCommissionRate(rate float64) error {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate >= 1 {
		return ErrCommissionOutOfRange
	}
	return nil
}
