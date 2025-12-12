### design issues
1) how to separate services? 
    - when I have 4 services, with kafka and grpc sending data between them, and ideally some kind of global limiter so we don't spawn too many python workers -> what does this all look like?

### Service A
1) make data generation script
    - data sent via kafka to transformer engine
    - files sent over gRPC to transformer engine
2) make tranformer engine
    - python worker pool (global tracking of workers across all services so we don't exceed the max)
    - all data to duck db
    - some data to redis
    - I don't think promethius does anything here. but I should check later


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


