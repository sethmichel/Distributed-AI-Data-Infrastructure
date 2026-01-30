import azure.functions as azure_func
import logging
import struct
import json

app = azure_func.FunctionApp()

@app.route(route="drift_analysis", auth_level=azure_func.AuthLevel.FUNCTION)
def drift_analysis(req: azure_func.HttpRequest) -> azure_func.HttpResponse:
    logging.info('Drift analysis function processing request.')

    try:
        body = req.get_body()
        logging.info(f"Received payload of size: {len(body)} bytes")
        
        ''' 
        ---------------------------------------------------------
        DESERIALIZATION LOGIC
        --------------------------------------------------------- 
        this is the azure function code. functions are set up in azure, this code transforms it, sends it, gets it back
        it gets the bytes payload from c, deserializes it, does the stats, returns the json obj to c

        The payload is the VARDATA of the Postgres bytea
        The C struct TDigestState has a 4-byte header (vl_len_) which is NOT sent
        
        C Struct Layout (assuming 64-bit alignment):
        0:  vl_len_ (int32) -> SKIPPED
        4:  count (int)
        8:  capacity (int)
        12: PADDING (4 bytes) -> To align total_weight (double) to 8-byte boundary
        16: total_weight (double)
        24: centroids (array of Centroid)
        
        Received Payload Layout:
        0:  count (int)
        4:  capacity (int)
        8:  PADDING (4 bytes)
        12: total_weight (double)
        20: centroids...
        '''
                
        # Minimum size check (count + cap + pad + weight) = 4+4+4+8 = 20 bytes
        if len(body) < 20:
            return azure_func.HttpResponse(
                json.dumps({"error": "Payload too small"}),
                status_code=400,
                mimetype="application/json"
            )

        # Unpack header
        # < = little endian (standard on x86)
        # i = int (4)
        # i = int (4)
        # 4x = skip 4 bytes (padding)
        # d = double (8)
        try:
            count, capacity, total_weight = struct.unpack('<ii4xd', body[0:20])
        except struct.error as e:
            # Fallback: maybe no padding? (Packed struct)
            # Try unpacking without padding: <iid (4+4+8=16 bytes)
            logging.warning("Failed standard unpack, trying packed format...")
            try:
                count, capacity, total_weight = struct.unpack('<iid', body[0:16])
                # If this worked, adjust offset for centroids
                header_size = 16
            except:
                raise e
        else:
            header_size = 20

        logging.info(f"Header: count={count}, capacity={capacity}, weight={total_weight}")

        # Parse Centroids
        # Each Centroid: mean (double), weight (double) -> 16 bytes
        centroid_size = 16
        centroids = []
        offset = header_size
        
        for i in range(count):
            if offset + centroid_size > len(body):
                logging.warning(f"Payload ended prematurely at centroid {i}")
                break
                
            mean, weight = struct.unpack('<dd', body[offset:offset+centroid_size])
            centroids.append({'mean': mean, 'weight': weight})
            offset += centroid_size

        # ---------------------------------------------------------
        # DRIFT ANALYSIS LOGIC (Placeholder)
        # ---------------------------------------------------------
        # Here you would implement your specific drift detection logic.
        # For now, we return basic statistics.
        
        mean_val = 0
        if total_weight > 0:
            weighted_sum = sum(c['mean'] * c['weight'] for c in centroids)
            mean_val = weighted_sum / total_weight

        result = {
            "status": "success",
            "analyzed_centroids": len(centroids),
            "total_weight": total_weight,
            "calculated_mean": mean_val,
            "drift_detected": False, # TODO: Implement actual logic
            "message": "Analysis complete"
        }

        return azure_func.HttpResponse(
            json.dumps(result),
            mimetype="application/json"
        )

    except Exception as e:
        logging.error(f"Error: {str(e)}")

        return azure_func.HttpResponse(
            json.dumps({"error": str(e)}),
            status_code=500,
            mimetype="application/json"
        )
