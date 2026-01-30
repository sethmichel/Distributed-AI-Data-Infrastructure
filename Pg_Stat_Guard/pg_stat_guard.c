#include "postgres.h"
#include "fmgr.h"
#include "utils/builtins.h"
#include "utils/jsonb.h"
#include "executor/spi.h"
#include "lib/stringinfo.h"

PG_MODULE_MAGIC;

/*
 * Simple T-Digest Implementation (Simplified for Example)
 * In production, use a robust T-Digest library (e.g., from tdigest-c).
 */

typedef struct
{
    double mean;
    double weight;
} Centroid;

typedef struct
{
    int32 vl_len_; /* varlena header (do not touch directly!) */
    int count;
    int capacity;
    double total_weight;
    Centroid centroids[FLEXIBLE_ARRAY_MEMBER];
} TDigestState;

#define TDIGEST_INITIAL_CAPACITY 100

/* Helper to allocate/resize state */
static TDigestState *
create_empty_tdigest(int capacity)
{
    Size size = offsetof(TDigestState, centroids) + capacity * sizeof(Centroid);
    TDigestState *state = (TDigestState *)palloc0(size);
    SET_VARSIZE(state, size);
    state->capacity = capacity;
    state->count = 0;
    state->total_weight = 0;
    return state;
}

PG_FUNCTION_INFO_V1(tdigest_add);
Datum
tdigest_add(PG_FUNCTION_ARGS)
{
    TDigestState *state;
    double value = PG_GETARG_FLOAT8(1);

    if (PG_ARGISNULL(0))
    {
        state = create_empty_tdigest(TDIGEST_INITIAL_CAPACITY);
    }
    else
    {
        state = (TDigestState *)PG_GETARG_POINTER(0);
    }

    /* 
     * Simplified logic: Just add as a centroid with weight 1. 
     * A real T-Digest would merge closest centroids here or lazily.
     */
    if (state->count >= state->capacity)
    {
        /* Expand */
        int new_capacity = state->capacity * 2;
        Size new_size = offsetof(TDigestState, centroids) + new_capacity * sizeof(Centroid);
        state = (TDigestState *)repalloc(state, new_size);
        SET_VARSIZE(state, new_size);
        state->capacity = new_capacity;
    }

    state->centroids[state->count].mean = value;
    state->centroids[state->count].weight = 1.0;
    state->count++;
    state->total_weight += 1.0;

    PG_RETURN_POINTER(state);
}

PG_FUNCTION_INFO_V1(tdigest_combine);
Datum
tdigest_combine(PG_FUNCTION_ARGS)
{
    TDigestState *state1 = PG_ARGISNULL(0) ? NULL : (TDigestState *)PG_GETARG_POINTER(0);
    TDigestState *state2 = PG_ARGISNULL(1) ? NULL : (TDigestState *)PG_GETARG_POINTER(1);

    if (!state1 && !state2) PG_RETURN_NULL();
    if (!state1) PG_RETURN_POINTER(state2);
    if (!state2) PG_RETURN_POINTER(state1);

    /* Merge state2 into state1 */
    int new_count = state1->count + state2->count;
    int new_capacity = state1->capacity;
    
    while (new_capacity < new_count)
        new_capacity *= 2;

    if (new_capacity > state1->capacity)
    {
        Size new_size = offsetof(TDigestState, centroids) + new_capacity * sizeof(Centroid);
        state1 = (TDigestState *)repalloc(state1, new_size);
        SET_VARSIZE(state1, new_size);
        state1->capacity = new_capacity;
    }

    memcpy(&state1->centroids[state1->count], state2->centroids, state2->count * sizeof(Centroid));
    state1->count = new_count;
    state1->total_weight += state2->total_weight;

    /* 
     * Here we should "compress" the t-digest (merge centroids) to keep size bounded.
     * Omitted for brevity in this skeleton.
     */

    PG_RETURN_POINTER(state1);
}

PG_FUNCTION_INFO_V1(tdigest_serialize);
Datum
tdigest_serialize(PG_FUNCTION_ARGS)
{
    /* Since our state is already a varlena compatible struct, we can just return it. */
    TDigestState *state = (TDigestState *)PG_GETARG_POINTER(0);
    
    /* 
     * Ideally, we serialize to a compact binary format for Azure.
     * For now, just return the internal state dump.
     */
     
    /* We must return a bytea. The state is already structurally a bytea (starts with length). */
    PG_RETURN_BYTEA_P(state);
}

PG_FUNCTION_INFO_V1(tdigest_deserialize);
Datum
tdigest_deserialize(PG_FUNCTION_ARGS)
{
    /* deserialize from bytea to internal state */
    bytea *data = PG_GETARG_BYTEA_P(0);
    /* In this simple implementation, they are the same layout */
    PG_RETURN_POINTER(data);
}


PG_FUNCTION_INFO_V1(perform_drift_analysis);
Datum
perform_drift_analysis(PG_FUNCTION_ARGS)
{
    Interval *interval = PG_GETARG_INTERVAL_P(0);
    char *interval_str;
    StringInfoData query_buf;
    int ret;
    Datum result_datum;
    bool is_null;
    
    /* Connect to SPI */
    if ((ret = SPI_connect()) < 0)
        elog(ERROR, "perform_drift_analysis: SPI_connect returned %d", ret);

    /* Construct Query */
    /* 
     * We need to convert the Interval to a string for the query, 
     * or bind it as a parameter. SPI_execute_with_args is better.
     */
    initStringInfo(&query_buf);
    /* 
     * TODO: Replace 'my_table' and 'value' with actual table/column names 
     * passed as args or configured.
     * For now, hardcoded placeholder.
     */
    appendStringInfo(&query_buf, 
        "WITH digest AS ( "
        "  SELECT tdigest_serialize(tdigest_agg_parallel(value)) as payload "
        "  FROM features_table "
        "  WHERE event_timestamp > now() - $1 "
        ") "
        "SELECT content::json "
        "FROM digest, "
        "LATERAL http_post('https://YOUR_AZURE_FUNCTION_URL', payload, 'application/octet-stream')");

    /* Prepare args types */
    Oid argtypes[1] = { INTERVALOID };
    Datum values[1] = { PointerGetDatum(interval) };
    char nulls[1] = { ' ' };

    /* Execute */
    ret = SPI_execute_with_args(query_buf.data, 1, argtypes, values, nulls, true, 0);

    if (ret < 0)
        elog(ERROR, "SPI_execute_with_args returned %d", ret);

    /* Process result */
    if (SPI_processed > 0 && SPI_tuptable != NULL)
    {
        TupleDesc tupdesc = SPI_tuptable->tupdesc;
        SPITupleTable *tuptable = SPI_tuptable;
        HeapTuple tuple = tuptable->vals[0];

        result_datum = SPI_getbinval(tuple, tupdesc, 1, &is_null);
        
        if (is_null)
             result_datum = (Datum)0; // Handle null
        else
             result_datum = SPI_datumTransfer(result_datum, false, -1); // Copy out
    }
    else
    {
        is_null = true;
    }

    SPI_finish();
    pfree(query_buf.data);

    if (is_null)
        PG_RETURN_NULL();
        
    PG_RETURN_DATUM(result_datum);
}
