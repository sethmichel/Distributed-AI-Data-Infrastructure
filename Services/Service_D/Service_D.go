package service_d

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	config "ai_infra_project/Global_Configs"
	"ai_infra_project/Services"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "ai_infra_project/Proto"
)

type ModelFeature struct {
	Index      int     `json:"index"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	DriftScore float64 `json:"drift_score"`
}

type ModelMetadata struct {
	ModelID          string         `json:"model_id"`
	ArtifactType     string         `json:"artifact_type"`
	Version          string         `json:"version"`
	TrainedDate      string         `json:"trained_date"`
	ModelDriftScore  float64        `json:"model_drift_score"`
	Status           string         `json:"status"`
	AzureLocation    string         `json:"azure_location"`
	ExpectedFeatures []ModelFeature `json:"expected_features"`
}

// Internal struct to match the flat structure returned by the DB service (where features is a JSON string)
type dbModelMetadata struct {
	ModelID         string  `json:"model_id"`
	ArtifactType    string  `json:"artifact_type"`
	Version         string  `json:"version"`
	TrainedDate     string  `json:"trained_date"`
	ModelDriftScore float64 `json:"model_drift_score"`
	Status          string  `json:"status"`
	AzureLocation   string  `json:"azure_location"`
	FeaturesJSON    string  `json:"features_json"`
}

type ModelOutputInfo struct {
	Type             string `json:"type"`
	Description      string `json:"description"`
	IsClassification bool   `json:"is_classification"`
	Classes          []int  `json:"classes,omitempty"`
}

type PythonScriptOutput struct {
	Artifacts map[string]string          `json:"artifacts"`
	Features  map[string][]ModelFeature  `json:"features"`
	Outputs   map[string]ModelOutputInfo `json:"outputs"`
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
			artifact_type,
			version, 
			CAST(trained_date AS VARCHAR) as trained_date, 
			model_drift_score,
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
			ModelID:         dbM.ModelID,
			ArtifactType:    dbM.ArtifactType,
			Version:         dbM.Version,
			TrainedDate:     dbM.TrainedDate,
			ModelDriftScore: dbM.ModelDriftScore,
			Status:          dbM.Status,
			AzureLocation:   dbM.AzureLocation,
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

// calls Service C to run drift analysis and processes the results.
func TriggerDriftDetection(ctx context.Context, serviceCAddr string) error {
	// 1. Connect to Service C
	log.Printf("Service D: Connecting to Service C at %s...", serviceCAddr)
	conn, err := grpc.NewClient(serviceCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("did not connect to service c: %v", err)
	}
	defer conn.Close()

	client := pb.NewPythonWorkerClient(conn)

	// 2. Call CalculateDrift
	log.Println("Service D: Requesting drift analysis...")
	req := &pb.DriftRequest{} // Empty request implies "analyze all"
	resp, err := client.CalculateDrift(ctx, req)
	if err != nil {
		return fmt.Errorf("RPC failed: %v", err)
	}

	// 3. Process Results
	log.Printf("Service D: Received drift analysis results for %d models", len(resp.ModelResults))

	for _, modelResult := range resp.ModelResults {
		log.Printf("  Model %s: Drift Detected? %v", modelResult.ModelId, modelResult.DriftDetected)

		for _, metric := range modelResult.FeatureMetrics {
			// Log the status
			log.Printf("    Feature '%s': Status=%s, PSI=%.4f, KS_Stat=%.4f, KS_P=%.4f",
				metric.FeatureName, metric.Status, metric.Psi, metric.KsStat, metric.KsPValue)

			// Action logic based on tags
			if metric.Status == "CRITICAL" {
				log.Printf("      !!! CRITICAL ALERT: Feature %s has severe drift! Triggering retraining workflow...", metric.FeatureName)
				// TODO: Queue retraining job here
			} else if metric.Status == "WARNING" {
				log.Printf("      ! WARNING: Feature %s is showing signs of drift.", metric.FeatureName)
			}
		}
	}

	return nil
}

// for model metadata
func incrementVersion(version string) (string, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("failed to update model version. version is the wrong format: %v", version)
	}

	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	patch, _ := strconv.Atoi(parts[2])

	// "iterate the version up by 0.0.1"
	patch++

	return fmt.Sprintf("%d.%d.%d", major, minor, patch), nil
}

// creates metadata for newly trained models.
// It checks against oldModels to decide if it's a retrain (increment version) or a new model (v0.1.0).
func PrepareNewModelMetadata(oldModels []ModelMetadata, newModelFeatures map[string][]ModelFeature) ([]ModelMetadata, error) {
	// Create a map for quick lookup of old models
	oldModelMap := make(map[string]ModelMetadata)
	for _, m := range oldModels {
		oldModelMap[m.ModelID] = m
	}

	var newMetadataList []ModelMetadata
	currentDate := time.Now().Format("2006-01-02") // format: yyyy-mm-dd

	for modelID, features := range newModelFeatures {
		if oldMetadata, exists := oldModelMap[modelID]; exists {
			// RETRAIN situation
			// "copy that data... iterate the version up by 0.0.1"
			newMeta := oldMetadata
			var err error
			newMeta.Version, err = incrementVersion(oldMetadata.Version)
			if err != nil {
				return nil, fmt.Errorf("failed to increment version for model %s: %w", modelID, err)
			}
			newMeta.TrainedDate = currentDate
			newMeta.ModelDriftScore = 0 // Reset drift score
			newMeta.Status = "production"

			// Reset drift score for features
			// We use the NEW features from the training script just in case they changed,
			// but usually for a retrain they should be the same structure.
			// The script output features have drift_score=0 by default (float zero value).
			newMeta.ExpectedFeatures = features
			for i := range newMeta.ExpectedFeatures {
				newMeta.ExpectedFeatures[i].DriftScore = 0
			}

			newMetadataList = append(newMetadataList, newMeta)
			log.Printf("Service D: Prepared metadata for RETRAINED model %s (v%s)", modelID, newMeta.Version)

		} else {
			// NEW MODEL situation
			// "create the metadata ourself... version is 0.1.0"
			newMeta := ModelMetadata{
				ModelID:          modelID,
				ArtifactType:     "pkl",
				Version:          "0.1.0",
				TrainedDate:      currentDate,
				ModelDriftScore:  0,
				Status:           "production",
				AzureLocation:    fmt.Sprintf("Model_%s/", modelID),
				ExpectedFeatures: features,
			}
			// Ensure features drift score is 0
			for i := range newMeta.ExpectedFeatures {
				newMeta.ExpectedFeatures[i].DriftScore = 0
			}

			newMetadataList = append(newMetadataList, newMeta)
			log.Printf("Service D: Prepared metadata for NEW model %s (v%s)", modelID, newMeta.Version)
		}
	}

	return newMetadataList, nil
}

// retrains models, backs up files locally (for debugging), deprecates old azure artifacts, uploads new artifacts to azure
// the backups are really just debugging. this deletes them and saves the new ones there
func RetrainAndUploadModels(ctx context.Context, app_config *config.App_Config, azureConn *azblob.Client, modelsMetadata []ModelMetadata) error {
	// 1. Run the Python retraining script
	log.Println("Service D: Starting model retraining task...")

	pythonCmd := "python"   // for running python3 in k3 vs my local python for local developement
	if os.Getenv("RUN_IN_K3S") == "true" {
		pythonCmd = "python3"
	}

	cmd := exec.Command(pythonCmd, "Services/Service_D/Random_Generated_Models.py")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("failed to run python script: %v, stderr: %s", err, string(exitErr.Stderr))
		}
		return fmt.Errorf("failed to run python script: %w", err)
	}

	var scriptOutput PythonScriptOutput
	if err := json.Unmarshal(output, &scriptOutput); err != nil {
		return fmt.Errorf("failed to parse python script output: %w", err)
	}

	// Log that we received features and outputs
	log.Printf("Service D: Received artifacts for %d models.", len(scriptOutput.Artifacts))
	for modelID, features := range scriptOutput.Features {
		log.Printf("  Model %s has %d features defined.", modelID, len(features))
	}
	for modelID, outputInfo := range scriptOutput.Outputs {
		log.Printf("  Model %s output info: Type=%s, IsClass=%v", modelID, outputInfo.Type, outputInfo.IsClassification)
	}

	modelData := make(map[string][]byte)
	for k, v := range scriptOutput.Artifacts {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return fmt.Errorf("failed to decode base64 for model %s: %w", k, err)
		}
		modelData[k] = decoded
	}

	// these are already in useable format
	modelFeatures := scriptOutput.Features
	modelOutputs := scriptOutput.Outputs

	log.Printf("Service D: Extracted %d feature sets and %d output definitions.", len(modelFeatures), len(modelOutputs))
	log.Println("Service D: Python script executed successfully and returned artifacts.")

	// 2. Handle Local Backups
	if err := HandleLocalBackups(modelData); err != nil {
		return err
	}

	// 3. Azure Operations
	if err := ManageAzureArtifacts(ctx, app_config, azureConn, modelData); err != nil {
		return err
	}

	// 4. Extract and Prepare Metadata for DuckDB
	log.Println("Service D: Step 4 - Preparing new model metadata...")
	newModelsMetadata, err := PrepareNewModelMetadata(modelsMetadata, modelFeatures)
	if err != nil {
		return fmt.Errorf("failed to prepare new model metadata: %w", err)
	}
	log.Printf("Service D: Prepared metadata for %d models. Ready for DB save (next step).", len(newModelsMetadata))

	return nil
}

func HandleLocalBackups(modelData map[string][]byte) error {
	backupDir := "Services/Service_D/Ml_Models_Backup"

	// 1. Clear existing backups
	files, err := os.ReadDir(backupDir)
	if err != nil {
		return fmt.Errorf("failed to read backup directory %s: %w", backupDir, err)
	}

	for _, f := range files {
		// Skip directories or strict file matching if desired, but "delete all files" was requested
		if !f.IsDir() {
			path := filepath.Join(backupDir, f.Name())
			if err := os.Remove(path); err != nil {
				log.Printf("Service D: Warning - failed to delete old backup %s: %v", path, err)
			}
		}
	}

	// 2. Save new files
	for modelName, data := range modelData {
		// create paths
		modelBaseName := fmt.Sprintf("Model_%s", modelName)
		newLocalBackupFileName := fmt.Sprintf("%s.pkl", modelBaseName)
		newLocalBackupPath := filepath.Join(backupDir, newLocalBackupFileName)

		// Save to Backup Directory
		if err := os.WriteFile(newLocalBackupPath, data, 0644); err != nil {
			log.Printf("Service D: Warning - could not save to backup %s: %v", newLocalBackupPath, err)
		} else {
			log.Printf("Service D: Saved backup to %s", newLocalBackupPath)
		}
	}

	return nil
}

/*
This is like this because of how bad I designed the azure system. That was early on and it's on my list of changes to do
- The function lists all blobs within the Model_{modelName}/ directory prefix
- It iterates through these blobs and identifies any that contain _Production in their name.
- For each found "Production" file, it downloads the content.
- Generates a new archive name by replacing _Production with _{RandomNumber} (marks it as depreceated as far as this program is concerned)
- Uploads the content to the new archive name.
- Deletes the old "Production" blob.
- Finally, it uploads the new model artifact with the standard Model_{modelName}_Production.pkl name.
*/
func ManageAzureArtifacts(ctx context.Context, app_config *config.App_Config, azureConn *azblob.Client, modelData map[string][]byte) error {
	azureContainerName := app_config.Connections.AzureContainerName

	for modelName, data := range modelData {
		modelBaseName := fmt.Sprintf("Model_%s", modelName)
		azureFolder := modelBaseName // ex) Model_House_Price

		// The requirement is to rename ANY file ending in "Production.pkl" (or similar) to have a random number instead.
		// Then upload the new one as the definitive "Production".

		// 1. Scan for existing Production files
		prefix := azureFolder + "/"
		pager := azureConn.NewListBlobsFlatPager(azureContainerName, &azblob.ListBlobsFlatOptions{
			Prefix: &prefix,
		})

		for pager.More() {
			resp, err := pager.NextPage(ctx)
			if err != nil {
				log.Printf("Service D: Warning - Error listing blobs to check for existing production model: %v", err)
				break
			}

			for _, blob := range resp.Segment.BlobItems {
				blobName := *blob.Name

				// Check if this file is a "Production" file
				// We look for "_Production" in the name.
				if strings.Contains(blobName, "_Production") {
					log.Printf("Service D: Found existing production model %s. Archiving...", blobName)

					// Download old data
					getResp, err := azureConn.DownloadStream(ctx, azureContainerName, blobName, nil)
					if err != nil {
						log.Printf("Service D: Warning - Failed to download old production model for archiving: %v", err)
						continue
					}

					oldData, err := io.ReadAll(getResp.Body)
					getResp.Body.Close()

					if err != nil {
						log.Printf("Service D: Warning - Failed to read old production model body: %v", err)
						continue
					}

					// Create Archive Name: Replace "_Production" with "_{RandomNumber}"
					randNum := rand.Intn(1000000)
					archiveBlobName := strings.Replace(blobName, "_Production", fmt.Sprintf("_%d", randNum), 1)

					log.Printf("Service D: Archiving to %s", archiveBlobName)

					// Upload to Archive
					_, err = azureConn.UploadBuffer(ctx, azureContainerName, archiveBlobName, oldData, nil)
					if err != nil {
						log.Printf("Service D: Warning - Failed to upload archive blob: %v", err)
					} else {
						// Delete old production blob
						_, err = azureConn.DeleteBlob(ctx, azureContainerName, blobName, nil)
						if err != nil {
							log.Printf("Service D: Warning - Failed to delete old production blob after archiving: %v", err)
						} else {
							log.Printf("Service D: Successfully archived and deleted old production blob.")
						}
					}
				}
			}
		}

		// 2. Upload NEW model as Production
		prodFileName := fmt.Sprintf("%s_Production.pkl", modelBaseName)
		prodBlobPath := fmt.Sprintf("%s/%s", azureFolder, prodFileName)

		log.Printf("Service D: Uploading new production model to %s", prodBlobPath)
		_, err := azureConn.UploadBuffer(ctx, azureContainerName, prodBlobPath, data, nil)
		if err != nil {
			log.Printf("Service D: Failed to upload new production model %s: %v", prodBlobPath, err)
		} else {
			log.Printf("Service D: Successfully uploaded %s", prodBlobPath)
		}
	}

	return nil
}

// StartScheduler initializes and runs background scheduled tasks
func StartScheduler(app_config *config.App_Config, azureConn *azblob.Client, models []ModelMetadata) {
	go func() {
		// Task 1: Drift Detection (e.g., every 1 hour)
		featureDriftTicker := time.NewTicker(1 * time.Hour)

		// Task 2: Model Retraining (e.g., every 1 hour)
		modelRetrainTicker := time.NewTicker(1 * time.Hour)

		// Task 3: Placeholder (e.g., every 1 hour)
		task3Ticker := time.NewTicker(1 * time.Hour)

		defer featureDriftTicker.Stop()
		defer modelRetrainTicker.Stop()
		defer task3Ticker.Stop()

		log.Println("Service D: Scheduler started with 3 tasks.")

		// Service C address
		serviceCAddr := "localhost:50053"

		// CRITICAL - the context timers are for the action to complete, not startup.
		// for example, if drift detection isn't done by the timer it will error out
		for {
			select {
			case <-featureDriftTicker.C:
				log.Println("Service D: Scheduler triggering drift detection...")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // timeout context
				if err := TriggerDriftDetection(ctx, serviceCAddr); err != nil {
					log.Printf("Service D: Error triggering drift detection: %v", err)
				}
				cancel()

			case <-modelRetrainTicker.C:
				log.Println("Service D: Scheduler triggering task 2 (Model Retraining)...")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				if err := RetrainAndUploadModels(ctx, app_config, azureConn, models); err != nil {
					log.Printf("Service D: Error during model retraining task: %v", err)
				}
				cancel()

			case <-task3Ticker.C:
				log.Println("Service D: Scheduler triggering task 3 (placeholder)...")
				// TODO: Implement task 3 logic
			}
		}
	}()
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

	// 4. Start the scheduler for background tasks
	StartScheduler(app_config, azureConn, models)

	return nil
}
