package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	MaxConns        int
	MinConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	TLSCert         string
	TLSKey          string
	TLSCA           string
	TLSMode         string
}

func NewConnection(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	sslMode := "disable"
	if cfg.TLSMode != "" {
		sslMode = cfg.TLSMode
	}

	connString := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database, sslMode,
	)

	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		connString += fmt.Sprintf("&sslcert=%s&sslkey=%s", cfg.TLSCert, cfg.TLSKey)
	}
	if cfg.TLSCA != "" {
		connString += fmt.Sprintf("&sslrootcert=%s", cfg.TLSCA)
	}

	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	poolConfig.MaxConns = int32(cfg.MaxConns)
	poolConfig.MinConns = int32(cfg.MinConns)
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}

func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationSQL string) error {
	_, err := pool.Exec(ctx, migrationSQL)
	if err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}
