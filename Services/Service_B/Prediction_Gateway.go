package service_b

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	config "ai_infra_project/Global_Configs"
	pb "ai_infra_project/Proto"
	"ai_infra_project/Services"

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

// Queries Redis for the expected features of a given model
func GetModelFeatures(ctx context.Context, redisClient *redis.Client, modelID string) ([]ModelFeature, string, error) {
	// The metadata is stored as a JSON string in Redis under "model_metadata:<ModelID>"
	redisKey := fmt.Sprintf("model_metadata:%s", modelID)

	metadataJSON, err := redisClient.Get(ctx, redisKey).Result()
	if err == redis.Nil {
		return nil, "", fmt.Errorf("no metadata found in Redis for model %s", modelID)
	} else if err != nil {
		return nil, "", fmt.Errorf("failed to get metadata from Redis: %w", err)
	}

	var metadata ModelMetadata
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal metadata for model %s: %w", modelID, err)
	}

	if len(metadata.ExpectedFeatures) == 0 {
		return nil, metadata.Version, fmt.Errorf("metadata 'expected_features' is empty")
	}

	return metadata.ExpectedFeatures, metadata.Version, nil
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
	// Check if we are in a container/linux environment
	pythonPath := "python3"

	// Fallback for local Windows dev if needed
	// Check for the existence of the venv python executable
	if _, err := os.Stat("venv/Scripts/python.exe"); err == nil {
		pythonPath = "venv/Scripts/python.exe"
	}

	scriptPath := filepath.Join("Services", "Service_B", "model_server.py")
	log.Printf("Launching Python Worker: %s %s", pythonPath, scriptPath)

	cmd := exec.Command(pythonPath, scriptPath)

	// Direct output to stdout/stderr for debugging
	// Prefixing output would be nicer but requires more code; direct piping is simpler
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// this is a background process. this go script will keep running as this python script is running
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

// python worker has to start here. it's what we use to run the models
func Service_B_Start(app_config_struct *config.App_Config) {
	redisAddr := app_config_struct.Connections.RedisAddr

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

	// Create Redis Client
	redisClient := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer redisClient.Close()

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

		availableModels, err := redisClient.SMembers(loopCtx, "prod_models").Result()
		if err != nil {
			log.Printf("Error fetching models from Redis: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if len(availableModels) == 0 {
			fmt.Println("No production models found in Redis. Service D must run first. Retrying in 5s...")
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
		features, modelVersion, err := GetModelFeatures(loopCtx, redisClient, selectedModelID)
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
		logModelPrediction(loopCtx, redisClient, selectedModelID, modelVersion, userInputs, resp.Prediction, resp.ErrorMessage)

		fmt.Println("\nPrediction cycle complete. Restarting...")
		time.Sleep(1 * time.Second)
	}
}
