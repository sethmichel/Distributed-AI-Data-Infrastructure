package Services

import (
    "context"
    "github.com/redis/go-redis/v9"
)

/*
CRITICAL NOTE: this does not give weights to jobs. so service D training might use all workers which is the 
               lowest priority. adding this weight feature is low priority
*/


type GlobalLimiter struct {
    rdb *redis.Client
    maxWorkers int
    key string
}

// TryAcquire attempts to reserve a "CPU slot". Returns true if successful.
func (l *GlobalLimiter) TryAcquire(ctx context.Context) (bool, error) {
    // Lua script to atomically check and increment
    // This prevents race conditions where 2 services check at the exact same time
    script := `
        local current = redis.call("GET", KEYS[1])
        if not current then current = 0 end
        if tonumber(current) < tonumber(ARGV[1]) then
            return redis.call("INCR", KEYS[1])
        else
            return -1
        end
    `
    val, err := l.rdb.Eval(ctx, script, []string{l.key}, l.maxWorkers).Int()
    if err != nil {
        return false, err
    }
    return val != -1, nil
}

func (l *GlobalLimiter) Release(ctx context.Context) {
    l.rdb.Decr(ctx, l.key)
}