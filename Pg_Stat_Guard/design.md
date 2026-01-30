This is carefully made over a few iterations. the main issue is that it's very easy to crash the databases, and it's hard to keep my low latency.

# Pg_Stat_Guard Design
This is a c extension whose goal is to do ml model drift detection on data in a postgres database only when requested by my local system. This means I'll do basic drift detection locally, and if something is wrong then I'll trigger this extension to a heavy compute over the postgres data. The database has data coming in constantly from multiple sources so the design needs to be particular regarding locks and other database issues.

## Overview
- No ingest trigger. Data goes in pg like normal.
- Connection details (host, port, user, pass, db) are defined in `Global_Configs/App.yaml` and loaded via `Config_Loader.go`.
- The platform connects to this Postgres instance to insert data and trigger drift detection.

**Exactly how it works**
the c file sends data to the azure url as a bytes payload. the python file does nothing locally, it's the function we run on azure. 
- pg reads the data
- the c file compresses the data into a tdigest (this happens 100% in the db, not locally)
- the c file uses pgsql-http to send this binary tdigest to teh azure url
- azure runs my python file on the data
  - the python code deserializes the data back into a tdigest and runs drift detection on it
- the python file/azure return a json result back to pg (my c code)


## Signal
- Local system runs something like: `if value > mean + 3*std_dev`
- If it gets flagged then it does something like: `SELECT perform_drift_analysis(interval '15 minutes');`
  - This checks the last 15 minutes of data.

## Postgres Function
- Execute internal query to fetch rows from the requested time window.
- These rows are fed to a C-based tdigest system. Since this is a batch operation, the CPU creates the tdigest very efficiently in one go, which is cache-friendly.
- Once the tdigest is made, the function sends this binary payload to Azure.
- To make this fast, we'll define a User-Defined Aggregate (UDA). This will let Postgres use its parallel workers to build the tdigest.
  - The UDA uses a state type to hold the tdigest centroids.
  - It has a transition function (`tdigest_add(state, value)`) that runs on the parallel workers.
  - It has a combine function like `tdigest_merge(state1, state2)` which lets Postgres parallelize the work and merge results.
  - Finally, we serialize the tdigest and send it as a payload.

## Azure
- We're using Azure Functions (serverless compute).
- Gets the tdigest, not the raw data.
- Runs the heavy ML/drift models on the tdigest.
- Returns the verdict to PG.
  - Use `pgsql-http` instead of something like `pg_net`.

## Flow
- Local Python does something like: `cursor.execute("SELECT perform_drift_analysis(...)")`
- Postgres: Aggregates data -> Calls Azure (Blocks for ~200ms) -> Receives JSON.
- Postgres: Returns the JSON as the result of the `SELECT`.
- Local Python: `result = cursor.fetchone()` contains the verdict.

### Notes
I can do another version that's better for large compute using `pg_net`, but it's overly complex.

## Postgres vs Azure
- Ingesting 1 million rows a day into Azure instead of PG would be at least $200 a month.
- Blob storage would kill me by API calls (too many). It might be like $10 a month but it won't work with drift detection.
- **Conclusion**: Just do it this way. Save to PG, send data to Azure for the compute.
  - Cosmos / Citrus: Storage
  - AI Search / Vector DB: For searching semantic embeddings (like text or images)
  - I need Azure Functions (serverless compute)
    - It runs code, not queries. I pay for the seconds the code runs.

## Existing Extensions
- `postgresML` is an extension that's too massive for me. It replaces the whole infra.
- `Pgdrift` looks for schema drift, not data drift.


# file format
something like this is usually how extensions are made
drift_detector/
├── drift_detector.c       # Your C logic (t-digest, UDA state management)
├── drift_detector.control # Metadata (version, description)
├── drift_detector--1.0.sql # SQL to register the C functions
└── Makefile               # The build instructions

# requirments
- pgsql-http extension installed in my pg server


# how to run
- from the extension dir run
    - make
      sudo make install
- enable in the db
    - CREATE extension pg_stat_guard;
- make sure azure is ready



### critical
- you have to save your azure functions url in the postgresql.conf file. the other way would be to call
sql commands everytime the extension is triggered to move it from a local env file to pg.