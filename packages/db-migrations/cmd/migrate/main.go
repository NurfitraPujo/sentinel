package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/NurfitraPujo/sentinel/packages/db-migrations"
)

var (
	target  string
	command string
	dir     string
	version int64
)

func sanitizeDSN(dsn string) string {
	re := regexp.MustCompile(`(password|passphrase|secret)=[^;]*`)
	return re.ReplaceAllString(dsn, "$1=***REDACTED***")
}

func getMigrationDir(dir string) string {
	if dir != "" {
		return fmt.Sprintf("%s/packages/db-migrations/migrations", strings.TrimSuffix(dir, "/"))
	}
	
	// Fallback to finding migrations directory relative to CWD or known structure
	cwd, _ := os.Getwd()
	if strings.Contains(cwd, "packages/db-migrations") {
		return "./migrations"
	}
	return "packages/db-migrations/migrations"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: migrate <command> [options]")
		fmt.Println("Commands: up, down, status, baseline, version")
		fmt.Println("Options:")
		fmt.Println("  -target string   Target database (processor, ingestor, dashboard)")
		fmt.Println("  -dir string      Base directory (default: current working directory)")
		fmt.Println("  -version int64   Version number for baseline command")
		os.Exit(1)
	}

	command = os.Args[1]
	ctx := context.Background()

	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "-target" && i+1 < len(os.Args):
			target = os.Args[i+1]
			i++
		case arg == "-dir" && i+1 < len(os.Args):
			dir = os.Args[i+1]
			i++
		case arg == "-version" && i+1 < len(os.Args):
			fmt.Sscanf(os.Args[i+1], "%d", &version)
			i++
		case strings.HasPrefix(arg, "-target="):
			target = strings.TrimPrefix(arg, "-target=")
		case strings.HasPrefix(arg, "-dir="):
			dir = strings.TrimPrefix(arg, "-dir=")
		case strings.HasPrefix(arg, "-version="):
			fmt.Sscanf(strings.TrimPrefix(arg, "-version="), "%d", &version)
		}
	}

	if target == "" {
		fmt.Println("Error: -target is required (processor, ingestor, dashboard)")
		os.Exit(1)
	}

	envKey := fmt.Sprintf("DB_URL_%s", strings.ToUpper(target))
	dsn := os.Getenv(envKey)
	if dsn == "" {
		log.Fatalf("Error: %s environment variable is not set", envKey)
	}

	sanitizedDSN := sanitizeDSN(dsn)
	fmt.Printf("Using target: %s\n", target)
	fmt.Printf("Connection: %s\n", sanitizedDSN)

	db, err := dbmigrations.Connect(dsn)
	if err != nil {
		log.Fatalf("Error connecting to database: %v", err)
	}
	defer db.Close()

	opts := dbmigrations.DefaultOptions()
	opts.Directory = getMigrationDir(dir)

	switch command {
	case "up", "down":
		fmt.Printf("Running %s migrations...\n", command)
		if err := dbmigrations.RunMigrations(ctx, db, command, opts); err != nil {
			log.Fatalf("Migration failed (%s): %v", command, err)
		}
		fmt.Printf("Migrations (%s) executed successfully\n", command)

	case "status":
		fmt.Println("Checking migration status...")
		if err := dbmigrations.GetStatus(ctx, db, opts); err != nil {
			log.Fatalf("Status check failed: %v", err)
		}

	case "baseline":
		if version == 0 {
			log.Fatalf("Error: -version is required for baseline command")
		}
		fmt.Printf("Baselining at version %d...\n", version)
		if err := dbmigrations.BaselineVersion(ctx, db, version, opts); err != nil {
			log.Fatalf("Baseline failed: %v", err)
		}
		fmt.Println("Baseline set successfully")

	case "version":
		fmt.Printf("Migration directory: %s\n", opts.Directory)

	default:
		fmt.Printf("Unknown command: %s\n", command)
		os.Exit(1)
	}
}
