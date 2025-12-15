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

	// connect to ensure table exists
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
		return fmt.Errorf("failed to create features table: %w", err)
	}
	log.Printf("DuckDB initialized successfully (table checked) at %s.", path)

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
	cmd := exec.Command("docker", "compose", "-f", "Global_Configs/docker-compose.yml", "up", "-d")

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

	cmd := exec.Command("docker", "compose", "-f", "Global_Configs/docker-compose.yml", "down")

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
