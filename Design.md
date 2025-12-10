**Overview**

Note: the target scale of this is small: it's a personal/learning project

A Distributed AI Data Platform (Feature Store + Model Serving + Drift Monitoring + Batch/Streaming Pipelines)

This is the infrastructure to support scalable ml models

Service A: Distributed Feature Store
- Ingests streaming + batch data
- Computes features with a DAG engine (Transform Orchestrator)
    - This means a feature computation pipeline. It’s a dag so a feature can depend on other features and can recompute past states (backfill)
- Keeps point-in-time–correct versions
    - It means when training a model we can’t leak future data into the past. If we request a 12:10 feature we need that data exactly as it was at 12:10
- Stores in an online KV store + offline warehouse
    - Kv store is key value db
- Supports backfills, lineage tracking, metadata
    - Lineage means how a feature was computed, what it depended on. It’s ancestry
    - Backfills mean recompute historical features after a bug fix, new data source...


Service B: Model Serving Platform
- Hosts multiple ML models (XGBoost, LightGBM, transformers, whatever)
- Autoscaling workers
- gRPC or REST inference API
- Model versioning + canary deployments
- p95 latency tracking


Service C: Drift + Data Quality Monitoring
- Monitors incoming distributions vs historical distributions
- Detects feature drift, label drift, performance drop
- Supports alerting
- Generates dashboards


Service D: Batch Pipeline Orchestrator
- Runs ETL/ELT pipelines
- Handles daily training jobs
- Distributed workers for heavy compute
- Checkpoints, retries, idempotent job execution

This is a scaled-down version of:
- Uber Michelangelo
- Netflix Metaflow + Titus
- Databricks Feature Store + MLflow + Model Serving
- AWS Sagemaker Pipelines


Skills
Go, Python, gRPC, Docker, Redis, Kafka/NATS, Postgres, DuckDB, Prometheus, Grafana, distributed systems, autoscaling, load balancing, streaming data pipelines, online/offline feature stores, model serving, drift detection, batch scheduling, consistency guarantees, microservices architecture, container orchestration, concurrency.

What does it solve
“It solves ML reliability. It guarantees that models always use fresh, correct, and consistent data; are monitored for drift; can be deployed safely; and can scale horizontally. In short: it makes machine learning production-ready.”
-------------------------------------------------

**breakdown of service A (Feature Store)**

- Ingests streaming + batch data
- Computes features with a DAG engine (Transform Orchestrator/controller)
- Keeps point-in-time–correct versions
- Stores in an online KV store + offline warehouse
- Supports backfills, lineage tracking, metadata

**Core Philosophy:**
- **Go** for the "Plumbing": Ingestion, API Serving, Orchestration, State Management.
- **Python** for the "Logic": Complex feature transformations (pandas/numpy support).
- **DuckDB** for the "History": Efficient analytical queries on a single node. (column db)
- **Redis** for the "Now": Ultra-fast online retrieval.

### 1. Core Components

*   **Ingestion Gateway (Go)**
summary
inputs: raw data and raw file uploads
outputs: valid model features saved in redis and duckdb

- A data generator file spawning 2+ go routines to generate fake user info in json ({ "user_id": 123, "action": "click", "timestamp": ... }) and streamed over kafka. It should also have 1 go routine which calls the gRPC api every 10 seconds. sends data to transformation engine in batches

- a gRPC api script to handle file uploads. kafka doesn't deal with this. it sends data to the transformation engine

- Both sends that data (likely in batches for kafka) to the transformation engine which transforms the data via python workers working in parallel. for file's it'll split the file up and do the same. now the data is a "feature"
    - issue: calling python from go for each event is slow. use option 2
        option 1) We will use a long-running Python gRPC server. Go sends a *batch* of events to Python to reduce network overhead
        option 2) or we accept the overhead for simplicity since scale is small.

- any critical data is sent to redis, and all data is sent to duckdb after that. use duckdb appender for kafka data, and use something like "insert into features select * from 'upload.parquet'" for files.
    - issue: Ensuring we don't leak future data.
        - Solution: Every feature write MUST have an `event_timestamp`.
            - DuckDB Query Strategy: `AS OF` joins are powerful here. We will store an append-only log of feature values in DuckDB: `(entity_id, feature_name, value, valid_at, created_at)`.

    kafka + gRPC -> transformation engine -> redis then to duckdb

- to clarify, kafka will batch it and send it here as 1 batch. so this engine will be turned off while there are no files and when kafka is still gathering data

Issues and imporvements
- issue, concurrency/rapid updates
    - *challenge*: if user updates profile twice in 1ms, processing order matters.
    - *solution*: I can do this though: Partition Kafka topics by `entity_id`. This ensures all events for "User A" go to the same worker sequentially.

- ignore this, I'll see how bad it actually is later: Issue, Infrastructure Complexity
    - *Challenge*: Running Kafka + Redis + DuckDB + Go + Python locally is heavy.
    - *possible solution*:
        - Use **Redpanda** (single binary compatible with Kafka) or **NATS Jetstream** (lighter, Go-native) (nats is alternative to kafka)
        - DuckDB is just a file, no server needed.

- ignore this, I won't do it for now. improvement: make calling python from go faster
    - Define your gRPC interface to accept BatchEventsRequest containing a list of events, rather than a single EventRequest.


details
    *   **Streaming**: Consumes from Kafka
        Q: what is sending data to kafka? where does it come from and what form is it in?
        A: a data generator (maybe json events) (e.g., { "user_id": 123, "action": "click", "timestamp": ... })
            - in a company, a upstream app is sending data to kafka, like a web server, or app. something customer facing maybe. but for this we'd use 
            - ex) A user clicks "Buy" on a website -> The Web API handles the order -> It simultaneously publishes an OrderCreated event to a Kafka topic

    *   **Batch**: gRPC endpoints for file uploads
        Q: check my understaning: so we'll have like get/post/put requests available like http endpoints for an api but we can use grpc to call them? and that's how people will upload files?
        A: kinda. grpc is a reaplcement for those http things, not a wrapper. in rest you send a post to upload a file. in grpc you define a function and the client calls that function directly

        Q: what is files in this context? because we have streaming already coming in from kafka, so are these endpoints like large datafiles manually uploaded?
        A: ya, these are offline/bulk data dumps. maybe we hit kafka storage maxes, we can upload a csv of 1 year of data instead. a credit score company might drop a huge csv on a ftp server once a week, I need a way to ingest that dump into my system
            - files are likely csv, parquet, or jsonL

        Q: where do kafka/grpc send the data?
        A: Everything goes to duckdb but critical data also gets updated in redis, but redis overall shouldn't have that much in it at any 1 time. since redis is expensive we do not store all new data. redis will have like user_age = 24, but new data comes in that's age 25, so we update redis THEN (before saving to duckdb). everything goes to duckdb. now redis can give critical info really fast on user log in. since file uploads are slow and likely not critical for the user to see, we store info from it in redis only if it has critical info, otherwise it goes in duckdb
            - overall, depends on how the model uses data. for basic stuff, the distinction isn't about if it's the last 1 day of data vs last month of data, it's about access patterns (online vs offline).
            - The duel write stategy. every time data comes in (kafka or file upload), the transformation engine calculates the features and sends it to BOTH duckdb and redis but in different formats
                - online store: redis: only the latest values. used for inference. when a user logs in, the model needs their current stats right away. This means we keep a little data here but update it
                - offline store: duckdb: every data point: for training, all data is equally as important
            - file uploads are usually backfilling, slow data. 


    *   *Role*: Deserializes data, validates schema against the Metadata Registry, and routes to the Transformation Engine.

*   **Transformation Engine (Go Host + Python Workers)**

    *   **The "Sidecar" Pattern**:
        *   The Go service acts as the controller
        *   It maintains a pool of **Python Workers** who process a gRPC file upload in parallel
            - what do my options look like?
                - docker
                    - my docker_compose.yml would have one go-controller service and multiple python workers services or 1 service scaled to n replicas. so each workers is 1 container
                    - the go service would loop up the workers by their dns name. it maintains a connection pool, when it has work it picks 1 worker container and sends it the data'
                    - me: this seems like a huge bother. just don't do it
                    - pros: decoupled/isolcated
                - gRPC 
                    - 1 user action != 1 worker. ex) a user uploads a csv of 1000 rows, the go host gets the file, and might split those 1000 rows into batches for workers to work on in parallel

        *   When an event arrives, Go sends the payload to a Python worker.

        *   Python runs the user-defined transformation (e.g., `def compute(event): return event['value'] * 2`).
            Q: does this just mean we transform the raw data into usuable features for the models?
            A: yes

        *   Go receives the result.

    *   *Why*: Best of both worlds. Go handles high concurrency/networking; Python handles the data science flexibility.

*   **Storage Layer**

    *   **Offline Store (DuckDB)**:
        *   Go writes processed features here in batches (to avoid locking the file too often). use duckdb appender if possible for extra speed for data, and use a special sql command for files "insert into features select * from 'upload.parquet'"
            Q: since duckdb is bad at writes, how should I approach this? is it good enough to write data in batches and write the whole file uploads at once? so there's only every 1 write happening?
            A: yes, 
                - just batch the data and write here only when the kafka buffer fills (like 5k rows) or on a timer. also can use the duckdb appender api in go, it's much faster than running insert sql because it writes directlry to the binary format
                - for files duckdb is really good at this, you can ingest a 1 gb csv in seconds using "insert into features select * from 'upload.parquet', this counts as 1 write

        *   Used for: Generating historical training datasets using Point-in-Time joins.

    *   **Online Store (Redis)**:
        *   Go writes the *latest* value here immediately after transformation.
        *   Used for: Real-time inference lookups.

*   **Serving API (Go)**
    *   Exposes gRPC endpoints for Service B (Model Serving) to fetch features.
        Q: how do launch or host this api? I'll have a data generation script running so basically how does it call it?
        A: standard client/server model. the server is my go program, to host it I just run the go script. it'll start a service on a port and wait for connections. the data generator is the client that connects to the server and it just calls the function - that's the api

END OF SERVICE A ----------------------------------------------


SERVICE B BREAKDOWN
**breakdown of service B (Model Serving Platform)**

**Core Philosophy:**
- **Latency is King**: Unlike Service A (throughput focus), Service B is about speed (ms).
- **Decoupled Compute**: The "Serving Layer" (handling requests) is separate from the "Inference Layer" (running heavy math).
- **Hydration**: The client often only sends an ID; the service "hydrates" the request by fetching features from Service A.

### 1. Core Components
Summary
- on startup, it should load the model weights
- Need a prediction gateway script (go). it takes in a "id" and sends the features associated (from redis) with that id (vectorized) to the inference engine
- Need an inference engine script. takes in the model features. 
??????


*   **Prediction Gateway (Go)**
    * the user or other service gives a request and this will look up that info in redis, add the missing data that we have stored, then send the whole thing vectorized to the model worker
        Q: is this accurate?
        A: yes. clients only know the user id. 

    **Role**: The traffic cop. It exposes the public gRPC API.
    *   **Responsibilities**:
        1.  Receives request (e.g., `Predict(user_id=123, model_version="v1")`).
        2.  **Feature Lookup**: Calls Service A's Redis to get the latest features for `user_id=123`.
        3.  **Routing**: Sends the combined vector (features) to the appropriate Model Worker.
    *   *Why Go?* It handles thousands of concurrent open connections much better than Python.

*   **Inference Engine (Python Workers)**
        input: the model features from the prediction gateway. 
        the model weights should have already been loaded at startup. this means we should have some random model stored for testing.

        Q: so basically this is called by the prediction gateway. The request it gets is something like a user_id and model_version and it loads the necessary model weights and artifacts
        A: No. this actually gets the features (the numbers/vectors), not the user_id. the gateway already did that work of swapping the user_id for the data. also the worker doesn't load weights per request; that would be too slow. the worker loads the model once when it starts up (the warm pool pattern mentioned elsewhere), so when teh reqeust arrives the model is already loaded.
    
    **Role**: The brains. It loads the actual model artifacts (.pkl, .onnx, .pt) into memory.
        Q: what does model artifacts mean?
        A: the files made by the training process. "save data" from a video game

    *   **Responsibilities**:
        1.  On startup: Downloads model weights from storage (S3/Local MinIO)
            Q: this project doesn't find any model weights, I've assumed that I'd have to download some models to run using this service to test things, so should I download models and store their weights in something like s3?
            A: yes. I can also use a dummy scikit learn model and save it as a pkl file

        2.  Listens for internal gRPC requests from the Gateway.
        3.  Runs `model.predict(features)` and returns the float/class.
            Q: what does it actually return?
            A: the raw prediction result. might look different depending on the model

    *   *Why Python?* Almost all ML libraries (PyTorch, Scikit-Learn) are native to Python.

*   **Model Registry (Metadata Store)**
    *   **Role**: The librarian.
    *   **Responsibilities**:
        - Tracks versions: "v1.0 is the current prod model", "v1.1 is the canary".
        - Maps models to locations: "v1.0 is located at s3://models/churn/v1.pkl".
        - Service B checks this on startup or periodically to know what to load.

### 2. Key Issues & Design Choices

*   **Issue: The "Hydration" Problem (Latency)**
    *   *Challenge*: If the client sends `user_id`, Service B has to make a network hop to Service A (Redis) to get features. This adds latency.
    *   *Option 1 (Thick Client)*: Client fetches features from Service A, then sends them to Service B. (Bad security, complex client).
    *   *Option 2 (Server-Side Hydration)*: Service B fetches features.
        *   *Choice*: **Option 2**. It keeps the client simple. The Gateway (Go) creates a high-performance connection pool to Redis (Service A).

*   **Issue: Python Global Interpreter Lock (GIL) vs Concurrency**
    *   *Challenge*: A single Python process can't handle parallel CPU-bound inference well.
    *   *Solution*:
        - **Worker Pool**: Run multiple independent Python processes (or Docker containers).
        - The Go Gateway Load Balances requests across these workers (Round Robin or Least Connections).

*   **Issue: Model Cold Starts**
    *   *Challenge*: Loading a 2GB model takes 10 seconds. You can't start a worker "on demand" for a request.
    *   *Solution*: **Warm Pools**. Workers must load the model *before* accepting traffic. Readiness probes (k8s/docker) only route traffic once the model is loaded.

### 3. Detailed Workflow (The "Request Path")

1.  **Client** calls `GetChurnPrediction(user_id=101)` via gRPC.
2.  **Go Gateway** receives request.
    - Checks `user_id=101`.
    - Calls **Service A (Redis)**: `MGET user:101:age user:101:clicks ...`
    - Receives `[25, 120]`.
3.  **Go Gateway** constructs payload: `[25, 120]`.
4.  **Go Gateway** picks a healthy **Python Worker** and sends internal gRPC: `PredictInternal(features=[25, 120])`.
5.  **Python Worker** (with XGBoost loaded) runs math. Returns `0.85`.
6.  **Go Gateway** returns `{ "churn_probability": 0.85 }` to Client.