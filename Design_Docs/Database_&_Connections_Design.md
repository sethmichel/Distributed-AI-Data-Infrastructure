### Duckdb
ISSUE: It can only have 1 writer at a time, and multiple readers present possible file lock issues between themselves and the writer.
SOLUTION: all services use a database handler service for all read/writes to duckdb. each service sends requests to a job queue in redis that's batched until database service does the jobs
ISSUE: the write appender api made things too complex, I can barely read it. I should rewrite it to be clearer

service A sends this info to kafka & it reads the files into this format and sends to gRPC
feature storage schema (use appender api)
    CREATE TABLE IF NOT EXISTS features (
        entity_id TEXT,
        feature_name TEXT,
        value DOUBLE,
        event_timestamp TIMESTAMP,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );

model metadata
metadata is searched for at service b startup (not app startup), service b looks for the metadata of prod models and downloads them from azure into redis
example:
	metadata := ModelMetadata{
		ModelID:       "credit_risk_v1",
		ArtifactType   "scaler",
		Version:       "1.0.0",
		TrainedDate:   "2023-10-25",
		Status:        "production",
		AzureLocation: "ai-models",
		ExpectedFeatures: []ModelFeature{
			{Index: 0, Name: "debt_to_income_ratio", Type: "float"},
			{Index: 1, Name: "age_years", Type: "int"},
		},
	}

# how to read/write to/from duckdb
since we have to use a database service to get around locking duckdb files
- NOTES ABOUT THIS DESIGN
	- this might bottleneck at scale, even for 1000 users. however I might have opimized it enough with spawning so many go routines for different jobs concurrently
	- relies on duckdb internal mvcc for thread safety rather than manually locking the db

- bug: abstract the read logic into a shared file

**write:**
- appoximatly this.
		pipe := redis_conn.Pipeline()
		for _, data := range batchData {
			// make write request
			req := Services.WriteRequest{
				Table: "features",
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
- then the db service will see redis got a job pushed to it and on its next scheduled check it'll process it

**reads**
- we have to pause the calling service until we get the duckdb response
- the database service will write the read result to redis under the same key
- it will do reads concurrently, up to 50 via go routines for each request
- 1 shared file
	- issue: we need a central file because there's so much code for every read. we can't put all the read processing in 1 shared file because it will bloat into a huge file and get circular dependencies and wierd logic with who owns what structs and data
	- solution: 1 shared function that handles the redis/wait logic, accepts a target interface parameter, parses the db result directly into the calling services structs
		- services will do something like 
			var models []ModelMetadata
			err := dbClient.Query(ctx, "SELECT ...", &models)
		- and the file will write the results into models

model artifacts
	- artifacts are saved under the key: model:{model_id}:artifact

- approximetly this
	1. generate a key
	2. make the query
	3. send request
	4. block itself while waiting for a response (BLpop). it'll return [queue_name, value]
	5. parse response, deserialize it, check for database errors. save to an array for all the different results returned

	// 1. generate a key
	responseKey := fmt.Sprintf("service_b:model_load:%d", time.Now().UnixNano())

	// 2. Construct the Query
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

	// 3. Create and Send Read Request
	req := Services.ReadRequest{
		Query:       query,
		ResponseKey: responseKey,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal read request: %w", err)
	}
	if err := h.redisClient.RPush(ctx, Services.RedisReadQueue, reqBytes).Err(); err != nil {
		return fmt.Errorf("failed to push read request to redis: %w", err)
	}

	// 4. Wait for Response (Blocking)
	// BLPop returns [key, value]
	log.Printf("Waiting for DB response on key: %s", responseKey)
	result, err := h.redisClient.BLPop(ctx, 30*time.Second, responseKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get response from db service (timeout or error): %w", err)
	}

	// 5. Parse Response
	// BLPop returns [queue_name, value]
	if len(result) < 2 {
		return fmt.Errorf("invalid response from redis")
	}
	payload := result[1]

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return fmt.Errorf("failed to unmarshal db response: %w", err)
	}

	// Check for application-level DB error
	if errMsg, ok := resp["error"]; ok && errMsg != nil {
		return fmt.Errorf("db service returned error: %v", errMsg)
	}

	// Extract Data Rows
	rows, ok := resp["data"].([]interface{})
	if !ok || rows == nil {
		log.Println("No production models found.")
		return nil
	}

	// next you map the results to their required fields




### Redis
- hosts the production model artifacts in bytes. where extention is the file extension
	- keys: model:<ModelID>:<Extension>

- hosts a basic list of which models we have downloaded. services use this to know what the user can be served
- it's done in a redis set (sadd) for efficient lookups (o1)
	- we can check for a model with: SISMEMBER prod_models "Loan_Approve"

- hosts the job queue for duckdb read/writes. 
	- key: duckdb_write_queue, key: duckdb_read_queue



### Azure
- Each model has its own folder containing all artifacts for that model
- prod artifacts for each model have "production" at the end of their name


### kafka
Use a function to start the kafka broker at app start if it's not already up. note that it might be ipv4 or ipv6

Stats: 1 topic, 1 partition, no replicas


### gRPC


### Promethius



### Granfana
