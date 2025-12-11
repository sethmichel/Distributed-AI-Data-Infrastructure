### Says how to start the project by designing the full setup and configs
idea is do this 1 time correctly, then never again


**duckdb file**
    - stores all features and uploaded files (use duckdb appender)
    - stores a model feature registry. it says the model name and what features the model needs

**redis database in docker**
    - python worker job queue system. python workers will look here to get their next job

**azure blob**
    - stores and sends model artifact files

**Prometheus, Grafana docker containers**
    - prometheus needs an endpoint grafana can scrape. see design.md
    - prometheus is in a container but needs to scrape my local endpoint so I need something like this under prometheus in the yml file, and point to whatever port my go app uses instead of localhost
        extra_hosts:
        - "host.docker.internal:host-gateway" # CRITICAL: Allows Prometheus (inside Docker) to talk to your Go app (on localhost)
    - bash script for a full tear down and rebuild of the containers/servers
    - prometheus.yml: Configures Prometheus to scrape your Go app's /metrics endpoint.
    - grafana_datasources.yaml: Auto-provisions Prometheus as a data source in Grafana so you don't have to do it manually every restart.

**Need a go file that all the duckdb writes go through**
    - every single duckdb write request must go though this

**Need a go file that handles all go routines, load balances the python workers, and restores dead workers/restarts stalled workers**
    - python workers: Set a max_python_workers variable and have a job queue system in redis they slowly go through
    - handle dead/stalled python workers: don't restore data state, just restart the worker with the same request. if a worker hangs and doesn't crash, then my workers can use the check() gRPC method every 5 seconds which I guess will tell me if they're stuck. I guess this means if a worker doesn't check in every 5 seconds then I assume it's stuck

**Need config files**
    - /configs dir
    - docker yml file (prometheus, grafana, redis)
    - env files for azure, duckdb, redis
    - env file loader (load the env files into go structs without exposing any info)
    - app.yaml: general app config
        - server_ports: (Service A gRPC, Service B gRPC, Metrics HTTP).
        - workers: max_python_workers count.
        - paths: Path to local model cache or "Azure" simulation folder.
        - thresholds: Drift score thresholds for alerts.

**requirements.txt**
    - pandas, numpy, scikit-learn, grpcio, redis, duckdb

**Need data generation file going into kafka who then sends it into duck db**
    - spawns 2+ go routines to generate fake user info in json ({ "user_id": 123, "action": "click", "timestamp": ... }) and streamed over kafka
    - spawns 1 go routine which calls the file upload gRPC api endpoint every 10 seconds. this will upload a json file

**Need gRPC api**
    - python workers call 1 function to check that they're still alive. this function should return the trigger to decide if they need to be restarted
    - Data generator script will call a file upload function that sends a file to service A for processing
    - Service B uses 1 function. it gives a model request identifier like user_id, and it needs to get all the features (from duckdb) the ml model will need to process the users request
    - Service D uses a feature drift detection (this will trigger a service C process) function, and a model retrain function. 

-------------------

# Confusing parts

**api.proto folder and gRPC**
- python and go are totally different worlds and can't talk without a middle man. that's gRPC -> api.proto is the directory they both agree to use. it's a blueprint to tellwhat function exist and what data they take using a neutral language like c++. it's called a protobuf language
- since api.proto isn't code, I can't run it. I have to feed this file into a compiler (called a protoc) which generates the actual code for me in the language I need. so,
    - go
        - The compiler reads api.proto and spits out api.pb.go
        - This file contains a Go struct PredictRequest and a Go Interface PredictionServiceServer.
        - I then write the Go code that implements that interface (the actual logic).
    - Python
        - The compiler reads api.proto and spits out api_pb2.py.
        - This file contains a Python class PredictRequest and a Python Class PredictionServiceServicer.
        - I then write the Python code that implements that class.
- why do I have to do it this way?
    - since I want my go server to talk to the python workers, go needs to know how to send the message and python needs to know how to read the message. otherwise the python would get a stream of binary bytes.

**go.mod and go.sum**
- go.mod is like requirements.txt and setup.py combined, but strictly enforced by the language. it tells the dependencies. main.go looks here for the libraries
- go.sum is a security check/lock file. it's cryptographic hashes of the versions of the libraries I use