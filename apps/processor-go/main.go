package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbCfg := database.Config{
		Host:            getEnv("POSTGRES_HOST", "localhost"),
		Port:            getEnvInt("POSTGRES_PORT", 5432),
		User:            getEnv("POSTGRES_USER", "sentinel"),
		Password:        getEnv("POSTGRES_PASSWORD", "changeme"),
		Database:        getEnv("POSTGRES_DB", "sentinel"),
		MaxConns:        25,
		MinConns:        5,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 10 * time.Minute,
	}

	db, err := database.NewConnection(ctx, dbCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	natsCfg := nats.SubscriberConfig{
		URL:       getEnv("NATS_URL", "nats://localhost:4222"),
		Stream:    "ERROR_EVENTS",
		Subject:   "error_events",
		Consumer:  "processor-consumer",
		BatchSize: 10,
		BatchWait: 1 * time.Second,
	}

	subscriber, err := nats.NewSubscriber(ctx, natsCfg)
	if err != nil {
		log.Fatalf("Failed to create NATS subscriber: %v", err)
	}
	defer subscriber.Close()

	proc := service.NewProcessorService(db)

	if err := proc.VerifyAuditLogTable(ctx); err != nil {
		log.Fatalf("AUDIT_VERIFICATION_FAILED: audit_logs table is not writable: %v", err)
	}
	log.Println("Audit log table verification passed")

	err = subscriber.Subscribe(ctx, func(data []byte) error {
		return proc.ProcessEvent(ctx, data)
	})
	if err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}

	log.Println("Processor started, waiting for events...")

	go func() {
		for err := range subscriber.Errors() {
			log.Printf("Subscriber error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down processor...")
	cancel()
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return defaultValue
}
