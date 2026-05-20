package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

type APIKeyAuthenticator struct {
	db *pgxpool.Pool
}

func NewAPIKeyAuthenticator(db *pgxpool.Pool) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{db: db}
}

func (a *APIKeyAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, "Missing API key", http.StatusUnauthorized)
			return
		}

		projectKey, err := a.validateAPIKey(r.Context(), apiKey)
		if err != nil {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), "project_key", projectKey)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *APIKeyAuthenticator) validateAPIKey(ctx context.Context, apiKey string) (string, error) {
	hash := sha256.Sum256([]byte(apiKey))
	hashStr := hex.EncodeToString(hash[:])

	var projectKey string
	err := a.db.QueryRow(ctx,
		"SELECT name FROM projects WHERE api_key_hash = $1",
		hashStr,
	).Scan(&projectKey)

	if err != nil {
		return "", err
	}

	return projectKey, nil
}
