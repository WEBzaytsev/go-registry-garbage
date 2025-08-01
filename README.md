# Registry-GC Service

**English** | [Русский](README.ru.md)

A microservice for automatic Docker Registry cleanup. The service provides multiple cleanup strategies:
- **Webhook-triggered GC**: Automatically runs garbage collection after manifest deletions
- **Periodic tag pruning**: Removes old tags on schedule, keeping only the latest N versions
- **Manual triggers**: On-demand cleanup via HTTP API

## Features

- 🔄 **Webhook-triggered GC**: Automatically runs garbage collection after manifest deletions
- 📅 **Periodic tag pruning**: Scheduled cleanup with configurable interval (default: 24h)
- 🏷️ **Smart tag management**: Keeps N latest tags, removes older ones with semver-aware sorting
- ⏱️ **Debounce**: Groups multiple deletion events into a single GC run (1 minute delay)
- 🔧 **Multiple triggers**: Manual GC, manual prune+GC, and webhook endpoints
- 🔐 **Selective authentication**: Basic Auth required only for tag pruning, not for GC
- ⚡ **Parallel processing**: Configurable worker count for concurrent tag deletion
- 🛡️ **Graceful shutdown**: Proper signal handling and context cancellation

## Quick Start

### 1. Build Image

```bash
# Build locally
docker build -t registry-gc-listener:latest .

# Or use pre-built image from registry
docker pull r.zaitsv.dev/go-registry-garbage:latest
```

### 2. Docker Compose Configuration

```yaml
version: '3.8'

services:
  registry-ui:
    image: joxit/docker-registry-ui:main
    environment:
      - SINGLE_REGISTRY=true
      - REGISTRY_TITLE=My Registry
      - DELETE_IMAGES=true
      - SHOW_CONTENT_DIGEST=true
      - NGINX_PROXY_PASS_URL=http://registry-server:5000
      - SHOW_CATALOG_NB_TAGS=true
      - CATALOG_MIN_BRANCHES=1
      - CATALOG_MAX_BRANCHES=1
      - TAGLIST_PAGE_SIZE=100
      - REGISTRY_SECURED=false
      - CATALOG_ELEMENTS_LIMIT=1000
    networks: [registry-net]

  registry-server:
    image: registry:2.8.2
    environment:
      REGISTRY_STORAGE_DELETE_ENABLED: "true"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Origin: "[http://registry-ui]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Methods: "[HEAD,GET,OPTIONS,DELETE]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Credentials: "[true]"
      REGISTRY_HTTP_HEADERS_Access-Control-Allow-Headers: "[Authorization,Accept,Cache-Control]"
      REGISTRY_HTTP_HEADERS_Access-Control-Expose-Headers: "[Docker-Content-Digest]"
    volumes:
      - ./registry-data:/var/lib/registry
      - ./registry-config/config.yml:/etc/docker/registry/config.yml:ro
    networks: [registry-net]

  registry-gc:
    image: r.zaitsv.dev/go-registry-garbage:latest
    depends_on: [registry-server]
    environment:
      - REGISTRY_URL=http://registry-server:5000
      - KEEP_N=10
      - WORKERS=8
      # - REGISTRY_USER=admin
      # - REGISTRY_PASS=password
    volumes:
      - ./registry-data:/var/lib/registry
      - ./registry-config/config.yml:/etc/docker/registry/config.yml:ro
    ports:
      - "8080:8080"
    networks: [registry-net]

networks:
  registry-net:
    driver: bridge
```

### 3. Registry Configuration

Create file `registry-config/config.yml`:

```yaml
version: 0.1
log:
  fields:
    service: registry

storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true  # Required for GC to work

http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]

health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3

notifications:
  endpoints:
    - name: gc-listener
      url: http://registry-gc:8080/events
      timeout: 500ms
      threshold: 3
      backoff: 1s
```

## HTTP API

| Method | Path      | Description                 | Response            | Auth Required |
|--------|-----------|-----------------------------|--------------------|---------------|
| POST   | `/events` | Webhook from Docker Registry| `202 Accepted`     | No            |
| POST   | `/gc`     | Manual GC trigger          | `GC started`       | No            |
| POST   | `/prune`  | Manual prune+GC trigger    | `prune+GC started` | Yes*          |

*Authentication is required only for `/prune` endpoint when `REGISTRY_USER`/`REGISTRY_PASS` are set.

### Usage Examples

```bash
# Manual GC trigger (no auth required)
curl -X POST http://localhost:8080/gc

# Manual prune+GC trigger (requires auth if configured)
curl -X POST http://localhost:8080/prune

# Send test webhook event
curl -X POST http://localhost:8080/events \
  -H "Content-Type: application/json" \
  -d '{"events":[{"action":"delete"}]}'

# Check logs
docker logs registry-gc
```

## Environment Variables

| Variable        | Default                         | Description                                 |
|-----------------|--------------------------------|---------------------------------------------|
| `REGISTRY_URL`  | `http://registry-server:5000`  | Docker Registry URL for API calls. Must be accessible from the GC service container |
| `KEEP_N`        | `10`                           | Number of latest tags to keep per repository. Set to 0 to disable tag pruning |
| `PRUNE_INTERVAL`| `24h`                          | How often to run automatic tag pruning. Set to `0` to disable periodic pruning |
| `WORKERS`       | `8`                            | Number of concurrent workers for parallel tag deletion. Higher values = faster cleanup but more load |
| `REGISTRY_USER` | -                              | Username for Basic Auth. Required only for tag pruning operations (optional) |
| `REGISTRY_PASS` | -                              | Password for Basic Auth. Required only for tag pruning operations (optional) |
| `LOG_LEVEL`     | `info`                         | Logging level: `debug`, `info`, `warn`, `error`. GC command output and per-tag deletions are shown only at `debug` |

### Variable Details

- **`REGISTRY_URL`**: The service needs to communicate with Docker Registry API to:
  - Get list of repositories (`/v2/_catalog`)
  - Get tags for each repository (`/v2/{repo}/tags/list`)
  - Get manifest digests (`/v2/{repo}/manifests/{tag}`)
  - Delete manifests (`DELETE /v2/{repo}/manifests/{digest}`)

- **`KEEP_N`**: Controls tag retention policy. For example:
  - `KEEP_N=5` keeps only 5 newest tags per repository
  - `KEEP_N=1` keeps only the latest tag (aggressive cleanup)
  - `KEEP_N=0` disables tag pruning completely

- **`PRUNE_INTERVAL`**: Controls automatic pruning schedule:
  - `PRUNE_INTERVAL=24h` runs pruning every 24 hours
  - `PRUNE_INTERVAL=12h` runs pruning every 12 hours
  - `PRUNE_INTERVAL=0` disables periodic pruning (manual only)

- **`WORKERS`**: Balances cleanup speed vs system load:
  - More workers = faster parallel processing
  - Too many workers may overwhelm the registry
  - Recommended: 4-16 depending on registry performance

- **Authentication**: Required only for tag pruning operations:
  - Set both `REGISTRY_USER` and `REGISTRY_PASS` for Basic Auth
  - Webhook GC works without authentication (uses volume access)
  - Leave empty to disable tag pruning features

### Hardcoded Constants

Some parameters are currently hardcoded in the source code and require rebuilding to change:

- **Debounce time**: `1 minute` - delay after last deletion event before starting GC
- **Registry config path**: `/etc/docker/registry/config.yml` - path to registry configuration file
- **HTTP timeout**: `10 seconds` - timeout for registry API calls
- **Server port**: `:8080` - port for webhook and manual trigger endpoints

To modify these values, edit `main.go` and rebuild the Docker image.

## How It Works

### Webhook-triggered GC
1. **Webhook events**: Registry sends events to `/events` when manifests are deleted
2. **Debounce**: Service waits 1 minute after the last deletion event
3. **Garbage Collection**: Runs `registry garbage-collect --delete-untagged`

### Periodic Tag Pruning
1. **Schedule**: Runs every `PRUNE_INTERVAL` (default: 24h)
2. **Tag cleanup**: Removes old tags, keeping only the latest `KEEP_N` versions
3. **Garbage Collection**: Runs GC after tag cleanup
4. **Authentication**: Requires `REGISTRY_USER`/`REGISTRY_PASS` for API access

### Manual Triggers
- **`/gc`**: Immediate garbage collection (no auth required)
- **`/prune`**: Tag pruning + garbage collection (auth required if configured)

## Tag Sorting Algorithm

The service uses smart tag sorting:
- If tag matches semantic versioning (semver) - sorts by versions
- Otherwise - sorts lexicographically
- Keeps the newest tags, removes old ones

## Logs

Service log examples:

```
INFO[2024-01-15T10:30:00Z] listener started on :8080
INFO[2024-01-15T10:35:00Z] GC scheduled in 1m0s
INFO[2024-01-15T10:36:00Z] ==> prune & GC start
INFO[2024-01-15T10:36:00Z] catalog: 5 repos
INFO[2024-01-15T10:36:01Z] [myapp:v1.0.0] deleted
INFO[2024-01-15T10:36:02Z] [myapp:v1.0.1] deleted
INFO[2024-01-15T10:36:05Z] ==> done in 5.2s
```

## Requirements

- Docker Registry 2.8.2+
- Go 1.24+ (for building)
- Enabled logical deletion in Registry (`storage.delete.enabled: true`)

## License

MIT License