# Redis Service

High-performance in-memory data structure store optimized for ARM64 Raspberry Pi 4.

## Configuration

- **Image**: `redis:7-alpine` (ARM64 compatible)
- **Memory Limit**: 512MB (suitable for Pi 4's 8GB RAM)
- **Persistence**: RDB + AOF for data durability
- **Networks**: Backend only (no external access)

## Features

- Persistent data storage in `./data`
- Custom configuration optimized for ARM64
- Health checks for container monitoring
- Memory management with LRU eviction
- Security hardening (disabled dangerous commands)

## Connection Details

- **Host**: `redis` (container name)
- **Port**: `6379`
- **Database**: 0-15 (16 databases available)

## Services That Can Use Redis

### High Priority
- **SearXNG**: Search result caching, rate limiting
- **N8N**: Workflow queuing, session management
- **Uptime Kuma**: Status caching, notification queuing

### Medium Priority
- **OpenWebUI/Perplexica**: Chat history, API caching
- **Jellyfin**: Metadata and thumbnail caching

## Usage Examples

### Basic Connection (Node.js)
```javascript
const redis = require('redis');
const client = redis.createClient({
  host: 'redis',
  port: 6379
});
```

### Python Connection
```python
import redis
r = redis.Redis(host='redis', port=6379, db=0)
```

### Environment Variables for Services
```env
REDIS_HOST=redis
REDIS_PORT=6379
REDIS_DB=0
```

## Monitoring

Access Redis CLI for debugging:
```bash
docker exec -it redis redis-cli
```

Check Redis info:
```bash
docker exec -it redis redis-cli info
```

## Memory Usage

- Maximum memory: 256MB (configurable in redis.conf)
- Eviction policy: `allkeys-lru` (removes least recently used keys)
- Persistence: RDB snapshots + AOF logging

## Security

- Dangerous commands disabled (FLUSHDB, FLUSHALL, DEBUG)
- CONFIG command renamed for security
- No external network access (backend only)
