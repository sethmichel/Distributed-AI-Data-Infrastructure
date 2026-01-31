package service_c

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	config "ai_infra_project/Global_Configs"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// StartServiceC loops and calls the postgres extension for drift detection
func StartServiceC() {
	log.Println("Service C: Starting Postgres Drift Detection Service (Event-Driven)...")

	// 1. Load Config
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Service C: Failed to load config: %v", err)
	}

	// 2. Connect to Postgres
	// config.Connections.PostgresAddr is expected to be "host" or "host:port"
	connStr := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable",
		cfg.Connections.PostgresAddr,
		cfg.Connections.PostgresUser,
		cfg.Connections.PostgresPassword,
		cfg.Connections.PostgresDB,
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatalf("Service C: Failed to open DB connection: %v", err)
	}
	defer db.Close()

	// Initial Ping
	if err := db.Ping(); err != nil {
		log.Printf("Service C: Warning - Failed to ping DB: %v. Will retry...", err)
	} else {
		log.Println("Service C: Connected to Postgres successfully.")
	}

	// 3. Connect to Redis
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Connections.RedisAddr,
	})
	// Test Redis
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Printf("Service C: Warning - Failed to connect to Redis: %v", err)
	} else {
		log.Println("Service C: Connected to Redis successfully.")
	}
	defer rdb.Close()

	// 4. Subscribe to Alerts
	ctx := context.Background()
	pubsub := rdb.Subscribe(ctx, "drift_alerts")
	defer pubsub.Close()

	// Wait for confirmation that subscription is created
	_, err = pubsub.Receive(ctx)
	if err != nil {
		log.Fatalf("Service C: Failed to subscribe to Redis: %v", err)
	}

	ch := pubsub.Channel()
	log.Println("Service C: Listening for drift alerts...")

	// 5. React to Signals
	for msg := range ch {
		log.Printf("Service C: Alert received for entity %s. Triggering investigation...", msg.Payload)

		// Run the heavy extension
		performDriftAnalysis(db)
	}
}

func performDriftAnalysis(db *sql.DB) {
	// The extension function signature: perform_drift_analysis(interval, table_name, column_name)
	// We use 'features_table' and 'value' assuming that's where the data is.
	query := `SELECT perform_drift_analysis('1 hour', 'features_table', 'value');`

	var resultJSON []byte
	err := db.QueryRow(query).Scan(&resultJSON)
	if err != nil {
		log.Printf("Service C: Error running drift analysis: %v", err)
		return
	}

	// Process result: Just print it for now
	log.Printf("Service C: Drift Analysis Result: %s", string(resultJSON))
}
