Start your venv
- bash ./venv/Scripts/activate to activate. if it worked I should see "venv" as my location in terminal
- I had to run this to see it in powershell: Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope Process -Force; .\venv\Scripts\Activate.ps1
- in git bash run this: source venv/Scripts/activate

-----------------------------


# fast/basic overview of the services
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
- gRPC inference API
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
