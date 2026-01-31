#include "postgres.h"
#include "fmgr.h"
#include "utils/builtins.h"
#include "utils/jsonb.h"
#include "utils/guc.h"
#include "executor/spi.h"
#include "lib/stringinfo.h"


/*
Remember that this isn't the actual data, it's a tdigest of the data

flow:
1. I run SELECT perform_drift_analysis('1 hour');
2. Postgres spins up parallel workers
3. Each worker scans features_table and calls tdigest_add
4. Workers merge results via tdigest_combine
5. The final binary blob is created via tdigest_serialize
6. The LATERAL http_post sends this blob to your Azure Function
7. The JSON response from Azure is returned to your SQL client

This has a number of new coding techniques. here's a list of some
- spi: executes sql in c
- palloc0: for pg, it's like malloc from c, but it zero's out the allocated memory
- #define: basically a final or const, it's more efficeient than a global. I think define costs 0 memory if I don't 
           use the variable, whereas const always uses memory
- datum: a general datatype. we give pg all data in this type. it's usually with a macro that we call before 
         all functions using sql
         we use this to avoid type incompatability btw c and pg
- pg kind of has a form of garbage collector. it deals with new memory in a context, and deletes that context
   after the query. in C we'd have to call free()
*/

PG_MODULE_MAGIC;  // for binary compatibilty with pg

void _PG_init(void);
static char *azure_function_url = NULL;

/* the azure url is saved in postgres, this code is still needed to tell pg to load it into this c variable
   _PG_init is called when the module is loaded
   We use it to define our custom GUC (Grand Unified Configuration) variable */
void _PG_init(void)
{
    DefineCustomStringVariable("pg_stat_guard.azure_function_url",
                               "URL for the Azure Function",
                               "Set this in postgresql.conf or via ALTER SYSTEM SET",
                               &azure_function_url,
                               NULL,  // this is a fallback if it can't find the url
                               PGC_USERSET,
                               0,
                               NULL,
                               NULL,
                               NULL);
}


// represents a cluster of points in the T-Digest (mean value and weight)
typedef struct
{
    double mean;
    double weight;
} Centroid;

// the state object passed between aggregation steps
typedef struct
{
    int32 vl_len_;  // critical -  It makes the struct compatible with pg's varlena (variable length) type
                    //             system (like text or bytea), allowing it to be safely stored and passed around
    int count;
    int capacity;
    double total_weight;
    Centroid centroids[FLEXIBLE_ARRAY_MEMBER];  // flex array memeber lets the centroids array grow dynamically at the end of the struct
} TDigestState;

static const TDIGEST_INITIAL_CAPACITY 100

// pg has its own memory system (its own malloc called "palloc")
// This is a helper to allocate/resize state
// zero'd memeory. malloc/palloc is ram, and it might have garbage. palloc0 (pg) gets ram and clears garbage from it
static TDigestState *create_empty_tdigest(int capacity)
{
    Size size = offsetof(TDigestState, centroids) + capacity * sizeof(Centroid);
    TDigestState *state = (TDigestState *)palloc0(size);   // palloc0 is for pg. it zeros' out the newly allocated ram
    SET_VARSIZE(state, size);    // sets vl_len_ header, so pg knows how big this chunk of memory is

    state->capacity = capacity;
    state->count = 0;
    state->total_weight = 0;

    return state;
}

/* pg thing. macro denerates a tiny function that tells pg "I'm using version 1 calling convention". pg needs this to
   know how to safly pass args to functions. you do it once for every function to want to expose to sql.
   satum is a pg c extension data type. everything gets passed around as a datum type, a generic data point*/
PG_FUNCTION_INFO_V1(tdigest_add);
Datum
// called for every row TODO: is this right?
// params: state and the new value
tdigest_add(PG_FUNCTION_ARGS)
{
    // if state is null, create i
    // if array is full, use repalloc (pg function) to grow it

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

    /* Simplified logic: Just add as a centroid with weight 1. 
     * A real T-Digest would merge closest centroids here or lazily */
    if (state->count >= state->capacity)
    {
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

// again, pg thing (see first instance)
PG_FUNCTION_INFO_V1(tdigest_combine);
Datum
// merges two states together. this lets pg use parallel workers
tdigest_combine(PG_FUNCTION_ARGS)
{
    TDigestState *state1;
    TDigestState *state2;

    if (PG_ARGISNULL(0)) {
        state1 = NULL;
    }
    else {
        state1 = (TDigestState *)PG_GETARG_POINTER(0);
    }

    if (PG_ARGISNULL(1)) {
        state2 = NULL;
    }
    else {
        state2 = (TDigestState *)PG_GETARG_POINTER(1);
    }

    if (!state1 && !state2) {
        PG_RETURN_NULL();
    } 
    if (!state1) {
        PG_RETURN_POINTER(state2);
    }
    if (!state2) {
        PG_RETURN_POINTER(state1);
    }

    // Merge state2 into state1
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

    /* Here we should "compress" the t-digest (merge centroids) to keep size bounded.
     * Omitted for brevity in this skeleton */

    PG_RETURN_POINTER(state1);
}

// again, pg thing (see first instance)
PG_FUNCTION_INFO_V1(tdigest_serialize);
Datum
// Called at the very end to convert the internal state into the final result (byte array) that gets sent to Azure
tdigest_serialize(PG_FUNCTION_ARGS)
{
    // Since our state is already a varlena compatible struct, we can just return it
    TDigestState *state = (TDigestState *)PG_GETARG_POINTER(0);
    
    /* Ideally, we serialize to a compact binary format for Azure.
     * For now, just return the internal state dump */
     
    // return a bytea. The state is already structurally a bytea (starts with length)
    PG_RETURN_BYTEA_P(state);
}

// again, pg thing (see first instance)
PG_FUNCTION_INFO_V1(tdigest_deserialize);
Datum
tdigest_deserialize(PG_FUNCTION_ARGS)
{
    // deserialize from bytea to internal state
    bytea *data = PG_GETARG_BYTEA_P(0);

    // In this simple implementation, they are the same layout
    PG_RETURN_POINTER(data);
}

// again, pg thing (see first instance)
PG_FUNCTION_INFO_V1(perform_drift_analysis);
Datum
// spi_connect(): connects the code to teh pg executor. this let's us tun queries inside this function
// this is going to build the query using the table, run the aggretate function, sends retults to http_post to azure
// spi_getbinval: extracts the json result returned by the http_post call
// spi_datumtransfer: copies result out of the spi memory context so it survives after spi_finish
perform_drift_analysis(PG_FUNCTION_ARGS)
{
    Interval *interval = PG_GETARG_INTERVAL_P(0);

    // table and column names come from the sql command that triggers this exension. so the user gives them
    text *table_text = PG_GETARG_TEXT_PP(1);                  // get pg text obj from teh function
    char *table_name = text_to_cstring(table_text);           // convert pg obj to c string
    const char *quoted_table = quote_identifier(table_name);  // pg helper that puts "" around the string

    text *col_text = PG_GETARG_TEXT_PP(2);                // get pg column name
    char *col_name = text_to_cstring(col_text);           // convert to c string
    const char *quoted_col = quote_identifier(col_name);  // quotes around c string

    // interval is the time period of data we're getting
    char *interval_str;
    StringInfoData query_buf;
    int ret;
    Datum result_datum;
    bool is_null;
    
    // Check if the Azure URL is set
    if (azure_function_url == NULL || azure_function_url[0] == '\0')
        elog(ERROR, "pg_stat_guard.azure_function_url is not set. Please set it in postgresql.conf using: pg_stat_guard.azure_function_url = 'YOUR_URL'");

    // Connect to SPI (connect to pg execeutor)
    if ((ret = SPI_connect()) < 0)
        elog(ERROR, "perform_drift_analysis: SPI_connect returned %d", ret);

    // create Query 
    /* We need to convert the Interval to a string for the query, 
     * or bind it as a parameter. SPI_execute_with_args is better */
    initStringInfo(&query_buf);

    // query for table column, table name -> send to azure function
    // payload is in bytes
    // we want azure to do the compute and return a json to lateral's join
    appendStringInfo(&query_buf, 
        "WITH digest AS ( "
        "  SELECT tdigest_serialize(tdigest_agg_parallel(%s)) as payload "
        "  FROM %s "
        "  WHERE event_timestamp > now() - $1 "
        ") "
        "SELECT content::json "
        "FROM digest, "
        "LATERAL http_post('%s', payload, 'application/octet-stream')",
        quoted_col,
        quoted_table,
        azure_function_url);

    // Prepare args types
    Oid argtypes[1] = { INTERVALOID };
    Datum values[1] = { PointerGetDatum(interval) };
    char nulls[1] = { ' ' };

    // Execute
    ret = SPI_execute_with_args(query_buf.data, 1, argtypes, values, nulls, true, 0);

    if (ret < 0)
        elog(ERROR, "SPI_execute_with_args returned %d", ret);

    // Process result
    if (SPI_processed > 0 && SPI_tuptable != NULL)
    {
        TupleDesc tupdesc = SPI_tuptable->tupdesc;
        SPITupleTable *tuptable = SPI_tuptable;
        HeapTuple tuple = tuptable->vals[0];

        result_datum = SPI_getbinval(tuple, tupdesc, 1, &is_null);
        
        if (is_null)
             result_datum = (Datum)0;
        else
             result_datum = SPI_datumTransfer(result_datum, false, -1); // Copy out
    }
    
    else {
        is_null = true;
    }

    SPI_finish();
    pfree(query_buf.data);

    if (is_null)
        PG_RETURN_NULL();
        
    PG_RETURN_DATUM(result_datum);
}
