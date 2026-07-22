package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const currentVersion = 2

type migration struct {
	version int
	up      func(context.Context, *sql.Tx) error
}

type migrationConnection interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var defaultMigrations = []migration{
	{version: 1},
	{version: currentVersion, up: createNamespaceSchema},
}

func readSchemaVersion(ctx context.Context, database migrationConnection) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read database schema version: %w", err)
	}
	return version, nil
}

func validateSchemaVersion(version int, migrations []migration) error {
	if err := validateMigrations(migrations); err != nil {
		return err
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf(
			"%w: database has version %d, supported version is %d",
			ErrUnsupportedSchemaVersion,
			version,
			len(migrations),
		)
	}
	return nil
}

func applyMigrations(
	ctx context.Context,
	database migrationConnection,
	migrations []migration,
	version int,
) error {
	for _, migration := range migrations[version:] {
		if err := applyMigration(ctx, database, migration); err != nil {
			return fmt.Errorf("apply database migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func validateMigrations(migrations []migration) error {
	for index, migration := range migrations {
		want := index + 1
		if migration.version != want {
			return fmt.Errorf("database migration version %d is out of sequence; want %d", migration.version, want)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, database migrationConnection, migration migration) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	rollback := func(cause error) error {
		return errorsJoin(cause, transaction.Rollback())
	}

	if migration.up != nil {
		if err := migration.up(ctx, transaction); err != nil {
			return rollback(err)
		}
	}
	if _, err := transaction.ExecContext(
		ctx,
		fmt.Sprintf("PRAGMA user_version = %d", migration.version),
	); err != nil {
		return rollback(fmt.Errorf("set schema version: %w", err))
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func errorsJoin(primary, cleanup error) error {
	if cleanup == nil || errors.Is(cleanup, sql.ErrTxDone) {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("rollback transaction: %w", cleanup))
}
