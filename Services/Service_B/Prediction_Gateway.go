package service_b

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"ai_infra_project/Services"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
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
type modelArtifactHandler struct {
	dbClient    *Services.DBClient
	redisClient *redis.Client
	azureClient *azblob.Client
	config      *AzureConfig
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
	db_client, err := Services.NewDBClient(redisAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create db client: %w", err)
	}

	return &modelArtifactHandler{
		dbClient:    db_client,
		redisClient: redis_client,
		azureClient: azure_client,
		config: &AzureConfig{
			ConnectionString: azure_conn_str,
			ContainerName:    azure_container_name,
		},
	}, nil
}

// closes the redis client
func (h *modelArtifactHandler) CloseRedisClient() error {
	h.dbClient.Close()
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
			CAST(trainedDate AS VARCHAR) as trained_date, 
			status, 
			azure_location, 
			json_serialize(expected_features) as features_json
		FROM model_metadata 
		WHERE status = 'production'
	`

	// 2. Make Request via DB Client
	var rawResults []map[string]interface{}
	if err := h.dbClient.Query(ctx, query, &rawResults); err != nil {
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
func (h *modelArtifactHandler) downloadAndSave(ctx context.Context, metadata ModelMetadata) error {
	log.Printf("Starting artifact load for model: %s", metadata.ModelID)

	// Construct the blob path.
	// Based on "azure_location": "ai-models" and "model_id": "credit_risk_v1",
	// we assume the structure is {azure_location}/{model_id}.pkl or similar.
	// We'll look for a file that matches the model_id in that location.
	blobPath := fmt.Sprintf("%s/%s.pkl", metadata.AzureLocation, metadata.ModelID)

	log.Printf("Attempting to download blob: %s from container: %s", blobPath, h.config.ContainerName)

	// Download from Azure
	downloadResponse, err := h.azureClient.DownloadStream(ctx, h.config.ContainerName, blobPath, nil)
	if err != nil {
		return fmt.Errorf("failed to download blob %s: %w", blobPath, err)
	}
	defer downloadResponse.Body.Close()

	// Read content
	data, err := io.ReadAll(downloadResponse.Body)
	if err != nil {
		return fmt.Errorf("failed to read blob body: %w", err)
	}

	log.Printf("Downloaded %d bytes. Storing in Redis...", len(data))

	// Store in Redis
	// We store the binary artifact.
	// Key scheme: model:{model_id}:artifact
	redisKey := fmt.Sprintf("model:%s:artifact", metadata.ModelID)

	// We might also want to store the metadata itself
	metadataKey := fmt.Sprintf("model:%s:metadata", metadata.ModelID)
	metadataBytes, _ := json.Marshal(metadata)

	pipe := h.redisClient.Pipeline()
	pipe.Set(ctx, redisKey, data, 0) // 0 means no expiration
	pipe.Set(ctx, metadataKey, metadataBytes, 0)

	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to save to redis: %w", err)
	}

	log.Printf("Successfully loaded model %s into Redis", metadata.ModelID)
	return nil
}

func Service_B_Start(redisAddr string) {
	// get all connections
	azureModelArtifactHandler, err := CreateNewHandler(redisAddr)
	if err != nil {
		log.Fatalf("Failed to initialize modelArtifactHandler: %v", err)
	}
	defer azureModelArtifactHandler.redisClient.Close()

	// limit azure downloads to x minutes - prevents hanging
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// download prod model artifacts from azure
	if err := azureModelArtifactHandler.LoadProductionModels(ctx); err != nil {
		log.Printf("Error loading artifacts: %v", err)
	}
}
