package Services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	config "ai_infra_project/Global_Configs"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/redis/go-redis/v9"
)

// Constants for Queue Names and Batch Settings
const (
	RedisWriteQueue = "duckdb_write_queue"
	RedisReadQueue  = "duckdb_read_queue"
	BatchInterval   = 4 * time.Second
	MaxBatchSize    = 100
)

// handles DuckDB interactions
type DBHandler struct {
	RedisClient *redis.Client
	DuckDB      *sql.DB
	Config      *config.App_Config
	dbLock      sync.Mutex // Serialize access to DuckDB to prevent locking issues
}

// represents a write request
type WriteRequest struct {
	Table string                 `json:"table"`
	Data  map[string]interface{} `json:"data"`
}

// represents a read request
type ReadRequest struct {
	Query       string `json:"query"`
	ResponseKey string `json:"response_key"` // Key to push the result back to
}

// initializes connections and starts processing loops
func StartDBHandler(ctx context.Context, cfg *config.App_Config) error {
	handler := &DBHandler{
		Config: cfg,
	}

	// Connect to Redis
	handler.RedisClient = redis.NewClient(&redis.Options{
		Addr: cfg.Connections.RedisAddr,
	})
	if err := handler.RedisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to redis: %w", err)
	}

	// Connect to DuckDB
	// Using a single connection to avoid locking issues, and enforcing it via mutex
	db, err := sql.Open("duckdb", cfg.Connections.DuckDBPath)
	if err != nil {
		return fmt.Errorf("failed to open duckdb: %w", err)
	}
	// Allow multiple open connections for concurrent reads (go routines).
	// DuckDB handles MVCC (Multi-Version Concurrency Control), this is why we don't manually lock it
	db.SetMaxOpenConns(50)
	handler.DuckDB = db

	log.Println("DBHandler started. Listening for Redis queues...")

	// Start Write Processor (Periodic Batch)
	go handler.processWrites(ctx)

	// Start Read Processor (Continuous Poll)
	go handler.processReads(ctx)

	return nil
}

// handles the Write Queue. It wakes up every BatchInterval to process pending writes
func (h *DBHandler) processWrites(ctx context.Context) {
	ticker := time.NewTicker(BatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.handleWriteBatch(ctx)
		}
	}
}

// handleWriteBatch processes up to MaxBatchSize items from the write queue using a transaction
func (h *DBHandler) handleWriteBatch(ctx context.Context) {
	// 1. Collect requests from Redis
	var requests []WriteRequest

	for i := 0; i < MaxBatchSize; i++ {
		result, err := h.RedisClient.LPop(ctx, RedisWriteQueue).Result()
		if err == redis.Nil {
			break // Queue empty
		} else if err != nil {
			log.Printf("Error popping from write queue: %v", err)
			break
		}

		var req WriteRequest
		if err := json.Unmarshal([]byte(result), &req); err != nil {
			log.Printf("Error unmarshaling write request: %v. Data: %s", err, result)
			continue
		}
		requests = append(requests, req)
	}

	if len(requests) == 0 {
		return
	}

	// 2. Execute Batch in Transaction
	h.dbLock.Lock()
	defer h.dbLock.Unlock()

	tx, err := h.DuckDB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Failed to begin transaction: %v", err)
		// Potential data loss here if we don't handle retry logic,
		// but for now we log error. BUG
		return
	}
	// Ensure rollback if panic or error before commit
	defer tx.Rollback()

	successCount := 0
	for _, req := range requests {
		if err := h.executeWriteTx(ctx, tx, req); err != nil {
			log.Printf("Error executing write to %s: %v", req.Table, err)
			// We continue to try other writes in the batch
		} else {
			successCount++
		}
	}

	if successCount > 0 {
		if err := tx.Commit(); err != nil {
			log.Printf("Failed to commit batch: %v", err)
		} else {
			log.Printf("Committed batch of %d writes", successCount)
		}
	}
}

// executeWriteTx performs the actual DuckDB insertion within a transaction
func (h *DBHandler) executeWriteTx(ctx context.Context, tx *sql.Tx, req WriteRequest) error {
	if len(req.Data) == 0 {
		return nil
	}

	// Construct Insert SQL dynamically
	// Note: This assumes simple key-value mapping to columns.
	var cols []string
	var vals []interface{}
	var placeholders []string

	for k, v := range req.Data {
		cols = append(cols, k)
		vals = append(vals, v)
		placeholders = append(placeholders, "?")
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		req.Table,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "))

	_, err := tx.ExecContext(ctx, query, vals...)
	return err
}

// processReads handles the Read Queue.
func (h *DBHandler) processReads(ctx context.Context) {
	// limit concurrent reads
	maxReads := make(chan struct{}, 50)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// BLPop blocks until an item is available or timeout
			result, err := h.RedisClient.BLPop(ctx, 1*time.Second, RedisReadQueue).Result()
			if err != nil {
				if err != redis.Nil {
					// Timeout is normal behavior for BLPop
				}
				continue
			}

			// BLPop returns [queue_name, value]
			if len(result) < 2 {
				continue
			}
			payload := result[1]

			// Acquire maxReads
			maxReads <- struct{}{}

			go func(p string) {
				defer func() { <-maxReads }() // Release maxReads

				var req ReadRequest
				if err := json.Unmarshal([]byte(p), &req); err != nil {
					log.Printf("Error unmarshaling read request: %v", err)
					return
				}

				h.handleRead(ctx, req)
			}(payload)
		}
	}
}

// does the DuckDB query and pushes results back to Redis
func (h *DBHandler) handleRead(ctx context.Context, req ReadRequest) {
	// Execute Query
	rows, err := h.DuckDB.QueryContext(ctx, req.Query)
	if err != nil {
		h.sendReadResponse(ctx, req.ResponseKey, nil, err)
		return
	}
	defer rows.Close()

	// Parse results dynamically
	cols, err := rows.Columns()
	if err != nil {
		h.sendReadResponse(ctx, req.ResponseKey, nil, err)
		return
	}

	var results []map[string]interface{}

	for rows.Next() {
		// Create a slice of interface{} to hold values
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i := range columns {
			columnPointers[i] = &columns[i]
		}

		if err := rows.Scan(columnPointers...); err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}

		// Create map for this row
		m := make(map[string]interface{})
		for i, colName := range cols {
			val := columnPointers[i].(*interface{})
			// Handle byte arrays (common in DBs) -> string if needed, or keep as is
			if b, ok := (*val).([]byte); ok {
				m[colName] = string(b)
			} else {
				m[colName] = *val
			}
		}
		results = append(results, m)
	}

	h.sendReadResponse(ctx, req.ResponseKey, results, nil)
}

func (h *DBHandler) sendReadResponse(ctx context.Context, key string, data interface{}, err error) {
	resp := map[string]interface{}{
		"data": data,
	}
	if err != nil {
		resp["error"] = err.Error()
	}

	bytes, _ := json.Marshal(resp)

	// Push response to the specified Redis key
	// Using RPUSH to allow for a queue-like response consumption, or just to hold the value.
	// Services should BLPop from this key.
	h.RedisClient.RPush(ctx, key, bytes)
	// Set generic TTL so redis doesn't accumulate garbage if no one reads it
	h.RedisClient.Expire(ctx, key, 5*time.Minute)
}
