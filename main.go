package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	config "ai_infra_project/Global_Configs"
	service_a "ai_infra_project/Services/Service_A"
)

func main() {
	log.Println("Starting System Setup Checks...")

	// 1. Load configs (app.yaml and azure)
	app_config_struct, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// 2 Setup shutdown handling - we want servers and stuff to end when the program ends
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Println("\nReceived interrupt signal. Shutting down...")
		if err := StopKafka(app_config_struct); err != nil {
			log.Printf("Error stopping Kafka during shutdown: %v", err)
		}
		if err := StopDockerServices(); err != nil {
			log.Printf("Error stopping Docker services during shutdown: %v", err)
		}
		os.Exit(0)
	}()

	// 3. Check/create DuckDB
	if err := CheckDuckDB(app_config_struct); err != nil {
		log.Fatalf("DuckDB check failed: %v", err)
	}

	// 4. check/create docker containers for promethious, granfana, redis
	if err := StartDockerServices(); err != nil {
		log.Fatalf("Docker services check failed: %v", err)
	}

	// 5. Check/start Kafka
	if err := CheckKafka(app_config_struct); err != nil {
		log.Fatalf("Kafka check failed: %v", err)
	}

	log.Println("All system checks completed successfully.")

	// SERVICE A
	log.Println("Starting Service A...")
	go service_a.Start(app_config_struct)

	log.Println("All system checks completed successfully. System is running.")
	select {} // infinite loop
}
