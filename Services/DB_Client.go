package Services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// handles interactions with the DB Handler Service via Redis
type DBJobQueueClient struct {
	Redis *redis.Client
}

// creates a reusable client
func NewDBJobQueueClient(redisAddr string) (*DBJobQueueClient, error) {
	r := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := r.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}
	return &DBJobQueueClient{Redis: r}, nil
}

// matches the JSON structure sent by DB_Handler_Service
type responseWrapper struct {
	Data  json.RawMessage `json:"data"`
	Error *string         `json:"error"`
}

// handles the full request/response cycle and unmarshals data into target (usually a pointer to a slice)
// Example usage (serice b would write this code):
//
//	var models []ModelMetadata
//	err := DBServiceClient.Query(ctx, "SELECT ...", &models)
func (c *DBJobQueueClient) Query(ctx context.Context, query string, target interface{}) error {
	// 1. Generate unique key for response
	responseKey := fmt.Sprintf("resp:%d", time.Now().UnixNano())

	// 2. Send Request
	req := ReadRequest{Query: query, ResponseKey: responseKey}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request failed: %w", err)
	}

	if err := c.Redis.RPush(ctx, RedisReadQueue, reqBytes).Err(); err != nil {
		return fmt.Errorf("redis push failed: %w", err)
	}

	// 3. Cleanup: Ensure we don't leave keys if we timeout/crash
	defer c.Redis.Del(context.Background(), responseKey)

	// 4. Wait for Response (Block)
	result, err := c.Redis.BLPop(ctx, 30*time.Second, responseKey).Result()
	if err != nil {
		return fmt.Errorf("db timeout or error: %w", err)
	}
	if len(result) < 2 {
		return fmt.Errorf("empty response from redis")
	}

	// 5. Parse Wrapper
	var wrapper responseWrapper
	if err := json.Unmarshal([]byte(result[1]), &wrapper); err != nil {
		return fmt.Errorf("failed to parse db wrapper: %w", err)
	}

	// 6. Check Database Error
	if wrapper.Error != nil {
		return fmt.Errorf("db service error: %s", *wrapper.Error)
	}

	// 7. Unmarshal Data into User's Struct
	// If no data was returned, we don't touch target
	if len(wrapper.Data) == 0 || string(wrapper.Data) == "null" {
		return nil
	}

	if target != nil {
		if err := json.Unmarshal(wrapper.Data, target); err != nil {
			return fmt.Errorf("failed to unmarshal data into target struct: %w", err)
		}
	}

	return nil
}

// Close closes the underlying redis connection
func (c *DBJobQueueClient) Close() error {
	return c.Redis.Close()
}
