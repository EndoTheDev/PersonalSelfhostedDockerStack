# Endo's Docker Services Stack

## Description

A comprehensive self-hosted services stack designed for Raspberry Pi 4 deployment, providing media management, productivity tools, monitoring, and secure remote access through a modular Docker Compose setup.

## Features

- **Media Management**: Jellyfin media server with automated downloading via ARR stack (Radarr, Sonarr, Lidarr, Bazarr, Prowlarr)
- **Dashboard**: Homarr for organizing and accessing services
- **AI Integration**: OpenWebUI for AI chat interfaces
- **Productivity**: N8N workflow automation, Tududi task management, Trilium note-taking
- **Security**: Vaultwarden password manager, AdGuard Home and Pi-hole ad blockers
- **Monitoring**: Uptime Kuma and Dozzle for system monitoring and logs
- **File Management**: Filebrowser for web-based file management
- **Search**: SearXNG privacy-focused metasearch engine
- **Torrenting**: qBittorrent client
- **Reverse Proxy**: Traefik with automatic SSL via Cloudflared tunnel
- **Databases**: Redis for caching, Qdrant for vector search

## Prerequisites

- Raspberry Pi 4 (or similar ARM64 device)
- Docker and Docker Compose installed
- Domain name with Cloudflare DNS
- External storage for media files (e.g., mounted HDD)

## Installation

1. Clone the repository:

   ```bash
   git clone <repository-url>
   cd docker-services-stack
   ```

2. Copy the environment file:

   ```bash
   cp .env.example .env
   ```

3. Configure your `.env` file with your domain, Cloudflare tunnel token, and other required variables.

4. Create the Docker network:

   ```bash
   docker network create services
   ```

5. Start the core infrastructure:

   ```bash
   docker-compose -f docker-compose.system.yml up -d
   ```

6. Start additional service groups as needed:

   ```bash
   docker-compose -f docker-compose.databases.yml up -d
   docker-compose -f docker-compose.arr.yml up -d
   docker-compose -f docker-compose.ai.yml up -d
   docker-compose -f docker-compose.monitor.yml up -d
   ```

7. Start the main services:
   ```bash
   docker-compose up -d
   ```

## Configuration

### Environment Variables

Key variables in `.env`:

- `DOMAIN`: Your root domain (e.g., example.com)
- `TUNNEL_TOKEN`: Cloudflare tunnel token from Zero Trust dashboard
- `BASIC_AUTH`: Traefik dashboard authentication (generate with htpasswd)
- Service-specific variables for Tududi, Pi-hole, Redis, etc.

### Volumes

- Media files: Mount external storage to `${STORAGE_PATH}media`
- Downloads: `${STORAGE_PATH}downloads`
- Configs: Stored in each service's `./config` directory

## Usage

Access services via:

- Local network: `http://service-name.local` (configure DNS accordingly)
- Remote: `https://service-name.yourdomain.com` (via Cloudflared tunnel)

### Service URLs

- Homarr Dashboard: `https://homarr.yourdomain.com`
- it-tools: `https://it-tools.yourdomain.com`
- Jellyfin: `https://jellyfin.yourdomain.com`
- Radarr: `https://radarr.yourdomain.com`
- Sonarr: `https://sonarr.yourdomain.com`
- Lidarr: `https://lidarr.yourdomain.com`
- Bazarr: `https://bazarr.yourdomain.com`
- Prowlarr: `https://prowlarr.yourdomain.com`
- qBittorrent: `https://qbittorrent.yourdomain.com`
- OpenWebUI: `https://openwebui.yourdomain.com`
- SearXNG: `https://searxng.yourdomain.com`
- N8N: `https://n8n.yourdomain.com`
- Tududi: `https://tududi.yourdomain.com`
- Vaultwarden: `https://vaultwarden.yourdomain.com`
- AdGuard Home: `https://adguard.yourdomain.com`
- Pi-hole: `https://pihole.yourdomain.com`
- Dozzle: `https://dozzle.yourdomain.com`
- Uptime Kuma: `https://uptime.yourdomain.com`
- Trilium: `https://trilium.yourdomain.com`
- Filebrowser: `https://filebrowser.yourdomain.com`

## Services Overview

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed service inventory and architecture diagram.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Test thoroughly
5. Submit a pull request

Please update ARCHITECTURE.md when adding/removing services.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

Copyright (c) 2024 EndoTheDev

## Acknowledgments

- Thanks to all the open-source projects that make this stack possible
- LinuxServer.io for container images
- Cloudflare for tunneling solutions
