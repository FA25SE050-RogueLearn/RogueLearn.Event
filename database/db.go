package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultTimeout = 10 * time.Second

func New(connStr string) (*pgx.Conn, error) {
	log.Println("connStr:", connStr)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		panic(err)
	}

	return conn, nil
}

func NewPool(connStr string) (*pgxpool.Pool, error) {
	log.Println("connStr:", connStr)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		panic(err)
	}

	// Disable prepared statements for transaction pooling (required for Supabase port 6543)
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	config.MaxConns = 10 // 2 replicas * 10 = 20 total connections
	config.MinConns = 3
	config.MaxConnLifetime = time.Hour
	config.MaxConnIdleTime = time.Minute * 30

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		panic(err)
	}

	// Test connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
