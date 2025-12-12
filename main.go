package main

import (
	"log"

	config "ai_infra_project/Global_Configs"
)

func main() {
	log.Println("Starting System Setup Checks...")

	// 1. Load configs (app.yaml and azure)
	app_config_struct, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// 2. Check DuckDB
	if err := CheckDuckDB(app_config_struct); err != nil {
		log.Fatalf("DuckDB check failed: %v", err)
	}

	log.Println("All system checks completed successfully.")
}
