package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/woolsen/router2api/ent"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

const merchantOnboardingLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('merchant_onboarding:' || $1::text || ':' || $2::text, 0))`

func newMerchantOnboardingTestRepo(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	return newAccountRepositoryWithSQL(client, db, nil), mock
}

func TestWithMerchantGroupLockTakesAdvisoryLockInTransaction(t *testing.T) {
	repo, mock := newMerchantOnboardingTestRepo(t)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(merchantOnboardingLockSQL)).
		WithArgs(int64(1), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	called := false
	err := repo.WithMerchantGroupLock(context.Background(), 1, 7, func(ctx context.Context) error {
		called = true
		require.NotNil(t, dbent.TxFromContext(ctx), "callback must run inside the locking transaction")
		return nil
	})

	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithMerchantGroupLockRollsBackWhenCallbackFails(t *testing.T) {
	repo, mock := newMerchantOnboardingTestRepo(t)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(merchantOnboardingLockSQL)).
		WithArgs(int64(1), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	boom := errors.New("boom")
	err := repo.WithMerchantGroupLock(context.Background(), 1, 7, func(context.Context) error {
		return boom
	})

	require.ErrorIs(t, err, boom)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithMerchantGroupLockReusesOuterTransaction(t *testing.T) {
	repo, mock := newMerchantOnboardingTestRepo(t)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(merchantOnboardingLockSQL)).
		WithArgs(int64(1), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	ctx := dbent.NewTxContext(context.Background(), tx)

	called := false
	require.NoError(t, repo.WithMerchantGroupLock(ctx, 1, 7, func(ctx context.Context) error {
		called = true
		require.Same(t, tx, dbent.TxFromContext(ctx))
		return nil
	}))
	require.True(t, called)
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}
