# SearXNG Redis Integration

SearXNG has been configured to use Redis for improved performance and caching.

## Configuration Changes

### Environment Variables
- `SEARXNG_REDIS_URL=redis://redis:6379/0` - Connection to Redis server

### Settings.yml Updates
- **Redis URL**: `redis://redis:6379/0`
- **Autocomplete minimum characters**: 2 (reduces unnecessary queries)
- **Request timeout optimizations**: 3.0s default, 15.0s max
- **Connection pooling**: 100 connections, 20 max pool size
- **HTTP/2 enabled**: Better performance for modern browsers

## Performance Benefits

### Search Result Caching
- Frequently searched terms are cached in Redis
- Reduces load on search engines
- Faster response times for popular queries

### Autocomplete Caching
- Google autocomplete suggestions cached
- Minimum 2 characters before triggering
- Improved responsiveness

### Connection Optimization
- HTTP/2 support for better browser performance
- Connection pooling reduces overhead
- Optimized timeouts for Pi 4 hardware

## Redis Database Usage
- **Database 0**: SearXNG cache and session data
- **Key patterns**: SearXNG uses its own internal key naming

## Monitoring

### Check Redis connection from SearXNG
```bash
docker logs searxng | grep -i redis
```

### View cached data in Redis
```bash
docker exec -it redis redis-cli
127.0.0.1:6379> SELECT 0
127.0.0.1:6379> KEYS searx*
```

### Monitor cache hit rates
```bash
docker exec -it redis redis-cli INFO stats
```

## Troubleshooting

### Common Issues
1. **Redis connection failed**: Ensure Redis container is running
2. **Cache not working**: Check Redis URL in environment variables
3. **Performance issues**: Monitor Redis memory usage

### Verification Commands
```bash
# Test Redis connectivity
docker exec searxng ping redis

# Check SearXNG logs for Redis errors
docker logs searxng --tail 50 | grep -i error

# Verify Redis is receiving connections
docker exec -it redis redis-cli MONITOR
```

## Performance Tuning

### For Heavy Usage
- Increase Redis memory limit in redis.conf
- Adjust connection pool settings
- Monitor cache hit ratios

### For Light Usage
- Current settings are optimized for typical home use
- Redis overhead is minimal on Pi 4

## Cache Invalidation
SearXNG handles cache expiration automatically based on:
- Search result relevance
- Time-based expiration
- Memory pressure (LRU eviction)
