duckdb schema
    CREATE TABLE features (
        entity_id text,            -- "user_123"
        feature_name text,         -- "click_count_7d"
        value DOUBLE,              -- 42.0
        event_timestamp TIMESTAMP, -- When the event actually happened
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP      -- When your system processed it
    );

redis schema
- just the most recent value of each feature with a timestamp. otherwise we don't know if a value is 1 day vs 1 week old

kafka setup
- batchsize is 5
- flushinterval is 2
- it'll send a batch when it reaches batchsize or the timer finishes

BUGS:
- transformer_engine.go
    - In the loop where I send data to duckdb, if one single insert fails (e.g., bad data), I log it and continue.

files
    - data generation (go)
        - generate json data {entity_id, feature_name, value, validated_at, created_at}
            - this will make like 100 entitiy_id's, 5 features, and cycle through them with new values for each feature
        - kafka (batch)
            - try to keep data in order by id so it's processed in order. something about partitioning kafka topics by id
            - this is the producer file transformer engine is the consumer file.
            - main
        - grpc
            - handle csv, parquet files

    - transformer engine (python)
        - accept kafka json data in batches
        - accept csv, parquet files from grpc
        - spawn python workers to process then send to db's
        - function to send data to duck db
        - function to send data to redis
duck db
    - duckdb appender for data
    - "insert into features select * from 'upload.parquet'" for files
    - feature store
        entity_id, feature_name, value, validated_at, created_at

    - Table Schema
    CREATE TABLE features (
        entity_id text,            -- "user_123"
        feature_name text,         -- "click_count_7d"
        value DOUBLE,              -- 42.0
        event_timestamp TIMESTAMP, -- When the event actually happened
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP      -- When your system processed it
    );

1) make data generation script
    - data sent via kafka to transformer engine
    - files sent over gRPC to transformer engine

2) make tranformer engine
    - python worker pool (global tracking of workers across all services so we don't exceed the max)
    - all data to duck db
    - some data to redis
        - only the last known value of "value" for each feature. this is the current state
        - use a hashmap like: HSET entity:user_123 value 42.0
    - I don't think promethius does anything here. but I should check later
    - we DO NOT vectorize the data. store it raw so it's more flexable for different models

### kafka
**Basics of Kafka**

# kafka broker
- this gets downloaded from online, ideally saved right on your C drive and not program files. 
- tutorials are all mixed up because old kafka used zookeeper, then in 3.x the user could choose btw zookeeper or kraft, now in 4.x it's kraft by default and so misc folders are gone.

**strengths**
    - a good distributed streaming service. not a "mailbox" but rather a "logbook"
    - high throughput (millions/second)
    - scability: easy to expand. if the load gets too high, we just add another server (broker) to the group and kafka CAN load balance (I have to set it up)
    - better durability: it writes messages to teh disk and keeps it there for a set period. other services delete the message after it's read
    - transfers data as bytes
    - data in partitions has strict ordering. it's ordered by like the user id whatever key we set

**weaknesses**
    - complex. rabbitMQ and redis pub/sub are easier
    - latency: it's fast but not real time fast. it's good at high throughput, but has some lag
    - total overkill for small data (too many resources)
    - data order: it guarantees message order only per partition, not across the topic

**vocab**
    - data UUID
        - it's a unique label to id the data. it's 128 bits so it's a really chance anyone ever generates the same uuid
        - these usually get attached to a message as a key or header so we can track it through microservices
    
    - cluster UUID
        - id for whole kafka setup

    - Cluster in context of kafka: 
        - the kafka cluster is kafka itself. it's the set of servers (brokers) that hold data. 
            - the broker is 1 server running kafka. it gets messages, writes them to disk, serves them to readers
            - the cluseter is a group of brokers

    - Topic
        - like a label of a drawer. id for a group. it's just a name

    - partition
        - a folder inside of a drawer. so data needs to choose a topic to go into (drawer), then pick a folder to go into (partition). unlike topics which are just names, a partition is where the data lives on disk
        - it's a subsection of a server/broker

**Best practice note**
- at least once pattern
    - kafka keeps a pointer (called 'offset") of the last message my consumer finishes. if I don't commit then then if I crash and restart kafka will send the same data again. if I commit early before writing to the db's, and crash, I lose the data forever. this is a "at least once" pattern. the cost is 1 network request to kafka; but since we're batching it's not that bad

**Confusing bits about kafka**
    - kafka is famously hard to use
    - during setup you'll likely see errors, but you can likely ignore them. it gives errors for tons of stuff because there's so many config options
    - on startup, it produces a ton of logs. you can't make sense of them all. it's logs for kafka's internals, not your program. for example, if you use a 1 partition system, you'll see startup logs refernceing partition 17 and 51 - those are fine, they're internal

**Example ideal kafka setup**
    - Generate 3 brokers (servers) (a,b,c). broker a gets the data, it copies that data to broker b and c. thus if broker a breaks, broker b takes over, etc.
    - set 1 cluster id (this is the id for the whole kafka setup)
    - set 1 topic
    - set 3 paritions (1,2,3)
    - set replication factor = 3
        - this means all partitions are on all servers
    
    So we have 1 topic, inside the topic are 3 servers, inside each server are 3 paritions
    
    - basic data
        - I have 3000 cars driving around, they send gps data every second
        - truck 101 sends {id: 101, lat: 40.7, lon: -74.0} -> each data point gets it's own UUID (we can use this to track data through our systems). so the producer makes the uuid
            - so uuid + payload is sent to the kafka topic
            - it's a tracking number on a fedEx package
        - the data collector uses the car id (101) as the key, kafka then hashs the id 101, it decides 101 always belongs to parition 1
            - now every message from car 101 always goes to A1. this also ensures data for car 101 is in order
    - storage
        - A is the leader, and writes partition 1 to the disk. B and C are followers (replica's). they copy taht data from A
        - A breaks down: kafka promotes B to leader, it already has a full copy of partition 1
        - the data collector starts sending to B
    - consumer
        - I'm looking at a dashboard app for the cars. I have 3 instances of this app
            - instance 1: connect to kafka. kafka assigns it partition 1
            - instance 2: connect to kafka. it's assigned parition 2
            - instance 3: same but assigned paritition 3
    
    scenarios
        - If I suddenly get 10000 more cars, I can add more partitions and more app instances. each app isntances processes only its parition

**SETUP**

start kafka broker (in cmd)
- https://kafka.apache.org/quickstart
- for go, "go get github.com/segmentio/kafka-go"

- cd into your kafka folder

- make a cluster UUID: bin\windows\kafka-storage.bat random-uuid
    - these are usually the "key" for a partition. 
    - use this to see your UUID's that are keys
        - kafka-console-consumer.sh \
        --bootstrap-server localhost:9092 \
        --topic <your-topic-name> \
        --from-beginning \
        --property print.key=true \
        --property key.separator=" : "
    - might get something like this
    `2025-12-13T00:59:28.266567500Z main ERROR Reconfiguration failed: No configuration found for 'c387f44' at 'null' in 'null'
    [UUID]`
        - that's fine, it worked. the thing at the end is the UUID

- format the storage directories
    - bin\windows\kafka-storage.bat format -t [uuid] -c config\server.properties --standalone
        - what does this do?
            - it's linking the hard drive to a kafka cluster uuid
            - kafka looks at the storage path in the servers file and makes a specific file inside it called meta.properties. in that file it writes that this hard drive belongs to cluster uuid: ...
    - now if we get like "No configuration found for 'c387f44' at 'null' in 'null'" error, it's kafka saying it needs to know how to intitilaize its internal controller quorum for kraft mode. basically it has no idea who's in charge of this cluster (in a distributed system sense).
    - force my 1 node to be the sole leader by using --standalone
    - if you get invalid cluster id, it might mean I have leftover data from a previous attempt

Start the server (broker)
    - bin\windows\kafka-server-start.bat config\server.properties

Make a topic
    - bin\windows\kafka-topics.bat --create --topic [TOPIC] --bootstrap-server localhost:9092 --partitions 1 --replication-factor 1
        - makes 1 partition, 1 server (replication of 1)

Test it's working
    - bin\windows\kafka-console-producer.bat --topic [TOPIC] --bootstrap-server localhost:9092
    - now write some stuff
    - in a THRID terminal run: bin\windows\kafka-console-consumer.bat --topic [TOPIC] --from-beginning --bootstrap-server localhost:9092
        - this should show the messages I wrote

How to stop the server
    - ctrl C in the terminal where it's running
    - run this in code: bin\windows\kafka-server-stop.bat

See if it's running
    - ask the server for a list of topics (in a new terminal)
        - bin\windows\kafka-topics.bat --bootstrap-server localhost:9092 --list


### gRPC details
**misc**

**go to go communication**
- the data generation file starts the config for grpc to send files from go to go
- setup (server)
    - grpc.newserver(): boot it up. this creates a generic grpc engine. it's an internal struct for network connections, and stuff. it doesn't 
know about my stuff or file uploads yet, it's just an empty server waiting for services to be registered
    - file_upload_server := grpc.NewServer(): makes the listener/protocol handler
    - pb.registerFeatureStoreserver(): links my code to the engine. file_upload_server is the generic engine I just made. grpc_server() is my 
custom struct that holds the db connections
    - now we have a engine that has the db connections. this is a SERVER, so I don't pass around a variable for it; the os knows about it. it tells the os to reserve that port and send all traffic that hits it to file_upload_server

    - separate go routine calls runGrpcUploader() (client)
        - every 10 seconds it generates a file that we'll send. but it's using the same port as the server we just made.

    - setup summary
        - startgrpcserver: tells os to listen to port x and direct traffic from there to it (background process)
        - rungrpcuploader: client for that server. sends data to that port


**ideal set up - go to python communication**



# details from design.md

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
