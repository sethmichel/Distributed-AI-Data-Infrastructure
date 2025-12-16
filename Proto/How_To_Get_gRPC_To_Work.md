

## How to get gRPC running

This needs to run in a venv. it requires a ton of outdated versions of libraries, it's 100x harder without one.

# Python

1) get grpc protoc here: https://github.com/protocolbuffers/protobuf/releases
    - extract it to program files and put the bin into your system path

2) pip install grpcio grpcio-tools protobuf 
    - grpcio → gRPC runtime
    - grpcio-tools → Python plugin for protoc
    - protobuf → message serialization

3) now make a proto like "My_Service.proto" and write something like this
        syntax = "proto3";

        package myservice;
        option go_package = "./;myservice";

        // The service definition.
        service MyService {
        // Sends a greeting
        rpc SayHello (HelloRequest) returns (HelloReply) {}
        }

        // The request message containing the user's name.
        message HelloRequest {
        string name = 1;
        }

        // The response message containing the greetings.
        message HelloReply {
        string message = 1;
        }
 
4) python -m grpc_tools.protoc --proto_path=. --python_out=. --grpc_python_out=. My_Service.proto
    - that should make 2 files: 
        - myservice_pb2.py
        - myservice_pb2_grpc.py
    - these have message classes, client/server interfaces, stub definitions

5) check with protoc --version
    - should be like libprotoc 33.2

# Go

1) go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
        - those are installed to %USERPROFILE%\go\bin which should be in my path

2) make a proto file, specify the go_package in it (we did this in the python steps)

2) now compile .proto files for go
    - protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative My_Service.proto
        - that should create these
            - myservice.pb.go
            - myservice_grpc.pb.go

# how these files work

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

# Regenerating the Proto Files (if you make changes to My_Service.proto)

After editing My_Service.proto, regenerate all files with these commands:

**For Go:**
```bash
cd Proto
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative My_Service.proto
```

**For Python (in venv):**
```bash
cd Proto
python -m grpc_tools.protoc --proto_path=. --python_out=. --grpc_python_out=. My_Service.proto
```