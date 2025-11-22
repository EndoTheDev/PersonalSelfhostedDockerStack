# Self-Hosted Services Stack

A comprehensive, production-ready self-hosted infrastructure stack built with Docker Compose, featuring media management, AI tools, monitoring, and essential services for a complete home server setup.

## Features

- **Media Ecosystem**: Complete media server with Jellyfin, automated downloading via qBittorrent and the ARR suite (Sonarr, Radarr, Lidarr, Bazarr)
- **AI & Automation**: OpenWebUI for AI chat interfaces and n8n for workflow automation
- **Security & Infrastructure**: Vaultwarden password manager, Pi-hole ad blocking, Traefik reverse proxy, and Cloudflare tunnel
- **Monitoring**: Uptime Kuma for service monitoring and Dozzle for container log viewing
- **Databases**: Redis caching and Qdrant vector database for AI applications
- **Modular Architecture**: Services organized in logical stacks with shared networking

## Prerequisites

- Docker Engine 20.10+
- Docker Compose v2.0+
- At least 4GB RAM (8GB+ recommended)
- Linux/macOS/Windows with Docker support

## Quick Start

1. **Clone the repository**
   ```bash
   git clone <repository-url>
   cd docker-services-stack
   ```

2. **Configure environment**
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   ```

3. **Create external networks**
   ```bash
   docker network create frontend
   docker network create backend
   ```

4. **Start services**
   ```bash
   # Start all services
   docker-compose -f docker-compose.yml -f docker-compose.arr.yml -f docker-compose.databases.yml -f docker-compose.monitor.yml -f docker-compose.system.yml -f docker-compose.ai.yml up -d

   # Or start individual stacks
   docker-compose -f docker-compose.system.yml up -d  # Infrastructure first
   docker-compose -f docker-compose.databases.yml up -d
   docker-compose -f docker-compose.arr.yml up -d
   # etc.
   ```

## Service Categories

### Core Services
- **Website**: Personal website/portfolio
- **Tududi**: Advanced task and project management with areas, recurring tasks, and rich notes
- **Vaultwarden**: Bitwarden-compatible password manager
- **SearXNG**: Privacy-focused metasearch engine

### Media Stack (ARR Suite)
- **qBittorrent**: Torrent client with web UI
- **Flaresolverr**: Anti-captcha service for indexers
- **Prowlarr**: Indexer manager for Sonarr/Radarr
- **Sonarr**: TV show automation and management
- **Radarr**: Movie automation and management
- **Lidarr**: Music automation and management
- **Bazarr**: Subtitle management and downloading
- **Jellyfin**: Media server with transcoding

### AI Services
- **OpenWebUI**: Web interface for AI models with vector search capabilities
- **n8n**: Workflow automation and integration platform

### Databases
- **Redis**: High-performance caching and data store
- **Qdrant**: Vector database for AI embeddings and similarity search

### Monitoring
- **Uptime Kuma**: Multi-protocol uptime monitoring
- **Dozzle**: Real-time container log viewer
- **Whoami**: Simple service for testing connectivity

### System Infrastructure
- **Traefik**: Modern reverse proxy and load balancer
- **Cloudflared**: Secure tunnel to Cloudflare edge
- **Pi-hole**: Network-wide ad blocking via DNS
- **Watchtower**: Automatic container updates

## Configuration

### Environment Variables

Create a `.env` file with the following variables:

```bash
# Cloudflared tunnel token
TUNNEL_TOKEN=your_cloudflare_tunnel_token

# Traefik basic auth (generate with htpasswd)
BASIC_AUTH=admin:$apr1$encrypted_password
```

### Networks

The stack uses two external Docker networks:
- `frontend`: Public-facing services accessible from outside
- `backend`: Internal services for inter-container communication

### Service Configuration

Each service has its own configuration directory under `services/{service-name}/config/`. Refer to individual service documentation for detailed setup.

## Usage

### Accessing Services

Services are typically accessible via Traefik routes. Default domain setup assumes `endothe.dev` - adjust accordingly.

Common service URLs:
- Tududi: `https://tududi.endothe.dev`
- Jellyfin: `https://jellyfin.endothe.dev`
- Vaultwarden: `https://vaultwarden.endothe.dev`
- OpenWebUI: `https://open-webui.endothe.dev`
- Traefik Dashboard: `https://traefik.endothe.dev`

### Common Commands

```bash
# View service status
docker-compose -f docker-compose.yml ps

# View logs
docker-compose -f docker-compose.arr.yml logs -f jellyfin

# Restart a service
docker-compose -f docker-compose.system.yml restart traefik

# Update all containers
docker-compose -f docker-compose.system.yml up -d watchtower

# Stop all services
docker-compose -f docker-compose.yml -f docker-compose.arr.yml -f docker-compose.databases.yml -f docker-compose.monitor.yml -f docker-compose.system.yml -f docker-compose.ai.yml down
```

### Backup Strategy

- Database backups: Configure automated backups for Redis, Qdrant, and service databases
- Media libraries: Regular backups of `/data/media` directories
- Configuration: Backup `services/*/config/` directories
- Environment: Secure backup of `.env` file

## Development

### Project Structure

```
.
├── docker-compose.yml              # Core services
├── docker-compose.arr.yml          # Media stack
├── docker-compose.databases.yml     # Database services
├── docker-compose.monitor.yml       # Monitoring stack
├── docker-compose.system.yml        # Infrastructure
├── docker-compose.ai.yml           # AI services
├── services/                       # Individual service configs
│   ├── jellyfin/
│   ├── traefik/
│   └── ...
├── .env                            # Environment variables
└── README.md
```

### Adding New Services

1. Create service directory: `services/new-service/`
2. Add `docker-compose.yml` with service definition
3. Extend from main compose files if needed
4. Update appropriate stack file (e.g., `docker-compose.system.yml`)

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/new-service`
3. Make changes and test thoroughly
4. Update documentation as needed
5. Submit a pull request

### Guidelines

- Follow Docker Compose best practices
- Include health checks for critical services
- Document any new environment variables
- Test service dependencies and startup order
- Ensure services work with external networks

## License

MIT License - see LICENSE file for details

## Support

- Check service-specific documentation in `services/*/README.md`
- Review Docker Compose logs for troubleshooting
- Ensure proper network connectivity and DNS resolution

---

**Note**: This stack is designed for personal use. Review security implications before exposing services to the internet. Always keep services updated and monitor for security vulnerabilities.