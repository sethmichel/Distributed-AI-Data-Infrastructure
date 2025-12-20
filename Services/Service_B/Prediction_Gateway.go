package service_b

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	config "ai_infra_project/Global_Configs"
	pb "ai_infra_project/Proto"
	"ai_infra_project/Services"

	"bufio"
	"strconv"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// represents the structure of the model configuration
type ModelFeature struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Type  string `json:"type"`
}

// we'll need to scan this often to swap models
type ModelMetadata struct {
	ModelID          string         `json:"model_id"`
	Version          string         `json:"version"`
	TrainedDate      string         `json:"trained_date"`
	Status           string         `json:"status"`
	AzureLocation    string         `json:"azure_location"`
	ExpectedFeatures []ModelFeature `json:"expected_features"`
}

// azure connection
type AzureConfig struct {
	ConnectionString string
	ContainerName    string
}

// manages the model artifacts read operations from azure to redis
// what is DBServiceClient? it's a redis connection but it's used to talk to the db service since we route duckdb requests to
//
//	a redis job queue. it abstracts the complexity of this away from the other redis connection
//	it also does the blocking system for the db stuff
type modelArtifactHandler struct {
	redisJobQueueClient *Services.DBJobQueueClient // see comment about why we have this
	redisClient         *redis.Client
	azureClient         *azblob.Client
	config              *AzureConfig
}

// creates a new handler struct
func CreateNewHandler(redisAddr string) (*modelArtifactHandler, error) {
	// Load azure variables
	if err := godotenv.Load("Global_Configs/Env/Azure.env"); err != nil {
		log.Printf("ERROR: could not load .env file: %v", err)
	}

	azure_conn_str := os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	if azure_conn_str == "" {
		return nil, fmt.Errorf("AZURE_STORAGE_CONNECTION_STRING is not set")
	}

	azure_container_name := os.Getenv("AZURE_CONTAINER_NAME")
	if azure_container_name == "" {
		return nil, fmt.Errorf("AZURE_CONTAINER_NAME is not set")
	}

	// Create azure Client
	azure_client, err := azblob.NewClientFromConnectionString(azure_conn_str, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create azure client: %w", err)
	}

	// Create redis Client
	redis_client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	// Create DB Client
	job_queue_db_client, err := Services.NewDBJobQueueClient(redisAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create db client: %w", err)
	}

	return &modelArtifactHandler{
		redisJobQueueClient: job_queue_db_client,
		redisClient:         redis_client,
		azureClient:         azure_client,
		config: &AzureConfig{
			ConnectionString: azure_conn_str,
			ContainerName:    azure_container_name,
		},
	}, nil
}

// closes the redis client
func (h *modelArtifactHandler) CloseRedisClient() error {
	h.redisJobQueueClient.Close()
	return h.redisClient.Close()
}

// downloads production model artifacts from Azure and stores them in Redis
// queries duckdb for models with status="production" via the DB Service
func (h *modelArtifactHandler) LoadProductionModels(ctx context.Context) error {
	log.Println("Starting to load production models via DB Service...")

	// 1. Construct the Query
	// We alias json_serialize to 'features_json' to easily access it in the map later
	query := `
		SELECT 
			model_id, 
			version, 
			CAST(trained_date AS VARCHAR) as trained_date, 
			status, 
			azure_location, 
			to_json(expected_features) as features_json
		FROM model_metadata 
		WHERE status = 'production'
	`

	// 2. Make Request via DB Client
	var rawResults []map[string]interface{}
	if err := h.redisJobQueueClient.Query(ctx, query, &rawResults); err != nil {
		return fmt.Errorf("failed to query db service: %w", err)
	}

	if len(rawResults) == 0 {
		log.Println("No production models found.")
		return nil
	}

	// 3. Process Results
	count := 0
	for _, rowMap := range rawResults {
		var m ModelMetadata

		// Map map[string]interface{} back to struct fields
		if v, ok := rowMap["model_id"].(string); ok {
			m.ModelID = v
		}
		if v, ok := rowMap["version"].(string); ok {
			m.Version = v
		}
		if v, ok := rowMap["trained_date"].(string); ok {
			m.TrainedDate = v
		}
		if v, ok := rowMap["status"].(string); ok {
			m.Status = v
		}
		if v, ok := rowMap["azure_location"].(string); ok {
			m.AzureLocation = v
		}

		var featuresJSON string
		if v, ok := rowMap["features_json"].(string); ok {
			featuresJSON = v
		}

		// Parse features JSON string
		if featuresJSON != "" {
			if err := json.Unmarshal([]byte(featuresJSON), &m.ExpectedFeatures); err != nil {
				log.Printf("Error unmarshaling features for model %s: %v", m.ModelID, err)
				continue
			}
		}

		// Download and Save
		if err := h.downloadAndSave(ctx, m); err != nil {
			log.Printf("Failed to load model %s: %v", m.ModelID, err)
		} else {
			count++
		}
	}

	log.Printf("Finished loading %d production models.", count)
	return nil
}

// internal helper to download a single model
// this should handle most file types by converting them to bytes
// it iterates through each blob looking for folders matching the model name, then for files ending in "production"
func (h *modelArtifactHandler) downloadAndSave(ctx context.Context, metadata ModelMetadata) error {
	log.Printf("Starting artifact load for model: %s", metadata.ModelID)

	// List blobs in the model's folder (ModelID)
	prefix := metadata.ModelID + "/"
	pager := h.azureClient.NewListBlobsFlatPager(h.config.ContainerName, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	foundArtifacts := 0

	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list blobs for model %s: %w", metadata.ModelID, err)
		}

		for _, blob := range resp.Segment.BlobItems {
			blobName := *blob.Name

			// Filter for files ending in "Production" (ignoring extension)
			ext := filepath.Ext(blobName)
			nameWithoutExt := strings.TrimSuffix(blobName, ext)

			// Check if it ends with "Production"
			if strings.HasSuffix(nameWithoutExt, "Production") {
				log.Printf("Found production artifact: %s", blobName)

				// Download
				downloadResponse, err := h.azureClient.DownloadStream(ctx, h.config.ContainerName, blobName, nil)
				if err != nil {
					log.Printf("Failed to download blob %s: %v", blobName, err)
					continue
				}

				data, err := io.ReadAll(downloadResponse.Body)
				downloadResponse.Body.Close()
				if err != nil {
					log.Printf("Failed to read blob body %s: %v", blobName, err)
					continue
				}

				// Store in Redis
				// Key: model:<ModelID>:<Extension>
				// Remove dot from extension for key
				cleanExt := strings.TrimPrefix(ext, ".")
				redisKey := fmt.Sprintf("model:%s:%s", metadata.ModelID, cleanExt)

				pipe := h.redisClient.Pipeline()
				pipe.Set(ctx, redisKey, data, 0)

				_, err = pipe.Exec(ctx)
				if err != nil {
					log.Printf("Failed to save %s to redis: %v", blobName, err)
					continue
				}

				log.Printf("Successfully loaded artifact %s into Redis as %s", blobName, redisKey)
				foundArtifacts++
			}
		}
	}

	if foundArtifacts == 0 {
		return fmt.Errorf("no production artifacts found for model %s", metadata.ModelID)
	}

	return nil
}

// Queries DuckDB for the expected features of a given model
func (h *modelArtifactHandler) GetModelFeatures(ctx context.Context, modelID string) ([]ModelFeature, string, error) {
	query := fmt.Sprintf(`
		SELECT version, to_json(expected_features) as features_json 
		FROM model_metadata 
		WHERE model_id = '%s'
	`, modelID)

	var rawResults []map[string]interface{}
	if err := h.redisJobQueueClient.Query(ctx, query, &rawResults); err != nil {
		return nil, "", fmt.Errorf("failed to query model metadata: %w", err)
	}

	if len(rawResults) == 0 {
		return nil, "", fmt.Errorf("no metadata found for model %s", modelID)
	}

	var version string
	if v, ok := rawResults[0]["version"].(string); ok {
		version = v
	}

	// Parse the JSON feature list
	var features []ModelFeature
	if featuresJSON, ok := rawResults[0]["features_json"].(string); ok && featuresJSON != "" {
		if err := json.Unmarshal([]byte(featuresJSON), &features); err != nil {
			return nil, "", fmt.Errorf("error unmarshaling features: %w", err)
		}
	} else {
		return nil, "", fmt.Errorf("metadata 'expected_features' is empty or invalid")
	}

	return features, version, nil
}

// Prompts the user to select a model from the available list
func PromptUserForModelSelection(reader *bufio.Reader, availableModels []string) (string, error) {
	fmt.Println("\n--- Available Models ---")
	for i, name := range availableModels {
		fmt.Printf("%d. %s\n", i+1, name)
	}
	fmt.Print("\nSelect a model (number): ")

	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	input = strings.TrimSpace(input)

	selection, err := strconv.Atoi(input)
	if err != nil || selection < 1 || selection > len(availableModels) {
		return "", fmt.Errorf("invalid selection")
	}

	return availableModels[selection-1], nil
}

// Prompts the user for inputs based on the model features
func PromptUserForInputs(reader *bufio.Reader, features []ModelFeature) (map[string]interface{}, error) {
	fmt.Println("\n--- Enter Model Inputs ---")
	userInputs := make(map[string]interface{})

	for _, feature := range features {
		fmt.Printf("Enter %s (Type: %s): ", feature.Name, feature.Type)
		valStr, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		valStr = strings.TrimSpace(valStr)

		// type conversion
		if feature.Type == "int" {
			val, err := strconv.Atoi(valStr)
			if err != nil {
				fmt.Printf("Invalid integer for %s. Using 0.\n", feature.Name)
				userInputs[feature.Name] = 0
			} else {
				userInputs[feature.Name] = val
			}
		} else if feature.Type == "float" || feature.Type == "double" {
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				fmt.Printf("Invalid float for %s. Using 0.0.\n", feature.Name)
				userInputs[feature.Name] = 0.0
			} else {
				userInputs[feature.Name] = val
			}
		} else {
			userInputs[feature.Name] = valStr
		}
	}

	return userInputs, nil
}

// attempts to locate the python executable and start the model server
func startPythonWorker() (*os.Process, error) {
	// use the venv python
	pythonPath := "venv/Scripts/python.exe"

	scriptPath := filepath.Join("Services", "Service_B", "model_server.py")
	log.Printf("Launching Python Worker: %s %s", pythonPath, scriptPath)

	cmd := exec.Command(pythonPath, scriptPath)

	// Direct output to stdout/stderr for debugging
	// Prefixing output would be nicer but requires more code; direct piping is simpler
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start python command: %w", err)
	}

	return cmd.Process, nil
}

func logModelPrediction(ctx context.Context, redisClient *redis.Client, modelID, version string, inputs map[string]interface{}, prediction float64, errorMsg string) {
	inputsBytes, _ := json.Marshal(inputs)

	req := Services.WriteRequest{
		Table: "model_logs",
		Data: map[string]interface{}{
			"model_id":       modelID,
			"model_version":  version,
			"model_inputs":   string(inputsBytes),
			"model_result":   fmt.Sprintf("%f", prediction),
			"model_error":    errorMsg,
			"event_datetime": time.Now(),
		},
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		log.Printf("Error marshaling log request: %v", err)
		return
	}

	if err := redisClient.RPush(ctx, Services.RedisWriteQueue, reqBytes).Err(); err != nil {
		log.Printf("Error logging to DuckDB: %v", err)
	}
}

// azure model artifacts were already handled by the system startup checks
// python worker has to start here. it's what we use to run the models
func Service_B_Start(redisAddr string, app_config_struct *config.App_Config) {
	// --- Start Python Worker ---
	log.Println("Starting Python Model Server...")
	pyProcess, err := startPythonWorker()
	if err != nil {
		log.Fatalf("Critical Error: Failed to start Python Model Server: %v", err)
	}

	// Ensure cleanup when this function exits (though it's an infinite loop, this handles crashes/returns)
	defer func() {
		log.Println("Stopping Python Worker...")
		if err := pyProcess.Kill(); err != nil {
			log.Printf("Failed to kill python worker: %v", err)
		}
	}()

	// Give the Python server a moment to bind to the port
	time.Sleep(2 * time.Second)
	// ---------------------------

	// get all connections
	// redis, azure, db clients/services
	azureModelArtifactHandler, err := CreateNewHandler(redisAddr)
	if err != nil {
		log.Fatalf("Failed to initialize modelArtifactHandler: %v", err)
	}
	defer azureModelArtifactHandler.redisClient.Close()

	// Connect to the Python gRPC Server
	// We use port 50052 for the Python worker
	conn, err := grpc.NewClient("localhost:50052", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to Python Worker: %v", err)
	}
	defer conn.Close()

	client := pb.NewPythonWorkerClient(conn)

	// Interactive Loop. a buffered reader listening to the terminal
	reader := bufio.NewReader(os.Stdin)

	for {
		// 1. Get Available Models from Redis
		loopCtx := context.Background()

		availableModels, err := azureModelArtifactHandler.redisClient.SMembers(loopCtx, "prod_models").Result()
		if err != nil {
			log.Printf("Error fetching models from Redis: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if len(availableModels) == 0 {
			fmt.Println("No production models found in Redis. Retrying in 5s...")
			time.Sleep(5 * time.Second)
			continue
		}

		// 2. Prompt User Selection
		selectedModelID, err := PromptUserForModelSelection(reader, availableModels)
		if err != nil {
			fmt.Println("Invalid selection. Please try again.")
			continue
		}
		fmt.Printf("Selected Model: %s\n", selectedModelID)

		// 3. Get Model Features
		features, modelVersion, err := azureModelArtifactHandler.GetModelFeatures(loopCtx, selectedModelID)
		if err != nil {
			log.Printf("Error getting features for model %s: %v", selectedModelID, err)
			continue
		}

		// 4. Prompt for Each Feature
		userInputs, err := PromptUserForInputs(reader, features)
		if err != nil {
			log.Printf("Error reading user inputs: %v", err)
			continue
		}

		// 5. Run Prediction
		fmt.Printf("\nSending request to model %s with inputs: %v\n", selectedModelID, userInputs)

		// Convert map[string]interface{} to map[string]string for gRPC
		grpcFeatures := make(map[string]string)
		for k, v := range userInputs {
			grpcFeatures[k] = fmt.Sprintf("%v", v) // Simple string conversion
		}

		// wait for the python worker response
		resp, err := client.RunInference(loopCtx, &pb.InferenceRequest{
			ModelName: selectedModelID,
			Features:  grpcFeatures,
		})

		if err != nil {
			log.Printf("gRPC Call Failed: %v", err)
			fmt.Println("Make sure the python model server is running.")
			time.Sleep(2 * time.Second)
			continue
		}

		if resp.ErrorMessage != "" {
			log.Printf("Model Error: %s", resp.ErrorMessage)
		} else {
			fmt.Printf("\n>>> Prediction Result: %f\n", resp.Prediction)
		}

		// Log to DuckDB via DB Client
		logModelPrediction(loopCtx, azureModelArtifactHandler.redisClient, selectedModelID, modelVersion, userInputs, resp.Prediction, resp.ErrorMessage)

		fmt.Println("\nPrediction cycle complete. Restarting...")
		time.Sleep(1 * time.Second)
	}
}
