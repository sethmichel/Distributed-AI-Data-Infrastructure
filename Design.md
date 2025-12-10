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

### Major Overall Summary & Key Notes




# DATABASE OVERVIEW

NOTE: Any of these could be multiple python workers, so if something only has 1 instance of being used, it could be used by 10 workers at once

ISSUE: duckdb can only handle 1 writer at a time
    - SOLUTION: go routine that handles all writes, everything goes through this

SCENARIO: gateway gets Predict(user_id=101, model="churn_v1"), how does it know what features that model uses?
    - SOLUTION: need a feature registry that says what each model uses. then if data is in redis we can make the redis keys. not doing this makes changing models super annoying


Who writes and reads to duckdb?
- writes
    - service A
        - writes cleaned features in batches when kafka fills - use duckdb appender (send to redis first)
            - appender is built in, it's way faster than sql's 'insert'
            - Every feature must have a event_timestamp
            - to prevent future leaks: use `AS OF` joins (powerful). We'll store an append-only log of feature values in DuckDB: `(entity_id, feature_name, value, valid_at, created_at)`.
            - files - use "insert into features select * from 'upload.parquet'"
		- Service B
			- Writes prediction event logs (metadata)
		- Service C
			- Write model drift scores/analysis scores
		- Service D
			- Might store metrics here after sending to /metrics, not sure
			- Model baseline.json files after each model training
			- Model metadata/training results

	- reads
		- Service B
			- On request, it uses the user_id to get the features
			- In go routine every 5 minutes, checks which models are in prod. If new model, load their data from azure into redis
		- Service D
			- Get the service B prediction logs
			- Final feature/much later: get model labels (truths)
			- Read model drift scores that service C wrote

Who writes and reads to redis?
	- writes
		- Service A
			- writes cleaned features
		- Service B
			- On startup it sends prod model artifacts from azure to here
		
	- Reads

Who uses azure
	- Writes
		- Service B
			- On startup load prod model artifacts into redis, also happen at any time

    - reads

END OF DATABASE OVERVIEW -----------------------------------------

# THREADING/ROUTINE CONSIDERATIONS

Design decision: how do I handle so many python workers? multiple services use pools of these workers. And how do I handle so many go routines? Again multiple services use a bunch of go routines

Assume across all services I have 10 go routines and 12 python workers all needing to run at the same time

ISSUE: how to handle a routine or worker crashing?
    - I need a health check 

SOLUTION:
    - answer: just set a max_python_workers and have a job queue system they slowly go through
    - go routines: they're super lightweight and I can run tons with no issues. the reason they're so lightweight is because they only grow in resouce consumption as needed and aren't managed by the os. also swapping btw routines is crazy fast by omitting the os. it does other things too that let it scale to thousands of routines
    - python workers: these use an actual cpu thread, also each worker might load in libraries like pandas or numpy which can eat ram. but doing everyhting via go is a really hard

END OF THREADING CONSIDERATIONS ----------------------------------

# OVERALL ISSUES
- how do I restart/restore that data for failed python workers?
    - my doctor thread habit won't work since this is a distributed system
    - SOLUTION: don't restore data state, just restart the worker with the same request. if a worker hangs and doesn't crash, then my workers can use the check() gRPC method every 5 seconds which I guess will tell me if they're stuck. I guess this means if a worker doesn't check in every 5 seconds then I assume it's stuck

- service C does heavy reads on duckdb. so many that it might block other services from writing to it.
    SOLUTION: other services can buffer logs in memory/Go channel and flush to duckdb in batches rather than per request

- where to store the job queue for workers
    SOLUTION: redis. do not store in duckdb, it's too frequent. can also use sqlite

- what are the hardest areas?
    - go <-> python gRPC code. because of data types
    - adding a new feature to a schema is a pain. I'd have to update the transformation worker code, the go metadata registry, the model
        - best way to deal with it is 1 unified config, but I think I'll skip that for this learning project
    - compute load: pipelines, and all these processes are really heavy. 



### SERVICES BREAKDOWN


**breakdown of service A (Feature Store)**

- Ingests streaming + batch data
- Computes features with a DAG engine (Transform Orchestrator/controller)
    - reason it's a dag: I'm transforming raw data into features. this uses steps that depend on each other. it's a really simple dag
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

- any critical data is sent to redis, and all data is sent to duckdb after that (after transformation). use duckdb appender for kafka data, and use something like "insert into features select * from 'upload.parquet'" for files.
    - issue: Ensuring we don't leak future data.
        - Solution: Every feature write MUST have an `event_timestamp`.
            - DuckDB Query Strategy: `AS OF` joins are powerful here. We will store an append-only log of feature values in DuckDB: `(entity_id, feature_name, value, valid_at, created_at)`.

    kafka + gRPC -> transformation engine -> redis/ duckdb

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
- on startup, the inference engine should load the model weights and model artifacts into memory (like configs, and scalers) (it's the only one who needs it)
- A prediction gateway script (go)
    - it takes in a "id" and sends the features associated (from redis) with that id (vectorized) to the inference engine via a gRPC request. the request is for a model prediction
    - should load balance the prediction reqeusts to the inference engine. so it should assign the job to workers
- An inference engine script
    - takes in the model features from the prediction gateway, and does a prediction.
    - this needs access to the model artifiacts/weights to do the prediction
    - uses python workers to process
- metadata/version control script
    - see notes. store metadata in duckdb, and do go routine polling every 5 minutes to find which model is in prod

Issues
*   **Getting features Problem (Latency)**
    *   I don't care, ignore for now
        *   *Challenge*: If the client sends `user_id`, Service B has to make a network hop to Service A (Redis) to get features. This adds latency.
        *   *Option 1 (Thick Client)*: Client fetches features from Service A, then sends them to Service B. (Bad security, complex client).
        *   *Option 2 (Server-Side Hydration)*: Service B fetches features.
            *   *Choice*: **Option 2**. It keeps the client simple. The Gateway (Go) creates a high-performance connection pool to Redis (Service A).

*   **Python Global Interpreter Lock (GIL) vs Concurrency**
    *   *Challenge*: A single Python process can't handle parallel CPU-bound inference well.
    *   *Solution*:
        - **Worker Pool**: Run multiple independent Python processes.
        - The Go Gateway Load Balances requests across these workers (Round Robin or Least Connections).
        - dealt with this exact issue in the trading platform, worker pool will work

*   **Model Cold Starts**
    *   *Challenge*: Loading a 2GB model takes 10 seconds. You can't start a worker "on demand" for a request.
    *   *Solution*: since this is small scale, just load them at startup and sleep the program while they're loading.
    *   *Actial large scale solution*: Warm Pools. Workers must load the model *before* accepting traffic. Readiness probes (k8s/docker) only route traffic once the model is loaded.

Details
*   **Prediction Gateway (Go)**
    * the user or other service gives a request and this will look up that info in duckdb, add the missing data that we have stored, then send the whole thing vectorized to the model worker
        Q: is this accurate?
        A: yes. clients only know the user id. 

    **Role**: The traffic cop. It exposes the public gRPC API.
    *   **Responsibilities**:
        1.  Receives request (e.g., `Predict(user_id=123, model_version="v1")`).
        2.  **Feature Lookup**: Calls Service A's duckdb to get the latest features for `user_id=123`.
        3.  **Routing**: Sends the combined vector (features) to the appropriate Model Worker.
    *   *Why Go?* It handles thousands of concurrent open connections much better than Python.

*   **Inference Engine (Python Workers)**
        - Should load model weights at startup from azure
        - Should load model artifacts at startup from azure into memory. reason we need non weight files is like scalers, tokenizers, configs...
        Take in the model features (vectorized) from the prediction gateway; meaning the gateway sends a gRPC request to here, then it does the model prediction and returns the result. the python workers (pool of workers) is managed by the go gateway. go gateway can load balance the requests among them for parallel processing. can do multiple workers on 1 model, or on multuple models

        Q: so basically this is called by the prediction gateway. The request it gets is something like a user_id and model_version and it loads the necessary model weights and artifacts
        A: No. this actually gets the features (the numbers/vectors), not the user_id. the gateway already did that work of swapping the user_id for the data. also the worker doesn't load weights per request; that would be too slow. the worker loads the model once when it starts up (the warm pool pattern mentioned elsewhere), so when the reqeust arrives the model is already loaded.
    
    **Role**: The brains. It loads the actual model artifacts (.pkl, .onnx, .pt) into memory.
        Q: what does model artifacts mean?
        A: the files made by the training process. "save data" from a video game

        Q: it says its role is to load the actual model artifacts into memory, does it do that 1 time at startup? and if the artifiacts are just files made by the training process, then why load them into memory at all? it seems to me that this sub-component of service B is meant to do model predictions, not train the models. 
        A: do it 1 time at startup, we need other files like configs, and scalers in memory

    *   **Responsibilities**:
        1.  On startup: Downloads model weights from azure
            Q: this project doesn't find any model weights, I've assumed that I'd have to download some models to run using this service to test things, so should I download models and store their weights in something like azure?
            A: yes. I can also use a dummy scikit learn model and save it as a pkl file

        2.  Listens for internal gRPC requests from the Gateway.
            Q: what internal gRPC requests is the gateway making to this?
            A: the gateway sends a prediction reqeust

        3.  Runs `model.predict(features)` and returns the float/class.
            Q: what does it actually return?
            A: the raw prediction result. might look different depending on the model

    *   *Why Python?* Almost all ML libraries (PyTorch, Scikit-Learn) are native to Python.

    **Model Registry (Metadata Store)**
    *   **Role**: The librarian.
    *   **Responsibilities**:
        - Tracks versions: "v1.0 is the current prod model", "v1.1 is the canary".
        - Maps models to locations: "v1.0 is located at azure://models/churn/v1.pkl".
        - Service B checks this on startup or periodically to know what to load.    
    *   **Storage Strategy**:
        -   **Metadata**: Store in duckdb. redis has good hashing but I want to keep as much out of memory as possible
    *   **how we switch models**: 
        - Redis has Pub/Sub, but we're using duckdb. so I have to use polling every 5 min via a go routine. just check which model has the tag "production"


### 2. Detailed Workflow (The "Request Path")

1.  **Client** calls `GetChurnPrediction(user_id=101)` via gRPC.
2.  **Go Gateway** receives request.
    - Checks `user_id=101`.
    - Calls **Service A (Redis)**: `MGET user:101:age user:101:clicks ...`
    - Receives `[25, 120]`. 
3.  **Go Gateway** constructs payload: `[25, 120]`.
4.  **Go Gateway** picks a healthy **Python Worker** and sends internal gRPC: `PredictInternal(features=[25, 120])`.
5.  **Python Worker** (with XGBoost loaded) runs math. Returns `0.85` (prediction)
6.  **Go Gateway** returns `{ "churn_probability": 0.85 }` to Client.


END OF SERVICE B -----------------------------------------------------------------


SERVICE C ------------------------------------------------------------------------
Service C: Drift + Data Quality Monitoring

**Core Philosophy:**
- **Observability is Reliability**: A model is not "done" when deployed. It degrades (drifts) the moment it hits production data. We must treat data quality as a system health metric.
- **Asynchronous & Non-blocking**: Monitoring calculations (statistical tests) are CPU heavy. They must *never* block the inference request (Service B). They run on the side (after the fact).
    Q: what are the monitoring calculations? just the drift detection system? and is it accurate to say these are probably the lowest priority processes of the app and should be paused when anything else needs the resouces?
    A: Yes, these are the statistical drift tests (KS-test, Chi-Square) and data quality checks (null rates). They are "offline" tasks and are lower priority than serving user requests. If the system is under load, monitoring jobs can be delayed or skipped without impacting the user experience.

- **Windowed Analysis**: Drift is statistical. You can't detect drift on 1 sample. We analyze windows of data (e.g., "Last Hour" vs "Training Data").
    Q: elaborate a bit more on what this means, and how to design it
    A: A single data point (e.g., Age=25) has no "distribution" to compare. You need a collection of points to form a shape (a histogram).
        - *Design*: We group inference logs into time buckets (e.g., 1-hour windows).
        - At the end of the hour, the Drift Worker wakes up, grabs that batch of data, calculates its statistics (mean, variance, histogram bins), and compares those stats to the baseline.

- **Python for Math**: We need libraries like `scipy`, `numpy`, or `scikit-learn` for calculating statistical distances (KS-Test, PSI, KL Divergence).

- **Prometheus/Grafana for Visualization**: Don't build a UI. Use standard infrastructure tools to alert us when `drift_score > 0.1`.
    Q: slightly unclear. first, by "vis" do you mean "visualization"? Then, I'm not familiar with prometheus or grafana. are we planning on using prometheus to collect the metrics info and use that info to compute key metrics like drift score (so prometheus would do the collecting and do the calculating)? I think we can have prometheus hit a /metrics endpoint we make every x seconds to get the metric data. Then, grafana will show the metrics that prometheus collects/computes? So basically prometheus is collecting/processing the metrics, and grafana is showing those metrics?
    A: mostly right, with one correction: Prometheus does NOT calculate the drift score. It is just a time-series database.
        - *Workflow*: Your Python Worker does the heavy math and calculates `drift_score = 0.45`. It exposes this number on a `/metrics` page.
        - *Prometheus*: "Scrapes" (visits) that page every 15s and stores the number `0.45` with a timestamp.
        - *Grafana*: Queries Prometheus to draw the line chart of those numbers over time.

### 1. Core Components
Summary
- graphana and prometheus need to be running on dockerarized localhost

- Service B sends the record of each prediction event to duckdb. Now this service is meant to process that and other metrics on a dashboard. however it's the lowest priority of the program so it should be paused if resources are needed elsewhere

- Every so often, we send a python worker to read the duckdb service B logs. it will find the model drift and feature drift. we can compare against the feature_baseline.json files that service D stored in the metadata area of duckdb.
    - the feature_baseline.json is like the histogram for each feature on a model we know doesn't have drift

- when the worker is done calcualting, it exposes the resutls to /metrics endpoint, prometheus scapes it, granfana queries prometheus every x minutes and updates the dashboard

- do model accuracy tracking much later: since this is a learning project, the random models I'll put on this platform don't have real world applications so I can't measure their accuracy. I'd need to make a new label_generator.py file to simulate like if it predicts a transaction is valid but it turns out to be fraud. I can track feature drift though. 



Issues
*   **The "Ground Truth" Lag**
    *   *Challenge*: We can calculate *Feature Drift* immediately (e.g., "Users are suddenly younger"). But we can't calculate *Performance* (Accuracy/Precision) because we don't know the actual outcome yet (e.g., "Did the user actually churn?"). That data comes days/weeks later.
    *   *Decision*: Scope Service C to **Drift Detection** (Feature/Prediction distribution) first. Handle Performance monitoring as a delayed batch job later when labels arrive.
    Q: what does this mean? if we're making a prediction then how would we know the "actual outcome" later? what are the labels that arrive days/weeks later?
    A: a label is the correct answer, it's for predictions like "the user will cancel the subscription" and we don't know until the first of the next month. since I don't have a real work application for these models, I'll need to simulate this in my data geneator file. probably one of the last features I add to the app. so ignore accuracy monitoring

*   **Defining the "Baseline"**
    *   *Challenge*: To know if data has "drifted", we need to know "drifted from what?".
    *   *Solution*: The Model Registry (in Service B) must store a `baseline_stats.json` or a reference dataset (parquet) alongside the model weights. Service C loads this to compare against the live traffic.

*   **Compute Overhead**
    *   *Challenge*: Running KS-Tests on millions of rows is slow.
    *   *Solution*: use a window of data. so this'll be like 1k samples for each feature instead of 10k samples per feature
    Q: what exactly does this mean? if I have 10 features for a model, and 10,000 data points per feature, what are you saying I should do?
    A: running on like 10 million points is super slow, we pick a random like 1000 points and find teh drift on that. this is basically what I already thought because we'll probably use a window of data

Details
*   **Drift Worker (Python)**
    *   Summary: The calculation engine.
    *   Input: Reads "Inference Logs" (what Service B predicted) and "Reference Data" (what the model was trained on).
        Q: so inference logs is basically just the model predictions from service b? saying "logs" is a bit misleading if that's what it is.
        A: it's a bit more than the prediction, it's a complete record of the event: timestamp, inputs (features), output (prediction). with these info we can figure out why we have prediction drift. like if we used to mostly deal with 20 yr olds, now it's 50 yr olds

    *   Logic: Calculates statistical distance (Drift)
    *   Output: Drift scores per feature
        Q: briefly, how are we going to figure out drift score per feature? I can maybe see how the model as a whole would get drift score, but if we do per feature, then is that going to be something like make a histogram for all data for feature x over a 7 day period and compare that to a histogram of the data from an older 7 day period? would that show the drift?
        A: yes. ex) you take a histogram of feature x from training data, make a histogram of recent data, compare them. (ks-test, psi). we do this for all features

*   **Metrics Server (Go)**
    *   Summary: The interface to the monitoring world.
    *   Role: Runs a ticker (e.g., every 10 min), triggers the Python Worker to calculate drift, receives the scores, and exposes them on a `/metrics` endpoint.
    *   Tech: Prometheus Go client.

*   **The Inference Log (DuckDB)**
    *   Summary: The shared memory between Serving (B) and Monitoring (C).
    *   Note: Service B needs to log every request `(timestamp, feature_inputs, prediction_output)` here. Service C reads from here.
        Q: so what exactly do I need to do here? Are you saying I should just have a system for storing service B's prediction logs so service C can see them? And I store them in duckdb?
        A: yes, store in duckdb. duckdb is used by service B and C. B is the writer and C is the reader.

*   **Dashboarding (Grafana)**
    *   Summary: Visualizes the Prometheus metrics over time.
        Q: briefly, what goes into making this grafana dashboard? is it going to be on localhost... basically how do I create and access a grafana dashboard for this personal project?
        A: since this is a personal project, I can run it in a docker container or a local binary. if docker then it's on localhost. in grafana we add a data source pointing to prometheus insteance (prometheus runs on another port on localhost). now we can make a dashboard


END OF SERVICE C ---------------------------------------------------


SERVICE D ----------------------------------------------------------
Service D: Batch Pipeline Orchestrator (The "Trainer")

important distrinction of about service D's real role in this project: 
    - at first it seems like it's overlapping tasks with other services; service a does the feature etl (handles raw data -> cleans/transforms it). service D handles those cleaned features and trains teh model with them on a schedule. service D never processes raw logs, it assumes service A already put clean data in duckdb and so it queries duckdb and trains the models
    - it's the "factory" that builds the actual products (models) which service B sells (serves to users)
    - "Doesn't Service A do ETL?"
        - **Service A (Feature ETL)**: Ingests Raw Logs -> Transforms -> Saves Features. (Happens constantly).
        - **Service D (Training Pipeline)**: Reads Features -> Trains Model -> Saves Model. (Happens weekly/on-demand).


**Core Philosophy:**
- **The "Factory"**: Services A and B are the "Storefront" (gathering data for and selling predictions). Service D is the factory that builds the products (Models).
- **Schedule-Driven vs Event-Driven**: Unlike Service A (which reacts to incoming data streams) or Service B (which reacts to user requests), Service D works on a schedule (e.g., "Retrain every Sunday at 2am") or triggers (e.g., "Drift detected").
- **State Management**: Training jobs take minutes or hours. If the app crashes, we need to know which jobs failed. We need a persistent "Job Queue".
- **Separation of Concerns**: 
    - Service A handles **Data ETL** (Raw Data -> Features).
    - Service D handles **Model Training Pipelines** (Features -> Trained Model).

### 1. Core Components
Summary
- go job scheduler file manages the training process. it's a cronjob to trigger a model retain
    - expose gRPC endpoints for the various tasks (model retrain, drift detection)
    - drift detection endpoint triggers service C
    - serivce C finding very high drift on a model will trigger the model retrain endpoint
    - has a job queue to track job statuses. this needs to save state data and be able to handle the job failing/breaking, succeeding
        - likely store the job queue in duckdb

- python workers do tasks not handled by other services, like training
    - in training it'll get the training data from duckdb, run training libraries, validate the model, and save the model and baseline stats for service c (stored in metadata area of duckdb)
    - if the retrained model is better than the prod model, just update the database entry for it signal this so service b can load it instead of the outdated model

Issues:
*   **Resource Contention**
    *   *Challenge*: Training a model eats 100% CPU. If we run this on the same machine as Service B (Serving), user requests will lag.
    *   *Solution*: 
        - In a real cluster: Run Service D on separate nodes.
        - For this project: simple resource limits or just schedule D to run at night.

*   **Dataset Size vs Memory**
    *   *Challenge*: We can't load 100GB of DuckDB data into Python RAM to train.
    *   *Solution*: Just don't train, this'll be the last thing we work on 
        - Use **Iterative Training** (e.g., `partial_fit` in sklearn or streaming datasets in PyTorch) reading directly from DuckDB chunks.
        - Or just limit the training set size for this learning project.

Details
*   **Job Scheduler (Go)**
    *   **Role**: The Manager.
    *   **Responsibilities**:
        - Runs a simple internal cron (e.g., `time.Ticker` or a library like `gocron`).
        - Exposes a gRPC endpoint `TriggerJob(job_type, params)` so Service C (Drift Monitor) can trigger a retrain if drift is high.
        - Manages a **Job Queue** (in Redis or Postgres/SQLite) to track status: `PENDING`, `RUNNING`, `COMPLETED`, `FAILED`.

*   **Training Workers (Python)**
    *   **Role**: The Heavy Lifter.
    *   **Responsibilities**:
        - Listens for jobs from the Scheduler.
        - **Extraction**: Connects to Service A's **Offline Store (DuckDB)** to fetch historical training data (e.g., `SELECT * FROM features WHERE event_time > '2023-01-01'`).
        - **Training**: Runs `scikit-learn` / `xgboost` / `pytorch` training logic.
        - **Validation**: Checks if the new model is actually better (Accuracy/F1 Score).
        - **Artifact Creation**: Saves the model file (`model_v2.pkl`) and a `baseline_stats.json` (for Service C).

*   **Artifact Manager (Shared Logic)**
    *   **Role**: The Delivery Driver.
    *   **Responsibilities**:
        - Uploads the `.pkl` and `.json` files to azure
        - Updates the **Model Registry** (in DuckDB) so Service B knows a new model is available (e.g., `INSERT INTO models (version, azure_path, status) VALUES ('v2', 'azure://...', 'staging')`).

### 3. Detailed Workflow (The "Retrain Path")

1.  **Trigger**: It's Sunday 2:00 AM. The **Scheduler** creates a job `TrainJob(model="churn", data_range="last_30_days")`.
2.  **Worker Pickup**: A free Python Worker picks up the job.
3.  **Fetch Data**: Worker queries Service A's DuckDB: `SELECT * FROM churn_features WHERE timestamp > NOW() - INTERVAL 30 DAYS`.
4.  **Train**: Worker runs `model.fit(X, y)`.
5.  **Evaluate**: Worker splits data, tests, and finds `accuracy = 0.88`. Old model was `0.85`.
6.  **Publish**:
    - Upload `model_v2.pkl` to azure
    - Upload `baseline_v2.json` (stats of the training data) to azure (for Service C).
    - Write to DuckDB Model Registry: `Version: v2, Status: Staging`.
7.  **Complete**: Mark job as `SUCCESS`. Service B will eventually pick up the new model.




--------------------------------------------------
Design Notes
- Need a resource tracker. should not exceed 90% ram, limit the number of threads/workers, etc

- Need to code in sleep's/timers to wait until other processes are finsihed. either so workers/threads free up, or for processing

- might not be mentioned anywhere, but model weights/arti's should be in azure
    - need to choose what goes into azure

- lingo: it seems that "online" means fast lookup/hot data, and "offline" means low priority data like training data

- lingo: "hydrated" means the full data amount we need. ex) we give the user_id and it gets hydrated by returning all the features for that user_id

- have 1 docker yml file to wpin up containers for prometheus, grafana, redis. and make a rebuild script. redis actually doesn't support windows so it needs to be in docker. serivce A,b,c code should be outside docker. prometheus is in a container but needs to scrape my local endpoint so I need soemthing like this under prometheus in the yml file, and point to whatever port my go app uses instead of localhost
        extra_hosts:
      - "host.docker.internal:host-gateway" # CRITICAL: Allows Prometheus (inside Docker) to talk to your Go app (on localhost)

- DO NOT TRAIN MODELS, it's way too much compute. make it the final thing I do if at all. this means service D will "fake train" models

----------------------------------------------------

NEXT STEPS
- figure out azure
    - azure blob storage with a tiny custom model registry around it. it's not great for metadata but that's going in duckdb

- ask ai about the overall project, what it thinks the hard parts are, what thinks I should avoid (requires I clearify what areas I'm skipping so it knows)
    - how will I handle multiple services trying to write to duckdb?
        - solution: some sort of job queue OR a singleton database writer in go which is a go routine for writes that it gets via a channel. OR use separate duckdb files for each service

- write a master summary 

- write the resume bullet points

- write a summary of what to setup since the setup is always pretty hard. goal is not to have to mess it at all during dev

- write each service one by one