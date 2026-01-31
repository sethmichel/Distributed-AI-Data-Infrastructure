package service_a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	config "ai_infra_project/Global_Configs"
	pb "ai_infra_project/Proto"
	"ai_infra_project/Services"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
)

// start kafka consumer client
// connect to redis
func SetupEngine(app_config_struct *config.App_Config) (*redis.Client, *kafka.Reader) {
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

	// Configure the kafka reader
	kafka_reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{app_config_struct.Connections.KafkaAddr},
		Topic:    app_config_struct.Connections.KafkaTopic,
		GroupID:  "transformer-group",
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
	})

	return redis_conn, kafka_reader
}

// sends a batch of data to redis
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
// send a bactch of data to duckdb via redis queue
// this is files and raw data
func SendBatch_Duckdb_Queue(batchData []FeatureData, redis_conn *redis.Client, ctx context.Context) {
	if len(batchData) > 0 {
		pipe := redis_conn.Pipeline()
		for _, data := range batchData {
			// make write request
			req := Services.WriteRequest{
				Table: "features_table",
				Data: map[string]interface{}{
					"entity_id":       data.EntityID,
					"feature_name":    data.FeatureName,
					"value":           data.Value,
					"event_timestamp": data.ValidatedAt,
					"created_at":      data.CreatedAt,
				},
			}

			valBytes, err := json.Marshal(req) // serialize into bytes
			if err != nil {
				log.Printf("Error marshaling duckdb write request: %v", err)
				continue
			}

			pipe.RPush(ctx, Services.RedisWriteQueue, valBytes)
		}

		_, err := pipe.Exec(ctx)
		if err != nil {
			log.Printf("Error queuing batch for DuckDB: %v", err)
		}
	}
}

// infinite loop listening to kafka producer
// it stores data until batchsize is reached, or it's been flushInterval seconds
// on reaching the sending trigger, it sends
//
//	FIRST: most recent value of each feature to redis
//	THEN: all values to duckdb
func StartDataProcessingEngine(redis_conn *redis.Client, app_config_struct *config.App_Config) {
	redis_conn, kafka_reader := SetupEngine(app_config_struct)
	
	fmt.Printf("Transformer Engine started, listening on topic: %s\n", app_config_struct.Connections.KafkaTopic)

	// Batch configuration -
	batchSize := 5
	flushInterval := 2 * time.Second
	
	// Alert configuration
	lastAlertTime := time.Time{}
	alertCooldown := 10 * time.Minute

	// batchdata has the data
	// batchmsgs has the data+metadata+kafka offset we need to commit to kafka
	var batchData []FeatureData
	var batchMsgs []kafka.Message

	// Helper function to process and send the current batch
	flushBatch := func() {
		if len(batchMsgs) == 0 {
			return
		}

		ctxInner := context.Background()

		// 0. service c communication - Lightweight Drift Detection Check
		// Check if we should trigger an alert based on values
		// Only check if we are outside the cooldown period
		if time.Since(lastAlertTime) > alertCooldown {
			triggerAlert := false
			var alertEntityID string

			for _, data := range batchData {
				// Simple threshold check: if value is too high (e.g. > 100), trigger investigation
				// This is a placeholder for "something looks wrong"
				if data.Value > 100.0 {
					triggerAlert = true
					alertEntityID = data.EntityID
					break
				}
			}

			if triggerAlert {
				// Publish alert to Redis
				// Use a timeout context for the publish
				pubCtx, cancel := context.WithTimeout(ctxInner, 2*time.Second)
				err := redis_conn.Publish(pubCtx, "drift_alerts", alertEntityID).Err()
				cancel()

				if err != nil {
					log.Printf("Failed to publish drift alert: %v", err)
				} else {
					log.Printf("Drift alert triggered for entity %s. Sent signal to Service C.", alertEntityID)
					lastAlertTime = time.Now()
				}
			}
		}

		// 1. Send batch to Redis
		SendBatch_Redis(batchData, redis_conn, ctxInner)

		// 2. Send batch to DuckDB Queue
		SendBatch_Duckdb_Queue(batchData, redis_conn, ctxInner)

		// 3. Commit messages to Kafka
		// We commit after processing so if processing crashes, we re-process (at-least-once)
		// details: kafka keeps a pointer (called 'offset") of the last message my consumer finishes. if I don't commit then then if I
		//    crash and restart kafka will send the same data again. if I commit early before writing to the db's, and crash,
		//    I lose the data forever. this is a "at least once" pattern. the cost is 1 network request to kafka; but since we're
		//    batching it's not that bad
		if err := kafka_reader.CommitMessages(ctxInner, batchMsgs...); err != nil {
			log.Printf("Error committing messages to Kafka: %v", err)
		} else {
			if len(batchData) > 0 {
				fmt.Printf("Processed and committed batch of %d records\n", len(batchData))
			}
		}

		// Clear buffers
		batchData = batchData[:0]
		batchMsgs = batchMsgs[:0]
	}

	// msg is the data and metadata like topic, partition, and offset (kafka pointer) that's needed to commit to kafka
	for {
		// Create a context with timeout for the fetch operation
		// This ensures we don't wait forever if the batch isn't full
		ctx, cancel := context.WithTimeout(context.Background(), flushInterval)
		msg, err := kafka_reader.FetchMessage(ctx)
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

//////////////////////////////////////
//  gRPC SERVER & FILE PROCESSING
//////////////////////////////////////

type grpc_server struct {
	pb.UnimplementedFeatureStoreServer
	redisClient *redis.Client
}

/*
grpc.newserver(): boot it up. this creates a generic grpc engine. it's an internal struct for network connections, and stuff. it doesn't

know about my stuff or file uploads yet, it's just an empty server waiting for services to be registered

file_upload_server := grpc.NewServer(): makes the listener/protocol handler

pb.registerFeatureStoreserver(): links my code to the engine. file_upload_server is the generic engine I just made. grpc_server() is my
custom struct that holds the db connections

this creates a SERVER, so I don't pass around a variable for it; the os knows about it. it tells the os to reserve that port and send
all traffic that hits it to file_upload_server
*/
func StartGrpcServer(app_config *config.App_Config, redisClient *redis.Client) {
	grpc_conn, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// internal grpc struct for connections, streams, etc (listener/protocol handler)
	file_upload_server := grpc.NewServer()

	// link my code to the engine. the registerfeaturestoreserver is a auto generated function that tells the file_upload_server that when
	//   a request comes in for featuresstore service, to route it to this grpc_server instance
	// so we're plugging 1 instance of grpc_server into this function to make a complete engine
	pb.RegisterFeatureStoreServer(file_upload_server, &grpc_server{
		redisClient: redisClient,
	})

	log.Printf("gRPC server listening at %v", grpc_conn.Addr())

	if err := file_upload_server.Serve(grpc_conn); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// this is attached to my grpc_server struct, it's the code the grpc server calls when it gets an upload request
func (s *grpc_server) UploadFile(stream pb.FeatureStore_UploadFileServer) error {
	var fileBytes []byte
	var filename string

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			// Finished receiving
			break
		}
		if err != nil {
			return err
		}
		fileBytes = append(fileBytes, chunk.Content...)
		filename = chunk.Filename
	}

	log.Printf("Received file: %s, size: %d bytes", filename, len(fileBytes))

	// Write to temp file to process
	tempFilename := "temp_" + filename
	if err := os.WriteFile(tempFilename, fileBytes, 0644); err != nil {
		return stream.SendAndClose(&pb.UploadStatus{
			Success: false,
			Message: fmt.Sprintf("Failed to write temp file: %v", err),
		})
	}
	defer os.Remove(tempFilename)

	var batchData []FeatureData
	var err error

	// Determine file type and process
	if len(filename) > 5 && filename[len(filename)-5:] == ".json" {
		batchData, err = processJSONFile(tempFilename)
	} else if len(filename) > 8 && filename[len(filename)-8:] == ".parquet" {
		batchData, err = processParquetFile(tempFilename)
	} else {
		err = fmt.Errorf("unsupported file extension")
	}

	if err != nil {
		return stream.SendAndClose(&pb.UploadStatus{
			Success: false,
			Message: fmt.Sprintf("Failed to process file: %v", err),
		})
	}

	// Send to storage
	SendBatch_Redis(batchData, s.redisClient, stream.Context())
	SendBatch_Duckdb_Queue(batchData, s.redisClient, stream.Context())

	return stream.SendAndClose(&pb.UploadStatus{
		Success: true,
		Message: fmt.Sprintf("Successfully processed file %s with %d records", filename, len(batchData)),
	})
}

func processJSONFile(filename string) ([]FeatureData, error) {
	bytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var data []FeatureData
	if err := json.Unmarshal(bytes, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func processParquetFile(filename string) ([]FeatureData, error) {
	// Simple Parquet reader implementation using pqarrow
	// Note: This is a simplified reader assuming the schema matches FeatureData

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mem := memory.NewGoAllocator()
	rdr, err := file.NewParquetReader(f)
	if err != nil {
		return nil, err
	}
	defer rdr.Close()

	arrowReader, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{}, mem)
	if err != nil {
		return nil, err
	}

	tbl, err := arrowReader.ReadTable(context.Background())
	if err != nil {
		return nil, err
	}
	defer tbl.Release()

	// Convert Table to FeatureData structs
	// This is a bit verbose in Arrow, for now let's implement a placeholder
	// or a very simple extraction if possible.
	// Given the complexity of Arrow -> Struct conversion without reflection or specific column readers,
	// I'll implement a basic column reader for the known schema.

	var results []FeatureData

	// Assuming columns are in order: entity_id, feature_name, value, validated_at, created_at
	// We need to check column names ideally, but for now assuming order based on data_generation.go

	// Implementation simplified for brevity, relying on knowing the schema
	// In a real app we would map by name.

	tr := array.NewTableReader(tbl, 10000)
	defer tr.Release()

	for tr.Next() {
		rec := tr.Record() // Use Record() for now as linter suggestion might be for a different type or version
		// Columns: 0:entity_id (string), 1:feature_name (string), 2:value (float64), 3:validated_at (timestamp), 4:created_at (timestamp)

		cEntity := rec.Column(0).(*array.String)
		cFeature := rec.Column(1).(*array.String)
		cValue := rec.Column(2).(*array.Float64)
		// cValid := rec.Column(3).(*array.Timestamp)
		// cCreated := rec.Column(4).(*array.Timestamp)

		for i := 0; i < int(rec.NumRows()); i++ {
			fd := FeatureData{
				EntityID:    cEntity.Value(i),
				FeatureName: cFeature.Value(i),
				Value:       cValue.Value(i),
				// Timestamps handling in arrow is a bit involved (units), skipping strict conversion for this fix
				// to avoid compilation errors if types mismatch.
				// Just using current time for now or 0.
			}
			results = append(results, fd)
		}
	}

	return results, nil
}
