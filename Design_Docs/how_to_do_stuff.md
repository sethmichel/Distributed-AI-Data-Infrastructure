
## REDIS

Redis is a key-value store, which means it works differently than relational databases like SQL (Postgres, MySQL).
- **No Tables**: You don't create tables with columns.
- **Keys & Values**: You save data by assigning a value to a unique key. these keys are how we make structure (since there's no tables). we can separate things by colons (e.g., `user:1001:email`).

No standard sql, it's all functions

**Using the redis cli in docker**
docker exec -it redis_store redis-cli   # this just goes in the shell
- **Set a value**: `SET mykey "Hello World"`
- **Get a value**: `GET mykey`
- **Delete a value**: `DEL mykey`
- **Check keys**: `KEYS *` (List all keys)
- **Expire keys**: `SETex session:123 10 "data"` (Sets key `session:123` to "data" that auto-deletes in 10 seconds)

If you want to store a user object like `{id: 1, name: "Seth", role: "admin"}`, you have two common options:
    - A: json string
        - Store the whole object as a JSON string under one key.
            SET user:1 '{"name": "Seth", "role": "admin"}'

    - B: Hash (More like a table row). Use a Redis "Hash" to store fields.
        - HSET user:1 name "Seth" role "admin"

**Using Python**
import redis
import json

host='localhost' # works if running script on host machine
r = redis.Redis(host='localhost', port=6379, decode_responses=True)

1. Simple Key-Value
r.set('thing1', 'thing2')
print(r.get('thing1')) # Output: thing2

2. Storing a "Record" (Hash)
user_id = 101
key = f"user:{user_id}"
r.hset(key, mapping={
    "name": "Alice",
    "email": "alice@example.com",
    "visits": 1
})

Read the "Record"
user = r.hgetall(key)
print(user) 
    - Output: {'name': 'Alice', 'email': 'alice@example.com', 'visits': '1'}


**Using Go**
import (
    "context"
    "fmt"
    "github.com/redis/go-redis/v9"
)

func main() {
    ctx := context.Background()

    // 1. Connect
    rdb := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
        Password: "", // no password set
        DB: 0,  // use default DB
    })

    // 2. Set Key
    err := rdb.Set(ctx, "score", 100, 0).Err()
    if err != nil {
        panic(err)
    }

    // 3. Get Key
    val, err := rdb.Get(ctx, "score").Result()
    if err != nil {
        panic(err)
    }
    fmt.Println("score", val)

    // 4. Working with Hashes ("Tables")
    rdb.HSet(ctx, "user:100", "name", "Bob", "age", 30)
    
    userMap, _ := rdb.HGetAll(ctx, "user:100").Result()
    fmt.Println("User:", userMap)
}

--------------------------------------------------------------

## duck db

DuckDB is a fast in-process analytical database. It works like SQLite (single file) but is optimized for data analysis (OLAP).
- **Files**: The database is just a file (e.g. `Data/Offline_Store.duckdb`).
- **SQL**: You use standard SQL to query it.

**Using Python**
con = duckdb.connect('Data/Offline_Store.duckdb')

con.execute("CREATE TABLE IF NOT EXISTS items (item VARCHAR, value DECIMAL(10,2), count INTEGER)")

con.execute("INSERT INTO items VALUES ('jeans', 20.0, 1), ('hammer', 42.2, 2)")

Query data (returns a list of tuples)
    - print(con.execute("SELECT * FROM items").fetchall())
        - Output: [('jeans', 20.0, 1), ('hammer', 42.2, 2)]

**Pandas integration (DuckDB is great at this)**
df = con.execute("SELECT * FROM items").df()
print(df)


**Using Go**
import (
    "database/sql"
    _ "github.com/marcboeker/go-duckdb/v2"
)

func main() {
    // Open the file
    db, _ := sql.Open("duckdb", "./Data/Offline_Store.duckdb")
    defer db.Close()

    // Query
    rows, _ := db.Query("SELECT * FROM items")
    defer rows.Close()
    
    // ... iterate rows using standard Go SQL ...
}

----------------------------------------------------------

### gRPC

**how do I send the string "do job A" from go in service A to python in service B?**

# 1. Define the Service (.proto)
`Proto/My_Service.proto` looks like this:
syntax = "proto3";
package myservice;
option go_package = "./;myservice";

service MyService {
  rpc SayHello (HelloRequest) returns (HelloReply) {}
}

message HelloRequest {
  string name = 1;
}

message HelloReply {
  string message = 1;
}

# 2. Python Server (Service B)
Create a file `server.py` (or similar) to run the service
import grpc
from concurrent import futures
import sys
import os
import My_Service_pb2
import My_Service_pb2_grpc
sys.path.append(os.path.abspath("Proto"))

class MyServiceHandler(My_Service_pb2_grpc.MyServiceServicer):
    def SayHello(self, request, context):
        print(f"Received request: {request.name}")
        
        # Logic to handle the specific string
        if request.name == "do job A":
            return My_Service_pb2.HelloReply(message="Job A started successfully")
        
        return My_Service_pb2.HelloReply(message=f"Hello {request.name}, I don't know that job.")

def serve():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    My_Service_pb2_grpc.add_MyServiceServicer_to_server(MyServiceHandler(), server)
    
    # Listen on port 50051
    server.add_insecure_port('[::]:50051')
    print("Server starting on port 50051...")
    server.start()
    server.wait_for_termination()

if __name__ == '__main__':
    serve()


#### 3. Go Client (Service A)
In your Go code (e.g., `main.go` or a separate client file), connect to the Python server.

```go
package main

import (
    "context"
    "log"
    "time"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    
    // Import the generated proto package
    // Ensure this matches your module name + path to proto folder
    pb "ai_infra_project/Proto" 
)

func runClient() {
    // 1. Connect to the server
    conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        log.Fatalf("did not connect: %v", err)
    }
    defer conn.Close()

    client := pb.NewMyServiceClient(conn)

    // 2. Call the method
    ctx, cancel := context.WithTimeout(context.Background(), time.Second)
    defer cancel()

    // Sending "do job A"
    r, err := client.SayHello(ctx, &pb.HelloRequest{Name: "do job A"})
    if err != nil {
        log.Fatalf("could not greet: %v", err)
    }

    log.Printf("Response from Python: %s", r.GetMessage())
}
```