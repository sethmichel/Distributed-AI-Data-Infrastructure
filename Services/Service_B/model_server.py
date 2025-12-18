import sys
import os
import redis
import pickle
import grpc
import pandas as pd
from concurrent import futures
from dotenv import load_dotenv

# Add project root to python path so we can import the generated proto files
# Current file is in Services/Service_B/, so we need to go up two levels to get to project root
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), '../../')))

import Proto.My_Service_pb2 as pb2
import Proto.My_Service_pb2_grpc as pb2_grpc

# Load environment variables
load_dotenv(os.path.join(os.path.dirname(__file__), '../../Global_Configs/Env/Azure.env'))

# Configuration
# Default to localhost if not set, but in docker this would be "redis"
REDIS_ADDR = os.getenv("REDIS_ADDR", "localhost:6379")
REDIS_HOST = REDIS_ADDR.split(":")[0]
REDIS_PORT = int(REDIS_ADDR.split(":")[1]) if ":" in REDIS_ADDR else 6379
GRPC_PORT = "50052" # Different port than Service A (50051)

class PythonWorkerService(pb2_grpc.PythonWorkerServicer):
    def __init__(self):
        # Connect to Redis
        try:
            self.redis_client = redis.Redis(host=REDIS_HOST, port=REDIS_PORT, db=0)
            self.redis_client.ping()
            print(f"Connected to Redis at {REDIS_HOST}:{REDIS_PORT}")
        except redis.ConnectionError as e:
            print(f"Failed to connect to Redis: {e}")
            sys.exit(1)

    def RunInference(self, request, context):
        model_id = request.model_name
        # print(f"\n[Received Request] Model: {model_id}, Features: {request.features}")

        try:
            # 1. Fetch Model from Redis
            # Key format: model:<ModelID>:pkl (Assuming extension is always pkl for python models)
            redis_key = f"model:{model_id}:pkl"
            model_bytes = self.redis_client.get(redis_key)

            if not model_bytes:
                return pb2.InferenceResponse(prediction=-1.0, error_message=f"Model {model_id} not found in Redis (key: {redis_key})")

            # 2. Unpickle
            try:
                model = pickle.loads(model_bytes)
            except Exception as e:
                 return pb2.InferenceResponse(prediction=-1.0, error_message=f"Failed to unpickle model: {str(e)}")

            # 3. Prepare Data
            # Convert map<string, string> to a generic Dictionary -> DataFrame
            # We treat everything as a list of 1 row because models expect batch dimensions (usually)
            input_dict = {k: [v] for k, v in request.features.items()}
            
            # Simple conversion: Try to convert numeric strings to floats
            # This is a naive implementation; normally you'd use the model's metadata to strict type
            for k, v_list in input_dict.items():
                try:
                    # check if it's an integer or float
                    val = v_list[0]
                    if "." in val:
                         input_dict[k] = [float(val)]
                    else:
                         try:
                             input_dict[k] = [int(val)]
                         except ValueError:
                             input_dict[k] = [float(val)]
                except ValueError:
                    pass # Keep as string
            
            df = pd.DataFrame(input_dict)

            # 4. Predict
            # Scikit-learn models usually return an array
            try:
                result = model.predict(df)
            except Exception as e:
                 return pb2.InferenceResponse(prediction=-1.0, error_message=f"Prediction failed: {str(e)}")
            
            # Extract single float value (assuming regression or binary class prob)
            try:
                prediction_value = float(result[0])
            except (IndexError, TypeError, ValueError):
                # Handle cases where result might be a scalar or different shape
                prediction_value = float(result)

            return pb2.InferenceResponse(prediction=prediction_value, error_message="")

        except Exception as e:
            print(f"Error during inference: {e}")
            return pb2.InferenceResponse(prediction=-1.0, error_message=str(e))

def serve():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    pb2_grpc.add_PythonWorkerServicer_to_server(PythonWorkerService(), server)
    server.add_insecure_port(f'[::]:{GRPC_PORT}')
    print(f"Python Model Server listening on port {GRPC_PORT}...")
    server.start()
    server.wait_for_termination()

if __name__ == '__main__':
    serve()

