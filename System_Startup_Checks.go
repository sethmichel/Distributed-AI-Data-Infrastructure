package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	config "ai_infra_project/Global_Configs"

	_ "github.com/marcboeker/go-duckdb/v2"
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
		// (entity_id, feature_name, value, valid_at, created_at)
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
