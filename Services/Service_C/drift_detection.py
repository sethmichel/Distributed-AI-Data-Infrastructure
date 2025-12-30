import json
import os
import sys
import time
import uuid
import numpy as np
from scipy.stats import ks_2samp, entropy
import pandas as pd
import redis

# Redis config
REDIS_HOST = 'localhost'
REDIS_PORT = 6379
READ_QUEUE = 'duckdb_read_queue'
WRITE_QUEUE = 'duckdb_write_queue'

MAX_SAMPLE_SIZE = 20000  # Cap on how much data to pull per slice to avoid ram issues

class DBJobQueueClient:
    def __init__(self, host=REDIS_HOST, port=REDIS_PORT):
        self.redis = redis.Redis(host=host, port=port, decode_responses=True)

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

def start_service_c():
    """
    Entry point for Service C drift detection logic.
    Follows steps:
    1. Get production models from model_metadata.
    2. Get expected features for those models.
    3. Slice feature data (Current 5% vs Historical 50-70%).
    """
    print("Starting Service C Drift Detection...")
    
    db_client = DBJobQueueClient()

    try:
        # Step 1: Get production models and their metadata
        print("Fetching production models from model_metadata...")
        
        query = """
            SELECT model_id, expected_features 
            FROM model_metadata 
            WHERE status = 'production'
        """
        models_data = db_client.query(query)
        
        if not models_data:
            print("No production models found or error fetching data.")
            return

        # Convert to DataFrame for easier handling
        models_df = pd.DataFrame(models_data)

        for _, row in models_df.iterrows():
            model_id = row['model_id']
            expected_features = row['expected_features']
            
            print(f"\nProcessing Model: {model_id}")
            
            if not expected_features: 
                print(f"  No expected features defined for {model_id}. Skipping.")
                continue
            
            # Handle string (JSON) representation if DuckDB returns it as such
            if isinstance(expected_features, str):
                try:
                    expected_features = json.loads(expected_features)
                except json.JSONDecodeError:
                    print(f"  Error parsing expected_features JSON for {model_id}")
                    continue
            
            # Check if it's a list (expected)
            if isinstance(expected_features, list):
                # Extract unique feature names
                feature_names = []
                for feat in expected_features:
                    if isinstance(feat, dict) and 'name' in feat:
                        feature_names.append(feat['name'])
                
                print(f"  Features to check: {feature_names}")
                
                # Step 2 & 3: Process each feature
                for feature_name in feature_names:
                    hist_df, curr_df = collect_feature_data(db_client, feature_name)
                    
                    if hist_df is None or curr_df is None or hist_df.empty or curr_df.empty:
                        print(f"    Skipping drift calculation for {feature_name} due to insufficient data.")
                        continue

                    # Step 4: Calculate Stats
                    print(f"    Calculating statistics for {feature_name}...")
                    hist_stats = calculate_basic_statistics(hist_df)
                    curr_stats = calculate_basic_statistics(curr_df)
                    
                    print(f"      Hist Mean: {hist_stats['mean']:.4f}, Var: {hist_stats['variance']:.4f}")
                    print(f"      Curr Mean: {curr_stats['mean']:.4f}, Var: {curr_stats['variance']:.4f}")
                    
                    # Step 5: Calculate Drift
                    print(f"    Calculating drift metrics for {feature_name}...")
                    drift_metrics = calculate_drift_metrics(hist_df, curr_df)
                    
                    if drift_metrics:
                        print(f"      KS Stat: {drift_metrics['ks_stat']:.4f} (p={drift_metrics['ks_p_value']:.4f})")
                        print(f"      PSI: {drift_metrics['psi']:.4f}")
                        print(f"      KL Divergence: {drift_metrics['kl_divergence']:.4f}")
                    else:
                        print("      Could not calculate drift metrics.")
                    
                    result = sound_the_alarms(drift_metrics) # this probably needs to be sent to service d. it'll decide if we need to retrain models

            else:
                print(f"  Unexpected format for expected_features: {type(expected_features)}")
                
    except Exception as e:
        print(f"An error occurred during execution: {e}")
    finally:
        print("\nService C run complete.")

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