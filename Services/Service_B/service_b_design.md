**Basic Design**

how the user interacts with a model
- at startup, we look for models in azure with "production" in the file name. then we download all that models artifacts into redis
- at startup, we store each prod models name into redis. now we can find out which models we can serve the user
- service b lists the available models to the user, who selects one
- service b queries duckdb for the model metadata, then asks the user for the required features
- service b sends the request over gRPC to a python file
- service b python file unpickles the model from redis, and runs the model



we need something to define the interfact of each model. model signature. it'll map values to features (redis)
  {
  "model_id": "credit_risk_v1",
  "version": "1.0.0",
  "trained_date": "2023-10-25",
  "status": "production"
  "azure_location": "ai-models"
  "expected_features": [
    { "index": 0, "name": "debt_to_income_ratio", "type": "float" },
    { "index": 1, "name": "age_years", "type": "int" }]
  }   

0) misc work
    - add redis model signature system, add this to duckdb. load into redis on startup, service b should check this on startup also
    - add the stock models to the program
    - add model event log table to duckdb
    - move the azure loading and system checks to the main.go process before the service is even called, then pass azure stuff to it

1) On startup, we load the model weights and model artifacts into redis (like configs, and scalers) from azure blob
    - this is the only part of the program that needs these

2) A prediction gateway script (go)
requests should be load balanced (workers do each reqeust)
    - the user is presented with the various models and they choose which one to use
    - the user prompts a model: input = [volatility %, minutes since open]
        - this sends this: { "model_id": "roi_prediction", "inputs": [0.6, 45] }. we look up the model signature in redis which has the expected features from redis, it approves the request, then we give it to the model
    - prompt the model via grpc
    - store model events: {"user_request_timestamp": , "model_id": "", "inputs": "[]", predicted_at_timestamp: timestamp} in duckdb

    - load balance handling (1000 users): just do 1000 go routines. the files can talk to each other via go channels
    - you'd only use python anywhere in this if the construction of the prompt is heavy compute. like using huggingface libraries 

- metadata/version control script
    - Tracks versions: "v1.0 is the current prod model", "v1.1 is the canary". (whichever has the prod tag)
    - Maps models to locations: "v1.0 is located at azure://models/churn/v1.pkl".
    - Service B checks this on startup or periodically to know what to load.
    - Store in duckdb. redis has good hashing but I want to keep as much out of memory as possible




---------------------------------------------------------------------------------------
(from the design.md file)
### SERVICE B BREAKDOWN

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
