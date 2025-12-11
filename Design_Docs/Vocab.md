**DAG engine**
- Directed acyclic graph. 
    - directed: things go in 1 direction
    - acyclic: no loops allowed (you can't go back to the start)
    - graph: network of nodes and edges
- A system that does tasks in a dependency graph where:
    - Each node = a computation step
    - Edges = dependencies
    - No cycles allowed
- In ml, dag's are used to handle transformations, track stuff, guarantee ordering, parallel execution...
- ex)
    - cooking recipe: chop onions, heat pan, saute onions (depends on steps a and b). you have to do it in that order

**Kv store**
- Key value store, like a key value db (dynamo db)
- Ml infra needs instant feature retrieval. Warehouses are too slow
- Online store = live real time inference
- Offline store = training, analytics, batch jobs

**RAG**
- Retrieval augmented generation
- A system design pattern where a llm is augmented with external knowledge pulled from a db or vector store at query time
- It's a pipeline:
    - Asks uses a question
    - System converts it into an embedding
    - It gets relevant documents from a vector db
    - Sends the retrieved text + question to the llm
    - Llm returns the grounded answer
- It's because llm's don't have fresh data, hallucinate, and can't store data. So rag makes llm's act like a search engine + llm

**Embeddings in ai context**
- The stuff our ai questions get converted into to feed an ai. It's a high dimension vector

**gRPC**
- It's a better http request at scale. it's function calls over the network, but fast and type safe. the "rpc" is for modern systems (high performance)
- instead of sending json or rest requests like "post/predict { text: hello world}" you send
    rpc Predict(predictRequest) returns (predictResponse)
    Then the client calls "response = model_server.prediuct(request)
    behind the scenes grpc serializes teh request in binary, sends it over http/2, gets a binary response, deserializes back to strongly typed object. so it's like calling a local function but it's a network thing

- It's really fast, strongly typed, bi-directional streaming. So it's ideal for microservices
- In AI (it's the standard):
    - It's the protocol for embedding services
    - Used by shard routers in vector clusters
    - Used by orchestrators
    - Used btw inference servers (token streaming)

- important feature: client side streaming: the client chops a large file into small binary peices and streams them to the server one by one over a single connection. it's really fast and efficient for large data transfers

    - example gRPC usage
        - to get it to work, follow the instructions in Proto/how to get grpc to work.md. this should make a bunch of files
        - my_service.proto (blueprint)
            - this is language neutral, I define what data to send and what functions to call
        - my_service_pb2.py (the data)
            - the python classes for messages. handles serializeation of converting python obj to bytes and deserialization. I import this to create requests and read responses
        - my_service_pb2_grpc.py (connection): 
            - client side (myservicestub), I use this to call the server. it sends data over the network
            - server side: myserviceservicer: I inherit from this class to implment the logic. it handles the plumbing of getting the request and sending teh response
        - my_service_grpc.pb.go
            - go interface for the connection. RegisterMyServiceServer(...) is a function to link your implementation to the gRPC server
        - my_service.pb.go
            - the generated go structs. it lets me use "req := &myservice.HelloRequest{Name: "Seth"}" in my code
    
    - tutorial: IBM -> https://www.youtube.com/watch?v=hVrwuMnCtok
        - scenario: python, java, go microservices. communication btw the services
        - summary: if you have microservices, you have to import something like http handling and maybe http json compression handling in each service. if the services are in different languages then these packages are maintained by different groups. these inconvenciences are answered by gprc which lets you define a schema for what data your send, and what protocols you expose to the services. it sends data as binary so you don't need to import the request/compression libraries in all serivcces. it's also much better in all ways (except cpu usage) at scale. how it works is it sends our protocols/message types to the compiler which outputs source code in the target language -> that's how golang can talk to python.
        - note: python, java, go all have their own http libraries maintained by difference groups
            - this can make problems down the road
        - code generation
            - because it uses protocol buffers. run the proto file against a compiler and it outputs source code in the target language.
            - the protocol buffers send messages as binary which astronomically smaller than rest json. we could use a tool to compress the rest json but we'd need to import it in each microservice in different languages
        - misc details
            - the proto file is basically a schema for data being sent (obj types, data struct), and the procedures we expect to expose. so we say what procedures other microservices can call (these are likley the callable functions)
                - the rpc in grpc is remote procedure calling, which is the proto file schema defining what functions (procedures) to expose so other things can call them



**Vector database**
- Distributed storage engine made for storing embeddings for ai, indexing them for ann search, scaling horizontally, replication and durability

**ANN**
- Approximate nearest neighbor. Find closest vectors to a query vector fast w/o scanning everything
- Exact would be too expensive
- This is the standard; AI retrieval systems, vector databases, and large-scale semantic search. Because nothing else is fast enough, embeddings make a huge work load

**shard/sharding (in vector db context)**
- Sharding = splitting a large db across multiple machines so no 1 machine has to store everything - it's multi machine storage
- In vector db's, a shard stores a subset of the vectors + an index for ANN search
- This is really hard

**Horizontal vs vertical scaling**
- vertical = give 1 machine more power
- Horizontal = add more machines

**Autoscaling workers**
- Auto change the number of workers based on demand

**Canary**
- A canary deployment is a safe rollout strategy. Not a tool, but tools do canary rollouts
- Process:
    - Model A old is live
    - I ship model b new but only direct 1-5% of traffic to it
    - I observe metrics
    - If b is good gradually increase it. Otherwise roll it back

**p95/p00 latency**
- The standard of latency
- They're %'s. 95 is 95% of request return faster than this time, the slowest 5% are above this.

**Feature drift**
- The distribution of features in prod changes a lot compared to what the model saw during training
- Like customer age or price over time (inflation)

**Label drift**
- Labels = The ground truth outputs I predict
- Drift = the distribution of labels changes, even if features stay the same
- ex) fraud detection randomly goes from 0.5% to 3% and stays there for a while

**etl/elt pipeline**
- It's what order we pull data, clean it, and store it. The pipeline to do that.
  - **ETL:**
    - Extract: pull raw data
    - Transform: clean data
    - Load: write the data to wherever
  - **ELT:**
    - Extract: pull data
    - Load: store raw data right away
    - Transform: run transformation logic in the warehouse
    - You do this because warehouses are really fast, scalable and cheap for compute

**Orchestrator**
- In context of like ubers internal orchestrator, lots of companies have a service like this
- It's a category of systems whose job it is to coordinate, schedule, manage, retry, and track distributed tasks
- It's the conductor of an orchestra
- Examples of orchestrators:
    - Kubernetes
    - Netflix conductor
    - Spotify luigi
    - Aws step functions
- It's a logic system

**Redis**
- a key value db
- in memory, key value db with very low latency used for caching, queues, other stuff. RAM based, netwrok accessable data structure server
- pros
    - very fast (ram)
    - data structure server, so not just key/value
    - horizontal scaling
- cons
    - ram is expensive. everythings in memory unless we use redis on flash or a disk  backed variant like redis enterprise
    - not acid complient
    - bad at complex queries (no joins, not relational)
    - writes are more expensive at scale
    - doesn't support windows so needs to run in docker (linux)

**duckdb**
- 'sql for analytics'
- very fast, columnar, vectorized engine. queries can do tens of millions of rows really fast
- it's a library, so a local file
- you can query parquet files directly which is super useful
- cons
    - sucks at writes (but fine for reads). Weak concurrency
    - it's 1 node so it can't scale well
    - bad at always on type stuff
- ideal for 
    feature engineering, ml preprocessing
- note: duckdb appender api can write really fast since it writes right to binary

**Prometheus**
- open source metrics collection/alert system (good at distributed systems). like a time series database for metrics & can compute certain metrics (cpu use, queue size, model drift score, p99). it can hit my apps /metrics endpoint every x seconds
- it's lightweight, integrates with everything, and is really good for most ecosystems

**Grafana**
- visualization/dashbaording tool that sites on top of things like prometheus
- like a ui for dashboards, can query prometheus, loki, postgres... it's a quick way to see metrics
- does not collect metrics, it just queries and shows them

**azure blob storage**
- binary large object. any file. it's unstructured data storage, so think of it as just tossing it in a drawer. accessed by https, or rest api. it's made for web based access. ex) spotify music storage: the db is used to index the data/metadata. the media files are in blob. the db has links to the blob.
    - block: block of data where we manage each block. can have up to 50k blocks per blob
    - append: blocks but opimized for appending new data to blobs
    - page blob: made for frequent, random read/writes
    - tiers
        - hot: frequently accessed
        - cool: not frequently accessed
        - archive: long term storage, slow

    I want cool tier or archive, I guess page blob if available. I won't have enough stuff for blocks