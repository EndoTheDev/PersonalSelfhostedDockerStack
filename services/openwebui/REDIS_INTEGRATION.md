# OpenWebUI Redis Integration

OpenWebUI has been configured to use Redis for app state and session management.

## Configuration Changes

### Environment Variables
- `REDIS_URL=redis://redis:6379/1` - Connection to Redis database 1

## Benefits

### Session Persistence
- **Chat history survival**: Conversations persist through container restarts
- **User preferences**: Settings, themes, and configurations saved
- **Authentication sessions**: Stay logged in across restarts
- **Model configurations**: Custom model settings persist

### Performance Improvements
- **Faster loading**: Cached app state reduces startup time
- **Instant access**: Previous conversations load immediately
- **Reduced disk I/O**: Frequently accessed data cached in memory

## Redis Database Usage
- **Database 0**: SearXNG cache and rate limiting
- **Database 1**: OpenWebUI app state and sessions (NEW)
- **Database 2**: Available for future services

## Verification

### Check Redis Connection
```bash
# Verify OpenWebUI can connect to Redis
docker logs openwebui | grep -i redis

# Check environment variables
docker exec openwebui printenv | grep REDIS
```

### Monitor Redis Usage
```bash
# View OpenWebUI data in Redis
docker exec -it redis redis-cli
127.0.0.1:6379> SELECT 1
127.0.0.1:6379[1]> KEYS *

# Monitor Redis operations from OpenWebUI
127.0.0.1:6379[1]> MONITOR
```

### Test Session Persistence
1. Start a conversation in OpenWebUI
2. Restart the OpenWebUI container: `docker restart openwebui`
3. Verify conversation history is still available

## Troubleshooting

### Common Issues
1. **Redis connection failed**: Ensure Redis container is running
2. **Session not persisting**: Check Redis URL environment variable
3. **Performance issues**: Monitor Redis memory usage

### Verification Commands
```bash
# Test Redis connectivity from OpenWebUI
docker exec openwebui ping redis

# Check OpenWebUI logs for errors
docker logs openwebui --tail 50 | grep -i error

# Verify Redis database 1 usage
docker exec -it redis redis-cli -n 1 INFO keyspace
```

## Performance Monitoring

### Redis Stats for Database 1
```bash
# Check keyspace info for OpenWebUI database
docker exec -it redis redis-cli INFO keyspace

# Monitor memory usage
docker exec -it redis redis-cli INFO memory | grep used_memory_human
```

### Expected Redis Keys
After using OpenWebUI, you should see keys like:
- Session data: `session:*`
- User preferences: `user:*`
- Chat history: `chat:*`
- Model configurations: `model:*`

## Benefits Realized
- ✅ Conversations survive container restarts
- ✅ User settings persist across sessions
- ✅ Faster app loading and navigation
- ✅ Consistent state management
- ✅ Improved user experience on Pi 4

## Future Enhancements
If needed later, websocket support can be added with:
```yaml
environment:
  - REDIS_URL=redis://redis:6379/1
  - ENABLE_WEBSOCKET_SUPPORT=true
  - WEBSOCKET_MANAGER=redis
  - WEBSOCKET_REDIS_URL=redis://redis:6379/2
