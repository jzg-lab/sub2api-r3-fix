package repository

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const schedulerAccountRevisionPrefix = "sched:acc:v2:revision:"

// Account IDs are never reused by normal creation. A deletion is permanent for
// that ID; an encoding failure only evicts its payload, retaining the revision.
var publishSchedulerAccountScript = redis.NewScript(`
local current = redis.call('GET', KEYS[4])
if current == 'deleted' then
    return -1
end
if ARGV[1] == 'delete' then
    redis.call('SET', KEYS[4], 'deleted')
    redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
    return -1
end
if current and current > ARGV[2] then
    return 0
end
redis.call('SET', KEYS[4], ARGV[2])
if ARGV[1] == 'evict' then
    redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
    return 0
end
redis.call('SET', KEYS[1], ARGV[3])
redis.call('SET', KEYS[2], ARGV[4])
return 1
`)

func schedulerAccountRevision(updatedAt time.Time) (string, error) {
	utc := updatedAt.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return "", fmt.Errorf("account revision is outside the supported time range")
	}
	// Fixed width UTC strings preserve nanosecond ordering without Lua's
	// floating-point precision loss at UnixNano magnitudes.
	return utc.Format("2006-01-02T15:04:05.000000000Z"), nil
}

func schedulerAccountPublicationKeys(id int64) []string {
	key := strconv.FormatInt(id, 10)
	return []string{
		schedulerAccountKey(key), schedulerAccountMetaKey(key),
		schedulerLastUsedKey(key), schedulerAccountRevisionPrefix + key,
	}
}

func (c *schedulerCache) retireAccount(ctx context.Context, accountID int64) error {
	return publishSchedulerAccountScript.Run(ctx, c.rdb,
		schedulerAccountPublicationKeys(accountID), "delete").Err()
}
