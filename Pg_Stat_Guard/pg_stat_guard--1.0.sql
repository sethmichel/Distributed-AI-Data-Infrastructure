-- complain if script is sourced in psql, rather than via CREATE EXTENSION
\echo Use "CREATE EXTENSION pg_stat_guard" to load this file. \quit

-- T-Digest UDA functions

-- State transition function
CREATE FUNCTION tdigest_add(internal, double precision)
RETURNS internal
AS 'MODULE_PATHNAME', 'tdigest_add'
LANGUAGE C PARALLEL SAFE;

-- Combine function for parallel aggregation
CREATE FUNCTION tdigest_combine(internal, internal)
RETURNS internal
AS 'MODULE_PATHNAME', 'tdigest_combine'
LANGUAGE C PARALLEL SAFE;

-- Serialization function (final function)
CREATE FUNCTION tdigest_serialize(internal)
RETURNS bytea
AS 'MODULE_PATHNAME', 'tdigest_serialize'
LANGUAGE C STRICT PARALLEL SAFE;

-- The Aggregate
CREATE AGGREGATE tdigest_agg (double precision) (
    SFUNC = tdigest_add,
    STYPE = internal,
    COMBINEFUNC = tdigest_combine,
    FINALFUNC = tdigest_serialize,
    SERIALFUNC = tdigest_serialize, -- needed for parallel workers to send state back? No, combinefunc handles it. But serial/deserial might be needed if internal state is complex.
    -- For internal state in parallel workers, we usually need serialization/deserialization if it's not a flat memory block.
    -- If we use internal, we usually need explicit serialization for parallel query.
    -- Let's stick to simple first or use bytea as state if we want to avoid complex serialization hooks.
    -- Using internal is efficient but requires serial/deserial functions for parallel.
    -- Let's use bytea for state to simplify for now, or assume we implement serial/deserial.
    -- Actually, let's define proper deserialization too.
    PARALLEL = SAFE
);

-- We need a deserialize function if we use SERIALFUNC
CREATE FUNCTION tdigest_deserialize(bytea, internal)
RETURNS internal
AS 'MODULE_PATHNAME', 'tdigest_deserialize'
LANGUAGE C STRICT PARALLEL SAFE;

CREATE AGGREGATE tdigest_agg_parallel (double precision) (
    SFUNC = tdigest_add,
    STYPE = internal,
    COMBINEFUNC = tdigest_combine,
    FINALFUNC = tdigest_serialize,
    SERIALFUNC = tdigest_serialize,
    DESERIALFUNC = tdigest_deserialize,
    PARALLEL = SAFE
);

-- Main function to trigger analysis
CREATE FUNCTION perform_drift_analysis(interval)
RETURNS json
AS 'MODULE_PATHNAME', 'perform_drift_analysis'
LANGUAGE C STRICT;
