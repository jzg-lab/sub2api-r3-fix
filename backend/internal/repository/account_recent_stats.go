package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var recordAccountAttemptScript = redis.NewScript(`
local bucket = math.floor(tonumber(ARGV[1]) / 60)
for _, field in ipairs(redis.call('HKEYS', KEYS[1])) do
  local minuteRaw = string.match(field, '^(%d+):')
  local minute = tonumber(minuteRaw)
  if minute and minute <= bucket - 10 then redis.call('HDEL', KEYS[1], field) end
end
local prefix = tostring(bucket) .. ':'
local success = tonumber(ARGV[2])
redis.call('HINCRBY', KEYS[1], prefix .. 's', success)
redis.call('HINCRBY', KEYS[1], prefix .. 'f', 1 - success)
if success == 1 then
  redis.call('HINCRBY', KEYS[1], prefix .. 'r', ARGV[3])
  redis.call('HINCRBY', KEYS[1], prefix .. 'i', ARGV[4])
  if tonumber(ARGV[5]) >= 0 then
    redis.call('HINCRBY', KEYS[1], prefix .. 't', ARGV[5])
    redis.call('HINCRBY', KEYS[1], prefix .. 'n', 1)
  end
end
local last = tonumber(redis.call('HGET', KEYS[1], 'at') or '0')
if tonumber(ARGV[1]) >= last then
  if success == 1 or tonumber(ARGV[1]) - last >= 60 then
    redis.call('HSET', KEYS[1], 'streak', 1 - success)
  else
    redis.call('HINCRBY', KEYS[1], 'streak', 1)
  end
  redis.call('HSET', KEYS[1], 'at', ARGV[1])
end
redis.call('EXPIRE', KEYS[1], 660)
return 1
`)

func accountRecentStatsKey(id int64) string { return fmt.Sprintf("account:recent:%d", id) }

func (c *gatewayCache) RecordAccountAttempt(ctx context.Context, id int64, observation service.AccountAttemptObservation, now time.Time) error {
	if id <= 0 {
		return nil
	}
	success, ttft := 0, -1
	if observation.Success {
		success = 1
	}
	if observation.FirstTokenMs != nil {
		ttft = max(-1, *observation.FirstTokenMs)
	}
	return recordAccountAttemptScript.Run(ctx, c.rdb, []string{accountRecentStatsKey(id)}, now.Unix(), success, max(0, observation.CacheReadTokens), max(0, observation.InputTokens), ttft).Err()
}

func (c *gatewayCache) GetAccountRecentStats(ctx context.Context, ids []int64, now time.Time) (map[int64]service.AccountRecentStats, error) {
	result := make(map[int64]service.AccountRecentStats, len(ids))
	pipe := c.rdb.Pipeline()
	commands := make(map[int64]*redis.MapStringStringCmd, len(ids))
	for _, id := range ids {
		if id > 0 {
			if _, found := commands[id]; !found {
				commands[id] = pipe.HGetAll(ctx, accountRecentStatsKey(id))
			}
		}
	}
	if len(commands) == 0 {
		return result, nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for id, command := range commands {
		fields := command.Val()
		stat := service.AccountRecentStats{}
		for field, raw := range fields {
			minuteRaw, name, ok := strings.Cut(field, ":")
			if !ok {
				continue
			}
			minute, err := strconv.ParseInt(minuteRaw, 10, 64)
			if err != nil || minute <= now.Unix()/60-10 || minute > now.Unix()/60 {
				continue
			}
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				continue
			}
			switch name {
			case "s":
				stat.Successes += value
			case "f":
				stat.Failures += value
			case "r":
				stat.CacheReadTokens += value
			case "i":
				stat.InputTokens += value
			case "t":
				stat.TTFTSumMs += value
			case "n":
				stat.TTFTCount += value
			}
		}
		if stat.Successes+stat.Failures > 0 {
			stat.LastObservedAt, _ = strconv.ParseInt(fields["at"], 10, 64)
			stat.ConsecutiveFailures, _ = strconv.ParseInt(fields["streak"], 10, 64)
		}
		result[id] = stat
	}
	return result, nil
}
