Service D: Batch Pipeline Orchestrator (The "Trainer")

important distrinction of about service D's real role in this project: 
    - at first it seems like it's overlapping tasks with other services; service a does the feature etl (handles raw data -> cleans/transforms it). service D handles those cleaned features and trains the model with them on a schedule. service D never processes raw logs, it assumes service A already put clean data in duckdb and so it queries duckdb and trains the models
    - it's the "factory" that builds the actual products (models) which service B sells (serves to users)
    - "Doesn't Service A do ETL?"
        - **Service A (Feature ETL)**: Ingests Raw Logs -> Transforms -> Saves Features. (Happens constantly).
        - **Service D (Training Pipeline)**: Reads Features -> Trains Model -> Saves Model. (Happens weekly/on-demand).


**Core Philosophy:**
- **The "Factory"**: Services A and B are the "Storefront" (gathering data for and selling predictions). Service D is the factory that builds the products (Models).
- **Schedule-Driven vs Event-Driven**: Unlike Service A (which reacts to incoming data streams) or Service B (which reacts to user requests), Service D works on a schedule (e.g., "Retrain every Sunday at 2am") or triggers (e.g., "Drift detected").
- **State Management**: Training jobs take minutes or hours. If the app crashes, we need to know which jobs failed. We need a persistent "service d job queue" not to be confused with the duckdb job queues that all services push to in redis.

### 1. Core Components
- startup
    - using duckdb model_metadata table, get the data for all values who have status = "production" and save them in redis

- Golang job scheduler: the brain of service d, it handles the scheduling of tasks
    - Analytics pipeline: 
        - call service C to run analytics via a gRPC endpoint, this will run feature drift detection and cause alerts to happen if the results are bad (we see this in Services/Service_C/drift_detection.py sound_the_alarms()). Service C should actually tell service D what the drift results are for each feature tested and service D is the one who sounds the alarm, and updates the database tables - not service C. 
        - This means when service c returns the results, service d should send a duckdb write request to the db_client.go to write the drift results to the model_metadata table. model_drift_score is blank since we aren't calculating that yet, but the expected_features struct has a drift_score for each feature that should be updated. Remember, we send duckdb job requests via a redis job queue.
        - Service D needs to log the drift results into duckdb 
        - if the alarm is sounded due to bad data, we should queue a model retraining for any model using the affected features
            - model retraining should work like this
                - If we get bad drift results from service C, submit a read request to the duckdb redis job queue for model_metadata table to get model_id and expected_features. for all features with bad drift scores, we'll retrain those models. 
                - Service d handles the retraining. Either call the necessary functions in service d or use a gRPC endpoint to call it. for right now, just do it right away, don't worry about system resources.
                - After retraining the models
                    - when we're done retraining, connect to azure blob storage. each model has a azure folder named "Model_{model name}" like "Model_Coffee_Recommender". inside each folder is all the artifacts for that model, each production artifact ends with "_Production". Now that we just retrained the model, we need to remove the production word from the end of these titles, then upload the new model artifacts we just made to this location and they should have "_Production" at the end of their names.
                        - file names should be "Model_{model name}_Production"
                        - other services will pull these artifacts based on their name ending in "Production"
                    - the duckdb model_metadata table should make a new entry for these models since they have new versions, drift scores (none), and trained dates. it makes sense to keep the old metadata for the older models.
                
                now, we need a way to tell service b to download the new production models and delete its old ones.




- go job scheduler file manages the training process. it's a cronjob to trigger a model retain
    - expose gRPC endpoints for the various tasks (model retrain, drift detection) (my gRPC code is in the Proto directory)
    - drift detection endpoint triggers service C, which is in charge of analytics
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

