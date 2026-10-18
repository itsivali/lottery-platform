package db

import "github.com/jackc/pgx/v5"

func pgxpoolConfig(dsn string) (*pgx.ConnConfig, error) { return pgx.ParseConfig(dsn) }
