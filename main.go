package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	config "ai_infra_project/Global_Configs"
	"ai_infra_project/Services"

	service_a "ai_infra_project/Services/Service_A"
	service_b "ai_infra_project/Services/Service_B"
	service_c "ai_infra_project/Services/Service_C"
	service_d "ai_infra_project/Services/Service_D"
)

func main() {
	log.Println("Starting System Setup Checks...")

	// 1. Load configs (app.yaml and azure)
	app_config_struct, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// 2. check files/directories all exist
	if err := CheckPathsDirs(); err != nil {
		log.Fatalf("File or directory major issue: %v", err)
	}

	// 3. Setup shutdown handling - we want servers and stuff to end when the program ends
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// 4. check/create docker containers for promethious, granfana, redis
	// if err := StartDockerServices(); err != nil {
	// 	log.Fatalf("Docker services check failed: %v", err)
	// }

	// 5. Check/create DuckDB and tables
	if err := CheckDuckDB(app_config_struct); err != nil {
		log.Fatalf("DuckDB check failed: %v", err)
	}

	// 5. START DB SERVICE (Must be before any service tries to query it)
	log.Println("Starting DB Handler...")
	if err := Services.StartDBHandler(context.Background(), app_config_struct); err != nil {
		log.Fatalf("Failed to start DB Handler: %v", err)
	}

	// 6. Check/start Kafka
	if err := CheckKafka(app_config_struct); err != nil {
		log.Fatalf("Kafka check failed: %v", err)
	}

	log.Println("All system checks completed successfully. Now starting up services...")

	// 7. START SERVICE D (loads prod model names, metadata, and artifacts into redis on startup)
	// service b uses this data
	if err := service_d.Service_D_Start(app_config_struct); err != nil {
		log.Fatalf("Service D failed to start: %v", err)
	}

	// 8. START SERVICE A - data generation
	log.Println("Starting Service A...")
	go service_a.ServiceAStart(app_config_struct)

	// 9. START SERVICE B - model servicing
	log.Println("Starting service B...")
	go service_b.Service_B_Start(app_config_struct)

	// 10. START SERVICE C - model metrics
	log.Println("Starting Service C...")
	go service_c.StartServiceC()

	log.Println("All system checks completed successfully. System is running. Press CTRL+C to stop.")

	// Wait for interrupt signal
	<-c
	log.Println("\nReceived interrupt signal. Shutting down...")

	// Cleanup - I can also run stop_system.bat
	if err := StopKafka(app_config_struct); err != nil {
		log.Printf("Error stopping Kafka during shutdown: %v", err)
	}
	// if err := StopDockerServices(); err != nil {
	// 	log.Printf("Error stopping Docker services during shutdown: %v", err)
	// }

	log.Println("Shutdown complete.")
}
