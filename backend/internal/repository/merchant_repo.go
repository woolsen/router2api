package repository

import (
	"context"

	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/ent/merchant"
	"github.com/woolsen/router2api/internal/pkg/pagination"
	"github.com/woolsen/router2api/internal/service"
)

type merchantRepository struct {
	client *dbent.Client
}

func NewMerchantRepository(client *dbent.Client) service.MerchantRepository {
	return &merchantRepository{client: client}
}

func (r *merchantRepository) Create(ctx context.Context, m *service.Merchant) error {
	client := clientFromContext(ctx, r.client)
	builder := client.Merchant.Create().
		SetName(m.Name).
		SetStatus(m.Status).
		SetCommissionRate(m.CommissionRate)
	if m.Contact != "" {
		builder.SetContact(m.Contact)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return err
	}
	m.ID = created.ID
	m.CreatedAt = created.CreatedAt
	return nil
}

func (r *merchantRepository) Get(ctx context.Context, id int64) (*service.Merchant, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.Merchant.Query().
		Where(merchant.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrMerchantNotFound, nil)
	}
	return merchantEntityToService(m), nil
}

func (r *merchantRepository) List(
	ctx context.Context,
	params pagination.PaginationParams,
) ([]service.Merchant, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.Merchant.Query()

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	items, err := q.
		Order(dbent.Desc(merchant.FieldID)).
		Offset(params.Offset()).
		Limit(params.Limit()).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}

	return merchantEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *merchantRepository) UpdateCommissionRate(ctx context.Context, id int64, rate float64) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.Merchant.UpdateOneID(id).
		SetCommissionRate(rate).
		Save(ctx)
	return translatePersistenceError(err, service.ErrMerchantNotFound, nil)
}

func merchantEntityToService(m *dbent.Merchant) *service.Merchant {
	if m == nil {
		return nil
	}
	contact := ""
	if m.Contact != nil {
		contact = *m.Contact
	}
	return &service.Merchant{
		ID:             m.ID,
		Name:           m.Name,
		Contact:        contact,
		Status:         m.Status,
		CommissionRate: m.CommissionRate,
		CreatedAt:      m.CreatedAt,
	}
}

func merchantEntitiesToService(models []*dbent.Merchant) []service.Merchant {
	out := make([]service.Merchant, 0, len(models))
	for i := range models {
		if s := merchantEntityToService(models[i]); s != nil {
			out = append(out, *s)
		}
	}
	return out
}
