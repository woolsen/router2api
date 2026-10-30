package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	dbent "github.com/woolsen/router2api/ent"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func newMerchantApplyLockTestRepo(t *testing.T) (*userRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	return newUserRepositoryWithSQL(client, db), mock
}

func TestWithMerchantApplyLockHoldsSessionAdvisoryLock(t *testing.T) {
	repo, mock := newMerchantApplyLockTestRepo(t)
	lockKey := advisoryLockHash("merchant_apply:user:42")

	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(lockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(lockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	called := false
	err := repo.WithMerchantApplyLock(context.Background(), 42, func(context.Context) error {
		called = true
		return nil
	})

	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithMerchantApplyLockReleasesOnFailure(t *testing.T) {
	repo, mock := newMerchantApplyLockTestRepo(t)
	lockKey := advisoryLockHash("merchant_apply:user:42")

	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(lockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(lockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	boom := errors.New("boom")
	err := repo.WithMerchantApplyLock(context.Background(), 42, func(context.Context) error {
		return boom
	})

	require.ErrorIs(t, err, boom)
	require.NoError(t, mock.ExpectationsWereMet())
}
