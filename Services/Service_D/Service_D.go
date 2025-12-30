package service_d

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	config "ai_infra_project/Global_Configs"
	"ai_infra_project/Services"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/redis/go-redis/v9"
)

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

// Internal struct to match the flat structure returned by the DB service (where features is a JSON string)
type dbModelMetadata struct {
	ModelID       string `json:"model_id"`
	Version       string `json:"version"`
	TrainedDate   string `json:"trained_date"`
	Status        string `json:"status"`
	AzureLocation string `json:"azure_location"`
	FeaturesJSON  string `json:"features_json"`
}

func EstablishDBConnections(app_config *config.App_Config) (*Services.DBJobQueueClient, *redis.Client, *azblob.Client, context.Context, context.CancelFunc, error) {
	// make a timer. if the setup takes longer than x seconds, the context tells them to error out
	// cancel cleans resources used by the timer
	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)

	// redis connection
	redisAddr := app_config.Connections.RedisAddr
	redisConn := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	// DB Job Queue Client (for talking to DB Service)
	dbClient, err := Services.NewDBJobQueueClient(redisAddr)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("service d failed to create db job queue client: %w", err)
	}

	// azure connection
	azureConn, err := azblob.NewClientFromConnectionString(app_config.Connections.AzureConn, nil)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("failed to create azure client: %w", err)
	}

	return dbClient, redisConn, azureConn, ctx, ctxCancel, nil
}

// Query DuckDB via DB Service and return production model metadata
func GetProductionModelMetadata(ctx context.Context, dbClient *Services.DBJobQueueClient) ([]ModelMetadata, error) {
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

	var dbModels []dbModelMetadata
	if err := dbClient.Query(ctx, query, &dbModels); err != nil {
		return nil, fmt.Errorf("failed to query model_metadata via db service: %w", err)
	}

	var models []ModelMetadata
	for _, dbM := range dbModels {
		m := ModelMetadata{
			ModelID:       dbM.ModelID,
			Version:       dbM.Version,
			TrainedDate:   dbM.TrainedDate,
			Status:        dbM.Status,
			AzureLocation: dbM.AzureLocation,
		}

		if dbM.FeaturesJSON != "" {
			if err := json.Unmarshal([]byte(dbM.FeaturesJSON), &m.ExpectedFeatures); err != nil {
				log.Printf("Service D: Warning - failed to unmarshal features for model %s: %v", m.ModelID, err)
			}
		}
		models = append(models, m)
	}

	if len(models) == 0 {
		log.Println("Service D: No production models found in DuckDB.")
	} else {
		log.Printf("Service D: Found %d production models in DuckDB.", len(models))
	}

	return models, nil
}

// Save model metadata to Redis. models is the full model metadata
func SaveModelMetadataToRedis(redisConn *redis.Client, ctx context.Context, models []ModelMetadata) error {
	for _, m := range models {
		metadataJSON, err := json.Marshal(m)
		if err != nil {
			log.Printf("Service D: Warning - failed to marshal metadata for model %s: %v", m.ModelID, err)
			continue
		}

		redisKey := "model_metadata:" + m.ModelID
		if err := redisConn.Set(ctx, redisKey, metadataJSON, 0).Err(); err != nil {
			log.Printf("Service D: Warning - failed to save metadata to redis for model %s: %v", m.ModelID, err)
		}
	}
	log.Printf("Service D: Processed metadata saving for %d models.", len(models))

	return nil
}

// Save model names (IDs) to the 'prod_models' set in Redis
// models is the full models metadata
func SaveModelNamesToRedis(redisConn *redis.Client, ctx context.Context, models []ModelMetadata) error {
	// Clear existing 'prod_models' key to ensure we don't have stale models
	if err := redisConn.Del(ctx, "prod_models").Err(); err != nil {
		log.Printf("Service D: Warning - failed to clear prod_models from redis: %v", err)
	}

	if len(models) == 0 {
		log.Println("Service D: 'prod_models' in Redis cleared (no production models found).")
		return nil
	}

	var modelIDs []interface{}
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelID)
	}

	if err := redisConn.SAdd(ctx, "prod_models", modelIDs...).Err(); err != nil {
		return fmt.Errorf("failed to add models to redis set: %w", err)
	}

	log.Printf("Service D: Successfully updated 'prod_models' in Redis with %d models.", len(models))

	return nil
}

// download model artifacts from Azure and saves them to Redis
// models = the production model metadata
func LoadAzureArtifactsToRedis(app_config *config.App_Config, azureConn *azblob.Client, redisConn *redis.Client, ctx context.Context, models []ModelMetadata) error {
	log.Println("Service D: Starting to load Azure artifacts...")

	for _, m := range models {
		log.Printf("Service D: Checking artifacts for model %s at %s", m.ModelID, m.AzureLocation)

		prefix := m.AzureLocation
		pager := azureConn.NewListBlobsFlatPager(app_config.Connections.AzureContainerName, &azblob.ListBlobsFlatOptions{
			Prefix: &prefix,
		})

		foundArtifacts := 0

		for pager.More() {
			resp, err := pager.NextPage(ctx)
			if err != nil {
				log.Printf("Service D: Warning - failed to list blobs for model %s: %v", m.ModelID, err)
				break
			}

			for _, blob := range resp.Segment.BlobItems {
				blobName := *blob.Name

				// Filter for files ending in "Production" (ignoring extension)
				ext := filepath.Ext(blobName)
				nameWithoutExt := strings.TrimSuffix(blobName, ext)

				if strings.HasSuffix(nameWithoutExt, "Production") {
					log.Printf("Service D: Found production artifact: %s", blobName)

					// Download
					downloadResponse, err := azureConn.DownloadStream(ctx, app_config.Connections.AzureContainerName, blobName, nil)
					if err != nil {
						log.Printf("Service D: Failed to download blob %s: %v", blobName, err)
						continue
					}

					data, err := io.ReadAll(downloadResponse.Body)
					downloadResponse.Body.Close() // very important
					if err != nil {
						log.Printf("Service D: Failed to read blob body %s: %v", blobName, err)
						continue
					}

					// Store in Redis
					cleanExt := strings.TrimPrefix(ext, ".")
					redisKey := fmt.Sprintf("model:%s:%s", m.ModelID, cleanExt)

					if err := redisConn.Set(ctx, redisKey, data, 0).Err(); err != nil {
						log.Printf("Service D: Failed to save %s to redis: %v", blobName, err)
						continue
					}

					log.Printf("Service D: Successfully loaded artifact %s into Redis as %s", blobName, redisKey)
					foundArtifacts++
				}
			}
		}

		if foundArtifacts == 0 {
			log.Printf("Service D: Warning - no production artifacts found for model %s", m.ModelID)
		}
	}

	return nil
}

// Service_D_Start loads production model information from DuckDB into Redis at startup.
// It queries the model_metadata table for models with status = 'production' and populates the 'prod_models' set in Redis.
func Service_D_Start(app_config *config.App_Config) error {
	log.Println("Service D: Starting up to load production models metadata/names, and azure artifacts into Redis...")

	// 1. get all db connections
	dbClient, redisConn, azureConn, ctx, ctxCancel, err := EstablishDBConnections(app_config)
	if err != nil {
		return fmt.Errorf("service d failed to establish all database connections: %w", err)
	}
	defer redisConn.Close()
	defer dbClient.Close()
	defer ctxCancel()
	// azure doesn't need a defer because azure is foundationally different than a normal db

	// 2. Query for production models & add their metadata and names to redis
	// get just the metadata
	models, err := GetProductionModelMetadata(ctx, dbClient)
	if err != nil {
		return fmt.Errorf("service d failed to get production models from duckdb via db service: %w", err)
	}

	// save the metadata to redis
	if err := SaveModelMetadataToRedis(redisConn, ctx, models); err != nil {
		return fmt.Errorf("service d failed to save model metadata to redis: %w", err)
	}

	// save the model names to redis
	if err := SaveModelNamesToRedis(redisConn, ctx, models); err != nil {
		return fmt.Errorf("service d failed to save model names to redis: %w", err)
	}

	// 3. load azure production model artifacts into redis
	if err := LoadAzureArtifactsToRedis(app_config, azureConn, redisConn, ctx, models); err != nil {
		return fmt.Errorf("service d failed to load azure artifacts: %w", err)
	}

	return nil
}
