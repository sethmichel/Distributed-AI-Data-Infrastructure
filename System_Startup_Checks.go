package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	config "ai_infra_project/Global_Configs"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/segmentio/kafka-go"
)

/*
1. duckdb directory and features table and metadata table
2. duckdb has model metadata in metadata table
3. save the required models names into redis so we know what's downloaded
4. check/start kafka server is running
5. start docker services (redis, promethius, granfana)
6. load prod status models into redis on startup

Service d loads the model artifacts and other info into redis which service b uses
	- block service b until service d is done starting up

*/

func DuckdbTablesQueries() (string, string, string) {
	featureTableQuery := `
		CREATE TABLE IF NOT EXISTS features_table (
			entity_id TEXT,
			feature_name TEXT,
			value DOUBLE,
			event_timestamp TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`

	metadataTableQuery := `
	CREATE TABLE IF NOT EXISTS model_metadata (
		model_id TEXT,
		artifact_type TEXT,
		version TEXT,
		trained_date DATE,
		model_drift_score DOUBLE,
		status TEXT,
		azure_location TEXT,
		expected_features STRUCT("index" INTEGER, "name" TEXT, "type" TEXT, "drift_score" DOUBLE)[],
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	modelLogsQuery := `
	CREATE TABLE IF NOT EXISTS model_logs (
		model_id TEXT,
		model_version TEXT,
		model_inputs TEXT,
		model_result TEXT,
		model_error TEXT,
		event_datetime TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	return featureTableQuery, metadataTableQuery, modelLogsQuery
}

func CheckPathsDirs() error {
	// TODO
	return nil
}

// checks directories exist
func CheckDuckDB(app_config_struct *config.App_Config) error {
	duckDbPath := app_config_struct.Connections.DuckDBPath
	featureTableQuery, metadataTableQuery, modelLogsQuery := DuckdbTablesQueries()

	// connect to duckdb to ensure db actually exists
	db, err := sql.Open("duckdb", duckDbPath)
	if err != nil {
		return fmt.Errorf("failed to open duckdb: %w", err)
	}
	defer db.Close()

	// Create features table if it doesn't exist
	if _, err := db.Exec(featureTableQuery); err != nil {
		return fmt.Errorf("failed to create features table: %w", err)
	}
	log.Printf("features table is present in duckdb")

	// create model metadata table if it doesn't exist
	// expected_features STRUCT is a list, so I can add 1 for each feature
	if _, err := db.Exec(metadataTableQuery); err != nil {
		return fmt.Errorf("failed to create model_metadata table: %w", err)
	}

	// check metadata for required models is present (it returns true/false for each model)
	// these are placeholder models, we can use anything but I just use these basic ones
	requiredModels := []string{"Loan_Approve", "House_Price", "Coffee_Recommender"}
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

	// check/create model logs table
	if _, err := db.Exec(modelLogsQuery); err != nil {
		return fmt.Errorf("failed to create model logs table in duckdb: %w", err)
	}
	log.Printf("model logs table is present in duckdb")

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
		if os.Getenv("RUN_IN_K3S") == "true" {
			return fmt.Errorf("failed to connect to Kafka at %s: %w (K3s mode - not attempting to start local server)", brokerAddress, err)
		}

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
