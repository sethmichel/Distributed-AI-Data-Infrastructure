duckdb
ISSUE: It can only have 1 writer at a time, and multiple readers present possible file lock issues between themselves and the writer.
SOLUTION: all services use a database handler service for all read/writes to duckdb. each service sends requests to a job queue in redis that's batched until database service does the jobs
ISSUE: the write appender api made things too complex, I can barely read it. I should rewrite it to be clearer

# how to read/write to/from duckdb
since we have to use a database service to get around locking duckdb files
- NOTES ABOUT THIS DESIGN
	- this might bottleneck at scale, even for 1000 users. however I might have opimized it enough with spawning so many go routines for different jobs concurrently
	- relies on duckdb internal mvcc for thread safety rather than manually locking the db

**features table**: all data lives here. id's are entity_id, feature_name, and created_at. this means there's overlapping data points that are only different by created_at
CREATE TABLE IF NOT EXISTS features_table (
        entity_id TEXT,
        feature_name TEXT,
        value DOUBLE,
        event_timestamp TIMESTAMP,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );
**how it's used**
- service A writes all data and files here

---

**model logs**: note that model inputs are text. they can be all different types so we'll need to store it as text
CREATE TABLE IF NOT EXISTS model_logs (
	model_id TEXT,
	model_version TEXT,
	model_inputs: TEXT,
	model_result: TEXT,
	model_error: TEXT,
	event_datetime TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
**how it's used**
- service b: after we deliver the user the models result, we write the full interaction here as a log

---

**model metadata**: prod metadata ends up in redis after service b startup
CREATE TABLE IF NOT EXISTS model_metadata (
	model_id TEXT,        # ex: House_Price
	artifact_type TEXT,   # ex: pkl
	version TEXT,         # ex: 1.0.0
	trained_date DATE,    # ex: mm-dd-yyyy
	model_drift_score DOUBLE,
	status TEXT,          # either "production" or "deprecated"
	azure_location TEXT,  # this will be "Model_{model_id}/"
	expected_features STRUCT("index" INTEGER, "name" TEXT, "type" TEXT, "drift_score" DOUBLE)[],
	created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
**how it's used**
- service b 
	- get's notified by service d when a new model is ready and gets the new info from redis and pulls the azure artifacts
	- SO NOTE: PROD METADATA IS IN REDIS
- service c 
	- does the drift detection, but it's service D who writes the drift score to it
- service d 
 	- changes status from production to deprecated when a new model is ready
	- retrains models and then makes NEW entries for the new models
	- on startup, gets all entries with production status from duckdb into redis


# redis
**features**
service a
	- overwrites the latest value for each feature so other services can get it quickly
		- entity_id: feature_name
		- value is a json string: {"val": 123.45, "timestamp": "2024-01-01T12:00:00Z"}
		- read/write examples
		   // --- WRITE ---
			key := "user_123:click_count"
			data := map[string]interface{} {
				"val":       42.5,
				"timestamp": time.Now(),
			}
			jsonData, _ := json.Marshal(data)
			err := rdb.Set(ctx, key, jsonData, 0).Err()
			
			// --- READ ---
			val, err := rdb.Get(ctx, key).Result()
			var result RedisValue
    		json.Unmarshal([]byte(val), &result)

**duckdb read/write job queue**
- all services use this to push read/write requests. the result of read requests to sent to the queue also and read by services
- read/write examples
		- // WRITE
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
			- then the db service will see redis got a job pushed to it and on its next scheduled check it'll process it

		- // READ
		- see example below (example 1)

**model_id's of production models**
key: redisConn.SAdd(ctx, "prod_models", args...)

- service d deletes and creates them

**model_metadata**
key = "model_metadata:" + m.ModelID

service b
	- accesses this, does not write
service d
	- only prod models: writes it at startup, and replaces it on retraining models
	- on drift analysis, we need to update this when a model is done for all its features
		- we'll use service d so write access remains in 1 location. otherwise it would be in service c

**model artifacts**
key (go):
	cleanExt := strings.TrimPrefix(ext, ".")
    fmt.Sprintf("model:%s:%s", m.ModelID, cleanExt)

- service d 
	- creates and deletes them (from redis only)
- service b
	- reads them

---

# azure

service d
	- downloads the prod artifacts
	- on training a new model
		- changes existing artifact names to remove the production word
		- uploads the new prod artifacts and they include the word production in their name



**Example 1**
redis job queue read request
- we have to pause the calling service until we get the duckdb response
- the database service will write the read result to redis under the same key
- it will do reads concurrently, up to 50 via go routines for each request
- 1 shared file
	- issue: we need a central file because there's so much code for every read. we can't put all the read processing in 1 shared file because it will bloat into a huge file and get circular dependencies and wierd logic with who owns what structs and data
	- solution: 1 shared function that handles the redis/wait logic, accepts a target interface parameter, parses the db result directly into the calling services structs
		- services will do something like 
			var models []ModelMetadata
			err := DBServiceClient.Query(ctx, "SELECT ...", &models)
		- and the file will write the results into models 

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
	- we can get all models: models, err := rdb.SMembers(ctx, "prod_models").Result() // ([]string, error)
		- for _, modelName := range models { }

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
