package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	config "ai_infra_project/Global_Configs"

	"github.com/segmentio/kafka-go"
)

// starts the Kafka consumer for the transformer engine
func StartTransformerEngine(app_config_struct *config.App_Config) {
	// Configure the reader
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{app_config_struct.Connections.KafkaAddr},
		Topic:    app_config_struct.Connections.KafkaTopic,
		GroupID:  "transformer-group",
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
	})
	defer reader.Close()

	fmt.Printf("Transformer Engine started, listening on topic: %s\n", app_config_struct.Connections.KafkaTopic)

	// this loop is efficient enough
	for {
		m, err := reader.ReadMessage(context.Background())
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		var data FeatureData
		if err := json.Unmarshal(m.Value, &data); err != nil { // unmarshal means deserialize. bytes -> json
			log.Printf("Error unmarshaling message: %v", err)
			continue
		}

		// Process the data (currently just printing)
		fmt.Printf("Transformer received data from Kafka: %+v\n", data)
	}
}
