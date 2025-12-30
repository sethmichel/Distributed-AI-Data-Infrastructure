**Basic design**
1) Service A generates raw json data in this format
	EntityID    string    `json:"entity_id"`
	FeatureName string    `json:"feature_name"`
	Value       float64   `json:"value"`
	ValidatedAt time.Time `json:"validated_at"`
	CreatedAt   time.Time `json:"created_at"`

    and generates json and parquet files. data_generation is the client for this. the grpc server is in data_processing_engine. data_generation sends the file over as binary, data_processing gets the file and sends it to storage

2) it starts a kafka writer which the json data is sent to. there's it's grouped in a batch until it reaches x size or it's been y seconds

3) in 1 go routine it generates the json data, and 1 other go routine it generates a json or parquet file which is sent via a grpc endpoint. all data goes to data_processing_engine.go.

4) all duckdb reads/writes go through a redis queue which is handled by the database service

**connections**
data generation file: kafka reader, grpc client, redis connection
data processing file: kafka writer, grpc server, redis connection

**schemas**
duckdb write format
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

Redis schema
- Key: feature_name. values: value, timestamp
- the most recent value of each feature with a timestamp. otherwise we don't know if a value is 1 day vs 1 week old

**BUGS**
- transformer_engine.go
    - In the loop where I send data to duckdb, if one single insert fails (e.g., bad data), I log it and continue.


--------------------------------------------------------------

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

**KAFKA SETUP**

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



