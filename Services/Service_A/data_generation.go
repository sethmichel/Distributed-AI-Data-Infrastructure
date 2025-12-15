package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	config "ai_infra_project/Global_Configs"
	pb "ai_infra_project/Proto"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/segmentio/kafka-go"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type FeatureData struct {
	EntityID    string    `json:"entity_id"`
	FeatureName string    `json:"feature_name"`
	Value       float64   `json:"value"`
	ValidatedAt time.Time `json:"validated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

func main() {
	// Load Configuration
	app_config_struct, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize Kafka writer
	writer := &kafka.Writer{
		Addr:     kafka.TCP(app_config_struct.Connections.KafkaAddr),
		Topic:    app_config_struct.Connections.KafkaTopic,
		Balancer: &kafka.LeastBytes{},
	}
	defer writer.Close()

	// Start transformer engine (consumer)
	go StartTransformerEngine(app_config_struct)

	// Start data generation routine
	go generateData(writer)

	// Start gRPC upload routine
	go runGrpcUploader()

	// Keep the main function running. since main() is go's master function, when it ends the program / routines end.
	// select {} means block forever. it pauses main() until I manually stop the program
	select {}
}

// go routine function
// we'll make 100 id's, and each id has x features. we then produce data of different values for those features
// it's iterating entity id's and doing 1 entry per feature. then goes to the next entity id.
func generateData(writer *kafka.Writer) {
	iteration_timer := time.NewTicker(500 * time.Millisecond) // timer for every 0.5 seconds
	defer iteration_timer.Stop()

	entityCount := 100
	// these are random features because this is a learning project
	features := []string{"click_count", "view_time", "purchase_amount", "logins_7d", "error_rate"}

	entityIdx := 0
	featureIdx := 0

	// create random data
	// remember that .C means channel, and that the ticker sends data to the channel which this loop iterates over
	//    the code will basically sleep when it reaches the end of the for loop until the channel gets more data.
	//    basically infinite while loop with auto sleep
	for range iteration_timer.C {
		entityID := fmt.Sprintf("user_%d", entityIdx+1)
		featureName := features[featureIdx]

		data := FeatureData{
			EntityID:    entityID,
			FeatureName: featureName,
			Value:       rand.Float64() * 100,
			ValidatedAt: time.Now(),
			CreatedAt:   time.Now(),
		}

		// Send to transformer engine
		SendToTransformer(writer, data)

		// Cycle through features and entities
		featureIdx++
		if featureIdx >= len(features) {
			featureIdx = 0
			entityIdx++
			if entityIdx >= entityCount {
				entityIdx = 0
			}
		}
	}
}

// sends data to kafka who sends it to the destination
func SendToTransformer(writer *kafka.Writer, data FeatureData) {
	jsonData, err := json.Marshal(data) // marshal is a serializer. it converts it to bytes
	if err != nil {
		log.Printf("Error converting data for kafka format: %v", err)
		return
	}

	err = writer.WriteMessages(context.Background(),
		kafka.Message{
			Key:   []byte(data.EntityID),
			Value: jsonData,
		},
	)
	if err != nil {
		log.Printf("Error writing to Kafka: %v", err)
	} else {
		// fmt.Printf("Sent to Kafka: %+v\n", data) // Optional logging
	}
}

// go routine function
// generate a json or parquet file (alternate between them) of random data
func runGrpcUploader() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	useParquet := false

	for range ticker.C {
		useParquet = !useParquet // Alternate between formats

		// Connect to the gRPC server (assuming localhost:50051 for now)
		// opens tcp network to localhost on that port
		conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Printf("Failed to connect to gRPC server: %v", err)
			continue
		}

		// auto generated function. clint is a translator, when I call a function on it, it converts my go data into binary and
		//      sends it over conn
		client := pb.NewFeatureStoreClient(conn)
		uploadFile(client, useParquet)

		conn.Close()
	}
}

// called in file upload go routine process
func uploadFile(client pb.FeatureStoreClient, useParquet bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var content []byte
	var filename string
	var err error

	if useParquet {
		content, filename, err = generateParquetFile()
	} else {
		content, filename, err = generateJSONFile()
	}

	if err != nil {
		log.Printf("Error generating file: %v", err)
		return
	}
	// Clean up the file after sending (optional, but good practice to avoid clutter)
	defer os.Remove(filename)

	// remember, client is the translator. so this is the stream we're going to use to send data.
	stream, err := client.UploadFile(ctx)
	if err != nil {
		log.Printf("Error creating stream: %v", err)
		return
	}

	// now we send the data to the translator (client) and it's converted to binary
	if err := stream.Send(&pb.FileChunk{
		Content:  content,
		Filename: filename,
	}); err != nil {
		log.Printf("Error sending chunk: %v", err)
		return
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		log.Printf("Error receiving response: %v", err)
		return
	}

	log.Printf("Upload status: %v", resp)
}

// this is random data just to have the system be able to handle this file
func generateJSONFile() ([]byte, string, error) {
	features := []string{"click_count", "view_time", "purchase_amount", "logins_7d", "error_rate"}
	var dataList []FeatureData

	// Generate a batch of 10 random records
	for i := 0; i < 10; i++ {
		entityID := fmt.Sprintf("user_%d", rand.Intn(100)+1)
		featureName := features[rand.Intn(len(features))]

		data := FeatureData{
			EntityID:    entityID,
			FeatureName: featureName,
			Value:       rand.Float64() * 100,
			ValidatedAt: time.Now(),
			CreatedAt:   time.Now(),
		}
		dataList = append(dataList, data)
	}

	jsonData, err := json.MarshalIndent(dataList, "", "  ")
	if err != nil {
		return nil, "", err
	}

	filename := fmt.Sprintf("data_%d.json", time.Now().UnixNano())
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return nil, "", err
	}

	log.Printf("Generated JSON file: %s", filename)
	return jsonData, filename, nil
}

// arrow a high performance. we're making it so memoriy buffers used for the columnar data are compatable with
//
//	go's garbage collector
func generateParquetFile() ([]byte, string, error) {
	features := []string{"click_count", "view_time", "purchase_amount", "logins_7d", "error_rate"}

	// uses go's malloc for managing the arrow arrays
	pool := memory.NewGoAllocator()

	schema := arrow.NewSchema(
		[]arrow.Field{
			{Name: "entity_id", Type: arrow.BinaryTypes.String},
			{Name: "feature_name", Type: arrow.BinaryTypes.String},
			{Name: "value", Type: arrow.PrimitiveTypes.Float64},
			{Name: "validated_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}},
			{Name: "created_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}},
		},
		nil,
	)

	// this is a "record builder". it's a struct containing the collection of specific column builders (like intBuilder)
	//      that matches our schema we made
	myRecordBuilder := array.NewRecordBuilder(pool, schema)
	// arrow is doing memeory management, and release is basically decementing the memory until it hits zero
	//    on 0, memory is freed, so it's anti-memory leak
	defer myRecordBuilder.Release()

	// Builders
	sbEntity := myRecordBuilder.Field(0).(*array.StringBuilder)
	sbFeature := myRecordBuilder.Field(1).(*array.StringBuilder)
	fbValue := myRecordBuilder.Field(2).(*array.Float64Builder)
	tbValid := myRecordBuilder.Field(3).(*array.TimestampBuilder)
	tbCreated := myRecordBuilder.Field(4).(*array.TimestampBuilder)

	// Generate 10 random records
	for i := 0; i < 10; i++ {
		entityID := fmt.Sprintf("user_%d", rand.Intn(100)+1)
		featureName := features[rand.Intn(len(features))]
		val := rand.Float64() * 100
		now := time.Now()
		ts := arrow.Timestamp(now.UnixMicro())

		sbEntity.Append(entityID)
		sbFeature.Append(featureName)
		fbValue.Append(val)
		tbValid.Append(ts)
		tbCreated.Append(ts)
	}

	// Manually build columns since b.NewRecord() is deprecated
	cols := make([]arrow.Array, len(schema.Fields()))
	rows := int64(0)
	for i, fieldBuilder := range myRecordBuilder.Fields() {
		cols[i] = fieldBuilder.NewArray()
		defer cols[i].Release()
		if i == 0 {
			rows = int64(cols[i].Len())
		}
	}

	// TODO: need to run the code to see if this function works, my ide says it's deprecated
	rec := array.NewRecord(schema, cols, rows)
	defer rec.Release()

	filename := fmt.Sprintf("data_%d.parquet", time.Now().UnixNano())
	parquet_file, err := os.Create(filename)
	if err != nil {
		return nil, "", err
	}

	// Create Parquet Writer
	parquetFileWriter, err := pqarrow.NewFileWriter(schema, parquet_file, nil, pqarrow.DefaultWriterProps())
	if err != nil {
		parquet_file.Close()
		return nil, "", err
	}

	if err := parquetFileWriter.Write(rec); err != nil {
		parquetFileWriter.Close()
		parquet_file.Close()
		return nil, "", err
	}

	parquetFileWriter.Close()
	parquet_file.Close()

	// Read back to return bytes
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", err
	}

	log.Printf("Generated Parquet file: %s", filename)
	return content, filename, nil
}
