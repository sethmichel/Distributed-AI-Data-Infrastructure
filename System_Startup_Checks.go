package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	config "ai_infra_project/Global_Configs"
	service_b "ai_infra_project/Services/Service_B"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

/*
1. duckdb directory and features table and metadata table
2. duckdb has model metadata in metadata table
3. save the required models names into redis so we know what's downloaded
4. check/start kafka server is running
5. start docker services (redis, promethius, granfana)
6. load prod status models into redis on startup
*/

func CheckDuckDB(app_config_struct *config.App_Config) error {
	path := app_config_struct.Connections.DuckDBPath

	// check directory exists
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory for duckdb: %w", err)
		}
	}

	// connect to ensure tables exists
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return fmt.Errorf("failed to open duckdb: %w", err)
	}
	defer db.Close()

	// Create features table if it doesn't exist
	query := `
		CREATE TABLE IF NOT EXISTS features (
			entity_id TEXT,
			feature_name TEXT,
			value DOUBLE,
			event_timestamp TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`

	if _, err := db.Exec(query); err != nil {
		return fmt.Errorf("failed to create features table: %w", err)
	}
	log.Printf("DuckDB initialized successfully (table checked) at %s.", path)

	// create model metadata table if it doesn't exist
	metadataQuery := `
		CREATE TABLE IF NOT EXISTS model_metadata (
			model_id TEXT,
			artifact_type TEXT,
			version TEXT,
			trainedDate DATE,
			status TEXT,
			azure_location TEXT,
			expected_features STRUCT("index" INTEGER, "name" TEXT, "type" TEXT)[],
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`

	if _, err := db.Exec(metadataQuery); err != nil {
		return fmt.Errorf("failed to create model_metadata table: %w", err)
	}

	// check metadata for required models is present (it returns true/false for each model)
	// these are placeholder models, we can use anything but I just use these basic ones
	requiredModels := []string{"Loan_Approve", "House_Price", "Coffee_Reccomender"}
	requiredModelQuery := `
		SELECT EXISTS (
			SELECT 1 
			FROM model_metadata 
			WHERE model_id = ?
		);`

	for _, model := range requiredModels {
		var exists bool
		err := db.QueryRow(requiredModelQuery, model).Scan(&exists)
		if err != nil {
			return fmt.Errorf("failed to check metadata for model %s: %w", model, err)
		}
		if !exists {
			return fmt.Errorf("required model metadata missing for: %s", model)
		}
		log.Printf("Validated model metadata for: %s", model)
	}

	// now add those model names to redis
	AddModelNamesRedis(requiredModels, app_config_struct)

	return nil
}

// Write required models to Redis
// we're using a redis set (sadd) for efficient lookups (o1)
// we can check for a model with: SISMEMBER prod_models "Loan_Approve"
func AddModelNamesRedis(requiredModels []string, app_config_struct *config.App_Config) error {
	redisAddr := app_config_struct.Connections.RedisAddr
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // prevent hanging
	defer cancel()

	// Clear existing key to ensure we don't have stale models
	if err := rdb.Del(ctx, "prod_models").Err(); err != nil {
		log.Printf(`Warning: failed to clear prod_models from redis. this was either an error and there were no models to clear.
		            this is refering to just the names of our downloaded models for other services to reference: %v`, err)
	}

	// Use SADD to add all models as a set (a O1 redis thing)
	// We need to convert []string to []interface{} for SAdd
	if len(requiredModels) > 0 {
		args := make([]interface{}, len(requiredModels))
		for i, v := range requiredModels {
			args[i] = v
		}
		if err := rdb.SAdd(ctx, "prod_models", args...).Err(); err != nil {
			return fmt.Errorf("failed to write required models to redis: %w", err)
		}
		log.Printf("Updated 'prod_models' in Redis with %d models.", len(requiredModels))
	}

	return nil
}

func LoadModelsOnStartup(app_config_struct *config.App_Config) error {
	log.Println("Calling service B to Load production models from Azure...")
	redisAddr := app_config_struct.Connections.RedisAddr

	handler, err := service_b.CreateNewHandler(redisAddr)
	if err != nil {
		return fmt.Errorf("failed to create service_b handler: %w", err)
	}
	defer handler.CloseRedisClient()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := handler.LoadProductionModels(ctx); err != nil {
		return fmt.Errorf("failed to load production models: %w", err)
	}

	log.Println("Successfully loaded production models.")
	return nil
}

// checks that kafka broker is running
func CheckKafka(app_config_struct *config.App_Config) error {
	brokerAddress := app_config_struct.Connections.KafkaAddr
	topic := app_config_struct.Connections.KafkaTopic

	if brokerAddress == "" {
		return fmt.Errorf("no broker address for kafka. can't verify it's running")
	}
	if topic == "" {
		return fmt.Errorf("no topic for kafka, can't check if it's running")
	}

	log.Printf("Checking Kafka at %s...", brokerAddress)

	// 1. Check basic connectivity to the broker
	// dialer is a struct for the config to connect to the broker.
	// 		it's got settings so if the broker dies, the check will end instead of hang
	dialer := &kafka.Dialer{
		Timeout:   2 * time.Second, // reduced timeout for quicker failure if down
		DualStack: true,
	}

	// Try tcp4 first to force IPv4
	// this tries to avoid a windows problems where localhost gets resolved to ipv6 x, but kafka is listening to ipv4 y
	conn, err := dialer.Dial("tcp4", brokerAddress)
	if err != nil {
		log.Printf("Failed to connect to Kafka at %s (IPv4): %v. Attempting to start it...", brokerAddress, err)

		kafkaDir := app_config_struct.Paths.KafkaInstallDir
		if kafkaDir == "" {
			return fmt.Errorf("kafka not running and kafka_install_dir not configured in App.yaml")
		}

		// actual command: bin\windows\kafka-server-start.bat config\server.properties
		cmdPath := filepath.Join(kafkaDir, "bin", "windows", "kafka-server-start.bat")
		configPath := filepath.Join(kafkaDir, "config", "server.properties")

		// Create the command
		// Using cmd /c start ... to run in a separate window/background so it doesn't block this process
		cmd := exec.Command("cmd", "/c", "start", cmdPath, configPath)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start Kafka: %w", err)
		}

		log.Println("Kafka start command issued. Waiting for it to initialize (up to 60s)...")

		// Retry connection in a loop
		connected := false
		for i := 0; i < 30; i++ {
			time.Sleep(2 * time.Second)
			conn, err = dialer.Dial("tcp4", brokerAddress)
			if err == nil {
				connected = true
				break
			}
		}

		if !connected {
			return fmt.Errorf("ERROR: kafka wasn't running and our start command failed (or timed out) %w", err)
		}
		log.Println("Successfully connected to Kafka after startup.")
	}
	defer conn.Close()

	// 2. Check if we can read partitions (implies broker is up and responding)
	partitions, err := conn.ReadPartitions()
	if err != nil {
		return fmt.Errorf("failed to read partitions from Kafka (Kafka might be down): %w", err)
	}

	log.Printf("Kafka is running. Found %d partitions across all topics.", len(partitions))

	return nil
}

// Check/start Docker containers for Prometheus, Grafana, Redis
// telling docker to start over and over while it's already up doesn't do anything bad
func StartDockerServices() error {
	cmd := exec.Command("docker", "compose", "-f", "Global_Configs/Docker_Compose.yml", "up", "-d")

	log.Println("Starting Docker services (Redis, Prometheus, Grafana)...")

	// use these variable if I want debug output in my terminal
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start docker services: %w", err)
	}

	log.Println("Docker services started successfully.")
	return nil
}

// Stop Docker containers for Prometheus, Grafana, Redis
func StopDockerServices() error {
	log.Println("Stopping Docker services...")

	cmd := exec.Command("docker", "compose", "-f", "Global_Configs/Docker_Compose.yml", "down")

	// Inherit stdout/stderr for visibility
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stop docker services: %w", err)
	}

	log.Println("Docker services stopped successfully.")
	return nil
}

// shut down the Kafka server when the program ends. (this is called when the program ends)
// we use the stop script included with kafka
func StopKafka(app_config_struct *config.App_Config) error {
	kafkaDir := app_config_struct.Paths.KafkaInstallDir
	if kafkaDir == "" {
		return fmt.Errorf("kafka_install_dir not configured")
	}

	cmdPath := filepath.Join(kafkaDir, "bin", "windows", "kafka-server-stop.bat")

	// Check if the script exists
	if _, err := os.Stat(cmdPath); os.IsNotExist(err) {
		return fmt.Errorf("stop script not found at %s", cmdPath)
	}

	log.Println("Stopping Kafka server...")

	cmd := exec.Command(cmdPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to stop Kafka: %v, output: %s", err, string(output))
	}

	log.Println("Kafka stop command issued.")
	return nil
}
