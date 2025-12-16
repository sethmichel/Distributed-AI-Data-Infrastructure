package service_a

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"time"

	config "ai_infra_project/Global_Configs"
	pb "ai_infra_project/Proto"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/redis/go-redis/v9"
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

type grpc_server struct {
	pb.UnimplementedFeatureStoreServer
	redisClient *redis.Client
	duckDB      *sql.DB
}

func Start(app_config_struct *config.App_Config) {
	// Initialize Kafka writer
	writer := &kafka.Writer{
		Addr:     kafka.TCP(app_config_struct.Connections.KafkaAddr),
		Topic:    app_config_struct.Connections.KafkaTopic,
		Balancer: &kafka.LeastBytes{},
	}
	defer writer.Close()

	// Setup connections - we do this here because grpc needs the connections. otherwise we'd do it in the transformer engine
	redis_conn, duck_conn, reader := SetupEngine(app_config_struct)
	// Note: We cannot defer Close here if we want them to survive in the goroutines,
	// BUT since Start blocks with select{}, defers here will only run when Start exits,
	// which is what we want.
	defer redis_conn.Close()
	defer duck_conn.Close()
	defer reader.Close()

	// Start transformer engine (consumer)
	go StartTransformerEngine(reader, redis_conn, duck_conn, app_config_struct)

	// Start gRPC Server
	go StartGrpcServer(app_config_struct, redis_conn, duck_conn)

	// Start data generation routine
	go generateData(writer)

	// Start gRPC upload routine
	go runGrpcUploader()

	// Keep the function running so the writer stays open
	select {}
}

//////////////////////////////////
//  BASIC DATA GENERATION (kafka)
//////////////////////////////////

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

//////////////////////////////////////
//  FILE GENERATION AND UPLOAD (gRPC)
//////////////////////////////////////

// overall, this is just setting up a grpc server, and using a go routine client to send files to it

/*
	grpc.newserver(): boot it up. this creates a generic grpc engine. it's an internal struct for network connections, and stuff. it doesn't

know about my stuff or file uploads yet, it's just an empty server waiting for services to be registered

file_upload_server := grpc.NewServer(): makes the listener/protocol handler

pb.registerFeatureStoreserver(): links my code to the engine. file_upload_server is the generic engine I just made. grpc_server() is my
custom struct that holds the db connections

this creates a SERVER, so I don't pass around a variable for it; the os knows about it. it tells the os to reserve that port and send
all traffic that hits it to file_upload_server
*/
func StartGrpcServer(app_config *config.App_Config, redisClient *redis.Client, duckDB *sql.DB) {
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
		duckDB:      duckDB,
	})

	log.Printf("gRPC server listening at %v", grpc_conn.Addr())

	if err := file_upload_server.Serve(grpc_conn); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// go routine function
// generate a json or parquet file (alternate between them) of random data
// this is a CLIENT. it's using the server we made. it's using the port which gets directed to teh file_upload_server
func runGrpcUploader() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	useParquet := false

	// Connect to the gRPC server (assuming localhost:50051 for now)
	// opens tcp network to localhost on that port
	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("Failed to connect to gRPC server: %v", err)
		return
	}
	defer conn.Close()

	// auto generated function. client is a translator, when I call a function on it, it converts my go data into binary and
	//      sends it over conn
	client := pb.NewFeatureStoreClient(conn)

	for range ticker.C {
		useParquet = !useParquet // Alternate between formats

		uploadFileClient(client, useParquet)
	}
}

// called in file upload go routine process
// use parquet alternates value so we send both json and parquet files
func uploadFileClient(client pb.FeatureStoreClient, useParquet bool) {
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
	// delete files locally after sending
	defer os.Remove(filename)

	// client is the translator. so this is the STREAM we're going to use to send data
	stream, err := client.UploadFile(ctx)
	if err != nil {
		log.Printf("Error creating stream: %v", err)
		return
	}

	// now we send the data to the translator and it's converted to binary
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
	rec := array.NewRecordBatch(schema, cols, rows)
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
	SendBatch_Duckdb(batchData, s.duckDB)

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

func timeFromISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}
