package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/jackc/pgx/v5/pgxpool"
	libredis "github.com/redis/go-redis/v9"
)

type APIKeyData struct {
	ProjectID      *string `json:"project_id"`
	OrganizationID string  `json:"organization_id"`
	Scope          string  `json:"scope"`
	RateLimitRPM   int     `json:"rate_limit_rpm"`
	ProjectName    string  `json:"project_name"`
}

type APIKeyAuthenticator struct {
	db    *pgxpool.Pool
	redis *libredis.Client
	sub   *nats.Subscriber
}

func NewAPIKeyAuthenticator(db *pgxpool.Pool, redis *libredis.Client, sub *nats.Subscriber) *APIKeyAuthenticator {
	auth := &APIKeyAuthenticator{db: db, redis: redis, sub: sub}
	if sub != nil {
		go func() {
			_ = sub.Subscribe(context.Background(), func(msg []byte) error {
				var data map[string]string
				if err := json.Unmarshal(msg, &data); err == nil {
					if keyHash, ok := data["key_hash"]; ok {
						if auth.redis != nil {
							_ = auth.redis.Del(context.Background(), "apikey:"+keyHash)
						}
					}
				}
				return nil
			})
		}()
	}
	return auth
}

func (a *APIKeyAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, "Missing API key", http.StatusUnauthorized)
			return
		}

		hash := sha256.Sum256([]byte(apiKey))
		hashStr := hex.EncodeToString(hash[:])

		data, err := a.getAPIKeyData(r.Context(), hashStr)
		if err != nil {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		if data.Scope == "read" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		var projectKey string
		if data.ProjectID == nil || data.ProjectName == "" {
			projectKey = r.Header.Get("X-Project-Key")
			if projectKey == "" {
				http.Error(w, "Missing X-Project-Key header for org-wide key", http.StatusBadRequest)
				return
			}
		} else {
			projectKey = data.ProjectName
		}

		ctx := context.WithValue(r.Context(), "project_key", projectKey)
		ctx = context.WithValue(ctx, "rate_limit_rpm", data.RateLimitRPM)
		ctx = context.WithValue(ctx, "api_key_hash", hashStr)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *APIKeyAuthenticator) getAPIKeyData(ctx context.Context, hashStr string) (*APIKeyData, error) {
	cacheKey := "apikey:" + hashStr
	if a.redis != nil {
		if val, err := a.redis.Get(ctx, cacheKey).Result(); err == nil {
			var data APIKeyData
			if err := json.Unmarshal([]byte(val), &data); err == nil {
				return &data, nil
			}
		}
	}

	var data APIKeyData
	var status string
	var projID *string
	var projName *string

	err := a.db.QueryRow(ctx,
		`SELECT pak.project_id, pak.organization_id, pak.scope, pak.rate_limit_rpm, pak.status, p.name 
		 FROM project_api_keys pak 
		 LEFT JOIN projects p ON p.id = pak.project_id 
		 WHERE pak.key_hash = $1`,
		hashStr,
	).Scan(&projID, &data.OrganizationID, &data.Scope, &data.RateLimitRPM, &status, &projName)

	if err != nil {
		return nil, err
	}

	if status != "active" {
		return nil, errors.New("key is not active")
	}

	if projID != nil {
		pid := *projID
		data.ProjectID = &pid
	}
	if projName != nil {
		data.ProjectName = *projName
	}

	if a.redis != nil {
		if b, err := json.Marshal(data); err == nil {
			_ = a.redis.Set(ctx, cacheKey, b, 60*time.Second)
		}
	}

	return &data, nil
}
