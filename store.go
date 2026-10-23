package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	poolMaxSize    = 5
	acquireTimeout = 5 * time.Second
)

// querier is implemented by both *pgxpool.Pool and *pgxpool.Conn.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type StockLevel struct {
	Location string `json:"location"`
	Quantity int    `json:"quantity"`
}

type PoolStats struct {
	Max     int32 `json:"max"`
	Total   int32 `json:"total"`
	Active  int32 `json:"active"`
	Idle    int32 `json:"idle"`
	Waiting int64 `json:"waiting"`
}

type Store struct {
	pool    *pgxpool.Pool
	waiting atomic.Int64
}

func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = poolMaxSize

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

// Acquire takes a connection from the pool and keeps count of callers waiting for one.
func (s *Store) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	return s.pool.Acquire(ctx)
}

func (s *Store) Stats() PoolStats {
	stat := s.pool.Stat()
	return PoolStats{
		Max:     stat.MaxConns(),
		Total:   stat.TotalConns(),
		Active:  stat.AcquiredConns(),
		Idle:    stat.IdleConns(),
		Waiting: s.waiting.Load(),
	}
}

func (s *Store) StockLevels(ctx context.Context, db querier, sku string) ([]StockLevel, error) {
	rows, err := db.Query(ctx,
		"SELECT location, quantity FROM inventory WHERE sku = $1 ORDER BY location", sku)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[StockLevel])
}

func (s *Store) Quantity(ctx context.Context, db querier, sku, location string) (int, error) {
	var quantity int
	err := db.QueryRow(ctx,
		"SELECT quantity FROM inventory WHERE sku = $1 AND location = $2", sku, location).Scan(&quantity)
	return quantity, err
}

func (s *Store) SetQuantity(ctx context.Context, db querier, sku, location string, quantity int) error {
	_, err := db.Exec(ctx,
		"UPDATE inventory SET quantity = $1, updated_at = now() WHERE sku = $2 AND location = $3",
		quantity, sku, location)
	return err
}

// fully rewrited MoveStock to avoid read-modift-write pattern
func (s *Store) MoveStock(ctx context.Context, db querier, sku, from, to string, quantity int) error {
	loc1, loc2 := from, to
	qty1, qty2 := -quantity, quantity

	if loc1 > loc2 {
		loc1, loc2 = loc2, loc1
		qty1, qty2 = qty2, qty1
	}

	_, err := db.Exec(ctx,
		"UPDATE inventory SET quantity = quantity + $1, updated_at = now() WHERE sku = $2 AND location = $3",
		qty1, sku, loc1)
	if err != nil {
		return err
	}

	_, err = db.Exec(ctx,
		"UPDATE inventory SET quantity = quantity + $1, updated_at = now() WHERE sku = $2 AND location = $3",
		qty2, sku, loc2)
	return err
}
