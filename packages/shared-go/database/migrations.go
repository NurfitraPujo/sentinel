package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

func LoadMigrations(migrationsDir string) (string, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return "", fmt.Errorf("failed to read migrations directory: %w", err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	var totalSQL string
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(migrationsDir, file))
		if err != nil {
			return "", fmt.Errorf("failed to read migration %s: %w", file, err)
		}
		totalSQL += string(content) + "\n"
	}

	return totalSQL, nil
}

func RunMigrationsWithPool(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	migrationSQL, err := LoadMigrations(migrationsDir)
	if err != nil {
		return err
	}
	return RunMigrations(ctx, pool, migrationSQL)
}
