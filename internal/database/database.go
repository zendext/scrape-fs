package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const DefaultLockPath = "/run/scrapefs/scrapefsd.lock"

var (
	ErrAlreadyRunning           = errors.New("scrapefsd is already running")
	ErrConnectionLeaseExpired   = errors.New("database connection lease expired")
	ErrUnsupportedSchemaVersion = errors.New("unsupported database schema version")
)

type Options struct {
	LockPath string
}

type DB struct {
	pool           *sql.DB
	conn           *sql.Conn
	directory      *os.File
	lock           *os.File
	connectionGate chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

type Connection struct {
	lease *connectionLease
}

type Transaction struct {
	lease       *connectionLease
	transaction *sql.Tx
}

type Row struct {
	lease *connectionLease
	scan  func(...any) error
}

type connectionLease struct {
	connection     *sql.Conn
	mu             sync.RWMutex
	active         bool
	transactionsMu sync.Mutex
	transactions   map[*Transaction]struct{}
}

func newConnectionLease(connection *sql.Conn) *connectionLease {
	return &connectionLease{
		connection:   connection,
		active:       true,
		transactions: make(map[*Transaction]struct{}),
	}
}

func (connection *Connection) QueryRowContext(
	ctx context.Context,
	query string,
	arguments ...any,
) *Row {
	arguments = append([]any(nil), arguments...)
	return &Row{
		lease: connection.lease,
		scan: func(destinations ...any) error {
			return connection.lease.connection.QueryRowContext(
				ctx,
				query,
				arguments...,
			).Scan(destinations...)
		},
	}
}

func (connection *Connection) BeginTx(
	ctx context.Context,
	options *sql.TxOptions,
) (*Transaction, error) {
	var transaction *Transaction
	err := connection.lease.use(func() error {
		opened, err := connection.lease.connection.BeginTx(ctx, options)
		if err != nil {
			return err
		}
		transaction = &Transaction{
			lease:       connection.lease,
			transaction: opened,
		}
		connection.lease.transactionsMu.Lock()
		connection.lease.transactions[transaction] = struct{}{}
		connection.lease.transactionsMu.Unlock()
		return nil
	})
	return transaction, err
}

func (transaction *Transaction) ExecContext(
	ctx context.Context,
	query string,
	arguments ...any,
) (sql.Result, error) {
	var result sql.Result
	err := transaction.lease.use(func() error {
		var err error
		result, err = transaction.transaction.ExecContext(ctx, query, arguments...)
		return err
	})
	return result, err
}

func (transaction *Transaction) QueryRowContext(
	ctx context.Context,
	query string,
	arguments ...any,
) *Row {
	arguments = append([]any(nil), arguments...)
	return &Row{
		lease: transaction.lease,
		scan: func(destinations ...any) error {
			return transaction.transaction.QueryRowContext(
				ctx,
				query,
				arguments...,
			).Scan(destinations...)
		},
	}
}

func (transaction *Transaction) Commit() error {
	return transaction.finish(transaction.transaction.Commit)
}

func (transaction *Transaction) Rollback() error {
	return transaction.finish(transaction.transaction.Rollback)
}

func (transaction *Transaction) finish(action func() error) error {
	return transaction.lease.use(func() error {
		defer transaction.lease.removeTransaction(transaction)
		return action()
	})
}

func (row *Row) Scan(destinations ...any) error {
	return row.lease.use(func() error {
		return row.scan(destinations...)
	})
}

func (lease *connectionLease) use(action func() error) error {
	lease.mu.RLock()
	defer lease.mu.RUnlock()
	if !lease.active {
		return ErrConnectionLeaseExpired
	}
	return action()
}

func (lease *connectionLease) removeTransaction(transaction *Transaction) {
	lease.transactionsMu.Lock()
	delete(lease.transactions, transaction)
	lease.transactionsMu.Unlock()
}

func (lease *connectionLease) close() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active {
		return nil
	}
	lease.active = false

	lease.transactionsMu.Lock()
	defer lease.transactionsMu.Unlock()
	var closeErr error
	for transaction := range lease.transactions {
		if err := transaction.transaction.Rollback(); err != nil &&
			!errors.Is(err, sql.ErrTxDone) {
			closeErr = errors.Join(
				closeErr,
				fmt.Errorf("rollback connection lease transaction: %w", err),
			)
		}
		delete(lease.transactions, transaction)
	}
	return closeErr
}

func Open(ctx context.Context, databasePath string) (*DB, error) {
	return OpenWithOptions(ctx, databasePath, Options{})
}

func OpenWithOptions(ctx context.Context, databasePath string, options Options) (*DB, error) {
	return open(ctx, databasePath, options, defaultMigrations)
}

func open(ctx context.Context, databasePath string, options Options, migrations []migration) (_ *DB, err error) {
	return openWithPrepareHook(ctx, databasePath, options, migrations, nil)
}

func openWithPrepareHook(
	ctx context.Context,
	databasePath string,
	options Options,
	migrations []migration,
	afterPrepare func() error,
) (_ *DB, err error) {
	if options.LockPath == "" {
		options.LockPath = DefaultLockPath
	}

	lock, err := acquireLock(options.LockPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, releaseLock(lock))
		}
	}()

	prepared, err := prepareDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()
	if afterPrepare != nil {
		if err := afterPrepare(); err != nil {
			return nil, fmt.Errorf("after database preparation: %w", err)
		}
	}

	pool, err := sql.Open("sqlite", sqliteDSN(prepared.sqlitePath()))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", databasePath, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, pool.Close())
		}
	}()

	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect SQLite database %q: %w", databasePath, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, conn.Close())
		}
	}()

	version, err := readSchemaVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := validateSchemaVersion(version, migrations); err != nil {
		return nil, err
	}
	if err := configurePragmas(ctx, conn); err != nil {
		return nil, err
	}
	if err := verifyPragmas(ctx, conn); err != nil {
		return nil, err
	}
	if err := applyMigrations(ctx, conn, migrations, version); err != nil {
		return nil, err
	}

	connectionGate := make(chan struct{}, 1)
	connectionGate <- struct{}{}
	return &DB{
		pool:           pool,
		conn:           conn,
		directory:      prepared.directory,
		lock:           lock,
		connectionGate: connectionGate,
	}, nil
}

func (database *DB) UseConnection(
	ctx context.Context,
	use func(*Connection) error,
) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-database.connectionGate:
	}
	if err := ctx.Err(); err != nil {
		database.connectionGate <- struct{}{}
		return err
	}
	defer func() {
		database.connectionGate <- struct{}{}
	}()
	lease := newConnectionLease(database.conn)
	defer func() {
		err = errors.Join(err, lease.close())
	}()
	return use(&Connection{lease: lease})
}

func (database *DB) Close() error {
	database.closeOnce.Do(func() {
		<-database.connectionGate
		defer func() {
			database.connectionGate <- struct{}{}
		}()
		database.closeErr = errors.Join(
			database.conn.Close(),
			database.pool.Close(),
			database.directory.Close(),
			releaseLock(database.lock),
		)
	})
	return database.closeErr
}

func sqliteDSN(databasePath string) string {
	dsn := &url.URL{Scheme: "file", Path: databasePath}
	query := dsn.Query()
	query.Set("mode", "rw")
	query.Set("_txlock", "immediate")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

type pragmaConnection interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func configurePragmas(ctx context.Context, conn pragmaConnection) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", statement, err)
		}
	}

	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("apply PRAGMA journal_mode = WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("incompatible PRAGMA journal_mode: got %q, want WAL", journalMode)
	}
	return nil
}

func verifyPragmas(ctx context.Context, database pragmaConnection) error {
	checks := []struct {
		name string
		want int
	}{
		{name: "foreign_keys", want: 1},
		{name: "synchronous", want: 2},
		{name: "busy_timeout", want: 5000},
	}
	for _, check := range checks {
		var got int
		if err := database.QueryRowContext(ctx, "PRAGMA "+check.name).Scan(&got); err != nil {
			return fmt.Errorf("verify PRAGMA %s: %w", check.name, err)
		}
		if got != check.want {
			return fmt.Errorf("incompatible PRAGMA %s: got %d, want %d", check.name, got, check.want)
		}
	}

	var journalMode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("verify PRAGMA journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("incompatible PRAGMA journal_mode: got %q, want WAL", journalMode)
	}
	return nil
}
