import json
import os
import sys
import time
import uuid
import numpy as np
from scipy.stats import ks_2samp, entropy
import pandas as pd
import redis
import grpc
from concurrent import futures

# Add Proto directory to path so we can import generated code
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), '../../Proto')))
import My_Service_pb2      # if there are import errors for protobuf it might be the ide. try restarting 
import My_Service_pb2_grpc 

# Redis config
REDIS_HOST = 'localhost'
REDIS_PORT = 6379
READ_QUEUE = 'duckdb_read_queue'
WRITE_QUEUE = 'duckdb_write_queue'

MAX_SAMPLE_SIZE = 20000  # Cap on how much data to pull per slice to avoid ram issues

class DBJobQueueClient:
    def __init__(self, redis_client):
        self.redis = redis_client

    def query(self, sql_query):
        """
        Sends a query to the Redis queue and waits for the result.
        Returns a list of dictionaries (rows) or None on error.
        """
        response_key = f"resp:{uuid.uuid4()}"
        
        request = {
            "query": sql_query,
            "response_key": response_key
        }
        
        try:
            # Push request to queue
            self.redis.rpush(READ_QUEUE, json.dumps(request))
            
            # Wait for response (timeout 30s)
            result = self.redis.blpop(response_key, timeout=30)
            
            if not result:
                print(f"Error: Query timed out. Query: {sql_query[:50]}...")
                return None
            
            # result is tuple (key, value)
            response_data = json.loads(result[1])
            
            if 'error' in response_data and response_data['error']:
                print(f"DB Service Error: {response_data['error']}")
                return None
                
            return response_data.get('data')
            
        except Exception as e:
            print(f"Redis/DB Error: {e}")
            return None
        finally:
            # Cleanup key just in case
            self.redis.delete(response_key)

def collect_feature_data(db_client, feature_name):
    """
    Fetches and slices data for a specific feature.
    Logic:
    - Look at total size of features_table table (for this feature).
    - Current Data: Most recent 5%.
    - Historical Data: 50% to 70% slice (sorted by time).
    Returns:
        (hist_df, curr_df) or (None, None) if error/no data
    """
    try:
        # Get total count for this feature to calculate percentages
        count_query = f"SELECT count(*) as count FROM features_table WHERE feature_name = '{feature_name}'"
        count_result = db_client.query(count_query)
        
        if not count_result or not count_result[0].get('count'):
            print(f"    Feature '{feature_name}': No data found (or error).")
            return None, None

        total_count = int(count_result[0]['count'])
        
        if total_count == 0:
            print(f"    Feature '{feature_name}': Zero rows.")
            return None, None

        # Calculate indices (offsets) based on sorted timestamp
        # Historical Window: Start at 50% way into data, End at 70%
        # This means skipping the first 50% of oldest data.
        hist_offset = int(total_count * 0.50)
        hist_count = int(total_count * 0.70) - hist_offset
        
        # Current Window: Most recent 5%
        # This means skipping the first 95% of data.
        curr_offset = int(total_count * 0.95)
        curr_count = total_count - curr_offset
        
        # Apply limits
        if hist_count > MAX_SAMPLE_SIZE:
            hist_count = MAX_SAMPLE_SIZE
            
        if curr_count > MAX_SAMPLE_SIZE:
            curr_count = MAX_SAMPLE_SIZE
            
        print(f"    Feature '{feature_name}': Total Rows {total_count}")
        print(f"      - Historical Slice (50-70%): Offset {hist_offset}, Limit {hist_count}")
        print(f"      - Current Slice (95-100%):   Offset {curr_offset}, Limit {curr_count}")

        # Fetch Historical Data
        hist_query = f"""
            SELECT value, event_timestamp 
            FROM features_table 
            WHERE feature_name = '{feature_name}' 
            ORDER BY event_timestamp ASC 
            LIMIT {hist_count} OFFSET {hist_offset}
        """
        hist_data = db_client.query(hist_query)
        hist_df = pd.DataFrame(hist_data) if hist_data else pd.DataFrame()
        
        # Fetch Current Data
        curr_query = f"""
            SELECT value, event_timestamp 
            FROM features 
            WHERE feature_name = '{feature_name}' 
            ORDER BY event_timestamp ASC 
            LIMIT {curr_count} OFFSET {curr_offset}
        """
        curr_data = db_client.query(curr_query)
        curr_df = pd.DataFrame(curr_data) if curr_data else pd.DataFrame()
        
        print(f"      - Fetched {len(hist_df)} hist rows, {len(curr_df)} curr rows")
        
        return hist_df, curr_df

    except Exception as e:
        print(f"    Error processing feature {feature_name}: {e}")
        return None, None


def calculate_basic_statistics(df):
    """
    Calculates basic statistics (mean, variance) for a dataframe with a 'value' column.
    """
    if df.empty:
        return {'mean': 0, 'variance': 0, 'std': 0}
    
    # Ensure values are numeric
    values = pd.to_numeric(df['value'], errors='coerce').dropna()
    
    if values.empty:
         return {'mean': 0, 'variance': 0, 'std': 0}
         
    return {
        'mean': values.mean(),
        'variance': values.var(),
        'std': values.std()
    }


def calculate_drift_metrics(hist_df, curr_df):
    """
    Calculates KS-Test, PSI, and KL Divergence between historical and current data.
    """
    if hist_df.empty or curr_df.empty:
        return None
        
    hist_values = pd.to_numeric(hist_df['value'], errors='coerce').dropna()
    curr_values = pd.to_numeric(curr_df['value'], errors='coerce').dropna()
    
    if hist_values.empty or curr_values.empty:
        return None
    
    # 1. KS-Test
    ks_stat, ks_p_value = ks_2samp(hist_values, curr_values)
    
    # 2. PSI & KL Divergence Setup
    # Create shared bins
    min_val = min(hist_values.min(), curr_values.min())
    max_val = max(hist_values.max(), curr_values.max())
    
    # Handle constant case
    if min_val == max_val:
        return {
            'ks_stat': ks_stat,
            'ks_p_value': ks_p_value,
            'psi': 0.0,
            'kl_divergence': 0.0
        }

    # 10 bins
    # Using linspace for now (uniform width bins)
    # A more robust approach would use quantiles of historical data
    bins = np.linspace(min_val, max_val, 11)
    
    hist_counts, _ = np.histogram(hist_values, bins=bins)
    curr_counts, _ = np.histogram(curr_values, bins=bins)
    
    # Add epsilon to avoid division by zero
    epsilon = 1e-10
    
    # Normalize to probabilities
    hist_probs = hist_counts / len(hist_values)
    curr_probs = curr_counts / len(curr_values)
    
    # Replace 0 with epsilon
    hist_probs = np.where(hist_probs == 0, epsilon, hist_probs)
    curr_probs = np.where(curr_probs == 0, epsilon, curr_probs)
    
    # PSI
    psi_values = (curr_probs - hist_probs) * np.log(curr_probs / hist_probs)
    psi = np.sum(psi_values)
    
    # KL Divergence (Kullback-Leibler)
    # entropy(pk, qk) computes KL(pk || qk)
    # We calculate KL(Current || Historical)
    kl_div = entropy(curr_probs, hist_probs)
    
    return {
        'ks_stat': ks_stat,
        'ks_p_value': ks_p_value,
        'psi': psi,
        'kl_divergence': kl_div
    }

# sound alarms if metrics are bad
# bad metrics are a bad psi value
# or if ks_p is low and the ks distance is large (this'll prevent alerts on low deviations on massive datasets)
# ks_stat > 0.1 implies the max distance between distributions is > 10%
def sound_the_alarms(drift_metrics):
    psi = drift_metrics['psi']
    ks_p = drift_metrics['ks_p_value']
    
    # first trigger
    if psi > 0.25:
        return "CRITICAL"
    elif psi > 0.1:
        return "WARNING"
        
    # 2. 2nd trigger
    if ks_p < 0.05 and drift_metrics['ks_stat'] > 0.1: 
         return "WARNING"

    return "PASS"

# we can get prod model names from redis, then use those to get prod model metadata from redis
def GetModelMetaData(redis_client):
    all_model_ids = redis_client.smembers('prod_models')
    all_models_metadata = []
    
    for model_id in all_model_ids:
        # key is model_metadata:<model_id>
        meta_key = f"model_metadata:{model_id}"
        meta_json = redis_client.get(meta_key)
        
        if meta_json:
            try:
                meta = json.loads(meta_json)
                all_models_metadata.append(meta)
            except json.JSONDecodeError:
                print(f"  Error parsing metadata JSON for {model_id}")
        else:
            print(f"  Warning: No metadata found for model {model_id}")

    if not all_models_metadata:
        print("No production models found in Redis.")
        return [], []
    
    return list(all_model_ids), all_models_metadata


# analyze 1 feature of 1 model
# we compare historical data to current data and look for strong differences
def AnalyzeFeature(db_client, feature_name):
    hist_df, curr_df = collect_feature_data(db_client, feature_name)
    
    if hist_df is None or curr_df is None or hist_df.empty or curr_df.empty:
        print(f"    Skipping drift calculation for {feature_name} due to insufficient data.")
        return None

    # 1. Calculate Stats
    print(f"    Calculating statistics for {feature_name}...")
    hist_stats = calculate_basic_statistics(hist_df)
    curr_stats = calculate_basic_statistics(curr_df)
    
    print(f"      Hist Mean: {hist_stats['mean']:.4f}, Var: {hist_stats['variance']:.4f}")
    print(f"      Curr Mean: {curr_stats['mean']:.4f}, Var: {curr_stats['variance']:.4f}")
    
    # 2: Calculate Drift
    print(f"    Calculating drift metrics for {feature_name}...")
    drift_metrics = calculate_drift_metrics(hist_df, curr_df)
    
    if drift_metrics:
        print(f"      KS Stat: {drift_metrics['ks_stat']:.4f} (p={drift_metrics['ks_p_value']:.4f})")
        print(f"      PSI: {drift_metrics['psi']:.4f}")
        print(f"      KL Divergence: {drift_metrics['kl_divergence']:.4f}")
        
        status = sound_the_alarms(drift_metrics)
        
        # Create proto object
        metric_proto = My_Service_pb2.FeatureDriftMetric(
            feature_name=feature_name,
            ks_stat=drift_metrics['ks_stat'],
            ks_p_value=drift_metrics['ks_p_value'],
            psi=drift_metrics['psi'],
            kl_divergence=drift_metrics['kl_divergence'],
            status=status
        )
        return metric_proto

    else:
        print("      Could not calculate drift metrics.")
        return None


# Analyze features per model
# model_metadata is for 1 model
def AnalyzeModel(model_metadata, db_client):
    model_id = model_metadata.get('model_id')
    print(f"\nProcessing Model: {model_id}")
    
    expected_features = model_metadata.get('expected_features')
    if not expected_features: 
        print(f"  No expected features defined for {model_id}. Skipping.")
        return None
    
    # Handle string (JSON) representation if it's in that format
    if isinstance(expected_features, str):
        try:
            expected_features = json.loads(expected_features)
        except json.JSONDecodeError:
            print(f"  Error parsing expected_features JSON for {model_id}")
            return None
    
    feature_metrics_list = []
    
    # Check if it's a list (expected)
    if isinstance(expected_features, list):
        # Extract unique feature names
        feature_names = []

        for feature in expected_features:
            if isinstance(feature, dict) and 'name' in feature:
                feature_names.append(feature['name'])
        
        print(f"  Features to check: {feature_names}")
        
        # Process each feature
        for feature_name in feature_names:
            metric = AnalyzeFeature(db_client, feature_name)
            if metric:
                feature_metrics_list.append(metric)

    else:
        print(f"  Unexpected format for expected_features: {type(expected_features)}")
        return None

    # Determine if any feature caused drift
    drift_detected = any(m.status in ["WARNING", "CRITICAL"] for m in feature_metrics_list)
    
    return My_Service_pb2.ModelDriftResult(
        model_id=model_id,
        drift_detected=drift_detected,
        feature_metrics=feature_metrics_list
    )


def EstablishDBConnections():
    redis_client = redis.Redis(host=REDIS_HOST, port=REDIS_PORT, decode_responses=True)
    db_client = DBJobQueueClient(redis_client)

    return redis_client, db_client

def run_drift_analysis():
    """
    Main logic for Service C drift detection. Returns list of ModelDriftResult.
    """
    print("Starting Drift Analysis...")
    model_results = []

    try:
        # 1. get db connections
        print("Establishing db connections...")
        redis_client, db_client = EstablishDBConnections()

        # 2. Get prod model names and their metadata
        print("Fetching production models from Redis...")
        all_model_ids, all_models_metadata = GetModelMetaData(redis_client)
        
        if not all_model_ids:
            return []
        
        # 3. analyze model features
        print("Starting feature drift analysis...")
        for model_metadata in all_models_metadata:
             result = AnalyzeModel(model_metadata, db_client)
             if result:
                 model_results.append(result)
            
    except Exception as e:
        print(f"An error occurred during execution: {e}")
    
    return model_results

# gRPC Service Implementation
class DriftWorker(My_Service_pb2_grpc.PythonWorkerServicer):
    def CalculateDrift(self, request, context):
        print(f"Received CalculateDrift request. Model ID filter: {request.model_id if request.model_id else 'None (All)'}")
        
        # Note: We are currently ignoring request.model_id and analyzing all models
        # because the logic iterates through all prod models. 
        # In a real scenario, we could filter all_models_metadata by request.model_id.
        
        results = run_drift_analysis()
        
        response = My_Service_pb2.DriftResponse(
            model_results=results
        )
        return response

def serve():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    My_Service_pb2_grpc.add_PythonWorkerServicer_to_server(DriftWorker(), server)
    
    # Listen on port 50053 (Service C)
    port = '50053'
    server.add_insecure_port(f'[::]:{port}')
    print(f"Service C gRPC Server started on port {port}")
    server.start()
    try:
        while True:
            time.sleep(86400)
    except KeyboardInterrupt:
        server.stop(0)

if __name__ == '__main__':
    serve()