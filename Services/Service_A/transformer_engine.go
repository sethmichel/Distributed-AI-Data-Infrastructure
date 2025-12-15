package service_a

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	config "ai_infra_project/Global_Configs"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

// start kafka consumer client
// connect to redis and duckdb
func SetupEngine(app_config_struct *config.App_Config) (*redis.Client, *sql.DB, *kafka.Reader) {
	// Redis Connection
	redis_conn := redis.NewClient(&redis.Options{
		Addr: app_config_struct.Connections.RedisAddr,
	})

	// Test Redis connection
	if _, err := redis_conn.Ping(context.Background()).Result(); err != nil {
		log.Printf("Warning: Failed to connect to Redis at %s: %v", app_config_struct.Connections.RedisAddr, err)
	} else {
		log.Println("Connected to Redis successfully")
	}

	// DuckDB Connection
	duck_conn, err := sql.Open("duckdb", app_config_struct.Connections.DuckDBPath)
	if err != nil {
		log.Fatalf("Failed to open DuckDB at %s: %v", app_config_struct.Connections.DuckDBPath, err)
	}

	// Configure the reader
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{app_config_struct.Connections.KafkaAddr},
		Topic:    app_config_struct.Connections.KafkaTopic,
		GroupID:  "transformer-group",
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
	})

	return redis_conn, duck_conn, reader
}

// sends a batch of kafka data to redis
func SendBatch_Redis(batchData []FeatureData, redis_conn *redis.Client, ctxInner context.Context) {
	if len(batchData) > 0 {
		pipe := redis_conn.Pipeline()
		for _, data := range batchData {
			// Key: entity_id:feature_name
			redisKey := fmt.Sprintf("%s:%s", data.EntityID, data.FeatureName)

			// Store value and timestamp
			valMap := map[string]interface{}{
				"val":       data.Value,
				"timestamp": data.ValidatedAt,
			}
			valBytes, err := json.Marshal(valMap)
			if err != nil {
				log.Printf("Error marshaling redis value: %v", err)
				continue
			}

			pipe.Set(ctxInner, redisKey, valBytes, 0)
		}
		_, err := pipe.Exec(ctxInner)
		if err != nil {
			log.Printf("Error writing batch to Redis: %v", err)
		}
	}
}

// Schema: entity_id, feature_name, value, event_timestamp
func SendBatch_Duckdb(batchData []FeatureData, duck_conn *sql.DB) {
	if len(batchData) > 0 {
		tx, err := duck_conn.Begin()
		if err != nil {
			log.Printf("Error starting DuckDB transaction: %v", err)
		} else {
			// Using prepared statement for efficiency
			stmt, err := tx.Prepare("INSERT INTO features (entity_id, feature_name, value, event_timestamp, created_at) VALUES (?, ?, ?, ?, ?)")
			if err != nil {
				log.Printf("Error preparing DuckDB statement: %v", err)
				tx.Rollback()
			} else {
				defer stmt.Close()
				for _, data := range batchData {
					_, err := stmt.Exec(data.EntityID, data.FeatureName, data.Value, data.ValidatedAt, data.CreatedAt)
					if err != nil {
						log.Printf("Error executing DuckDB insert: %v", err)
					}
				}
				if err := tx.Commit(); err != nil {
					log.Printf("Error committing DuckDB transaction: %v", err)
				}
			}
		}
	}
}

// infinite loop listening to kafka producer
// it stores data until batchsize is reached, or it's been flushInterval seconds
// on reaching the sending trigger, it sends
//
//	FIRST: most recent value of each feature to redis
//	THEN: all values to duckdb
func StartTransformerEngine(app_config_struct *config.App_Config) {
	redis_conn, duck_conn, reader := SetupEngine(app_config_struct)

	defer reader.Close()
	defer duck_conn.Close()
	defer redis_conn.Close()

	fmt.Printf("Transformer Engine started, listening on topic: %s\n", app_config_struct.Connections.KafkaTopic)

	// Batch configuration
	batchSize := 5
	flushInterval := 2 * time.Second

	var batchData []FeatureData
	var batchMsgs []kafka.Message

	// Helper function to process and send the current batch
	flushBatch := func() {
		if len(batchMsgs) == 0 {
			return
		}

		ctxInner := context.Background()

		// 1. Send batch to Redis (do this before duckdb)
		SendBatch_Redis(batchData, redis_conn, ctxInner)

		// 2. Send batch to DuckDB
		SendBatch_Duckdb(batchData, duck_conn)

		// 3. Commit messages to Kafka
		// We commit after processing so if processing crashes, we re-process (at-least-once)
		// details: kafka keeps a pointer (called 'offset") of the last message my consumer finishes. if I don't commit then then if I
		//    crash and restart kafka will send the same data again. if I commit early before writing to the db's, and crash,
		//    I lose the data forever. this is a "at least once" pattern. the cost is 1 network request to kafka; but since we're
		//    batching it's not that bad
		if err := reader.CommitMessages(ctxInner, batchMsgs...); err != nil {
			log.Printf("Error committing messages to Kafka: %v", err)
		} else {
			if len(batchData) > 0 {
				fmt.Printf("Processed and committed batch of %d records\n", len(batchData))
			}
		}

		// Clear buffers
		// batchdata has the data
		// batchmsgs has the data+metadata+kafka offset we need to commit to kafka
		batchData = batchData[:0]
		batchMsgs = batchMsgs[:0]
	}

	// msg is the data and metadata like topic, partition, and offset (kafka pointer) that's needed to commit to kafka
	for {
		// Create a context with timeout for the fetch operation
		// This ensures we don't wait forever if the batch isn't full
		ctx, cancel := context.WithTimeout(context.Background(), flushInterval)
		msg, err := reader.FetchMessage(ctx)
		cancel() // Cancel context immediately after fetch returns to release resources

		if err != nil {
			if err == context.DeadlineExceeded {
				// Timeout reached, flush what we have
				flushBatch()
				continue
			}
			log.Printf("Error reading message: %v", err)
			// Wait a bit before retrying to avoid tight loop on persistent errors
			time.Sleep(1 * time.Second)
			continue
		}

		var data FeatureData
		if err := json.Unmarshal(msg.Value, &data); err != nil {
			log.Printf("Error unmarshaling message: %v", err)
			// Still add to batchMsgs to commit and move past the bad message
			// But don't add to batchData
			batchMsgs = append(batchMsgs, msg)
		} else {
			batchData = append(batchData, data)
			batchMsgs = append(batchMsgs, msg)
		}

		// Check if batch size reached
		if len(batchMsgs) >= batchSize {
			flushBatch()
		}
	}
}
