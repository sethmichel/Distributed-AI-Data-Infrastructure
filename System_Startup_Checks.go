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

// check redis has python worker count
// check azure is ok
// check duckdb is made
// check yaml, yml, env files
// check docker containers are active and prometheus/granfana is running
// check the DuckDB file exists and the tables are created
func CheckDuckDB(app_config_struct *config.App_Config) error {
	path := app_config_struct.Connections.DuckDBPath

	// check directory exists
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory for duckdb: %w", err)
		}
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("DuckDB file not found at %s. Creating...", path)

		// Connect to create file and table
		db, err := sql.Open("duckdb", path)
		if err != nil {
			return fmt.Errorf("failed to open duckdb: %w", err)
		}
		defer db.Close()

		// Create table
		// event timestamp: When the event actually happened
		query := `
            CREATE TABLE IF NOT EXISTS features (
                entity_id TEXT,
                feature_name TEXT,
                value DOUBLE,
                event_timestamp TIMESTAMP,
                created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
            );`

		if _, err := db.Exec(query); err != nil {
			return fmt.Errorf("failed to create Prices table: %w", err)
		}
		log.Println("DuckDB initialized successfully with Prices table.")
	} else {
		log.Printf("DuckDB file found at %s.", path)
	}

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

	conn, err := dialer.Dial("tcp", brokerAddress)
	if err != nil {
		log.Printf("Failed to connect to Kafka at %s: %v. Attempting to start it...", brokerAddress, err)

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

		log.Println("Kafka start command issued. Waiting 5 seconds for it to initialize...")
		time.Sleep(5 * time.Second)

		// Retry connection
		conn, err = dialer.Dial("tcp", brokerAddress)
		if err != nil {
			return fmt.Errorf("ERROR: kafka wasn't running and our start command failed %w", err)
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
