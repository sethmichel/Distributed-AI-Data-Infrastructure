package service_b

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

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

// manages the model artifacts operations
type Handler struct {
	redisClient *redis.Client
	azureClient *azblob.Client
	config      *AzureConfig
}

// creates a new handler struct
func CreateNewHandler(redisAddr string) (*Handler, error) {
	// Load azure variables
	if err := godotenv.Load("Global_Configs/Env/Azure.env"); err != nil {
		log.Printf("ERROR: could not load .env file: %v", err)
	}

	connStr := os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	if connStr == "" {
		return nil, fmt.Errorf("AZURE_STORAGE_CONNECTION_STRING is not set")
	}

	containerName := os.Getenv("AZURE_CONTAINER_NAME")
	if containerName == "" {
		return nil, fmt.Errorf("AZURE_CONTAINER_NAME is not set")
	}

	// Create azure Client
	client, err := azblob.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create azure client: %w", err)
	}

	// Create redis Client
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	return &Handler{
		redisClient: rdb,
		azureClient: client,
		config: &AzureConfig{
			ConnectionString: connStr,
			ContainerName:    containerName,
		},
	}, nil
}

// LoadModelArtifacts downloads model artifacts from Azure and stores them in Redis
func (h *Handler) LoadModelArtifacts(ctx context.Context, metadata ModelMetadata) error {
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

// Example usage function (can be called from main)
func RunExample(redisAddr string) {
	// Example metadata from design doc
	metadata := ModelMetadata{
		ModelID:       "credit_risk_v1",
		Version:       "1.0.0",
		TrainedDate:   "2023-10-25",
		Status:        "production",
		AzureLocation: "ai-models",
		ExpectedFeatures: []ModelFeature{
			{Index: 0, Name: "debt_to_income_ratio", Type: "float"},
			{Index: 1, Name: "age_years", Type: "int"},
		},
	}

	handler, err := CreateNewHandler(redisAddr)
	if err != nil {
		log.Fatalf("Failed to initialize handler: %v", err)
	}
	defer handler.redisClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := handler.LoadModelArtifacts(ctx, metadata); err != nil {
		log.Printf("Error loading artifacts: %v", err)
	}
}
