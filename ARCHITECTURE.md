```mermaid
graph TD
    %% External Access
    Internet[Internet] --> Cloudflare[Cloudflare]
    Cloudflare --> Cloudflared[Cloudflared<br/>Tunnel]

    %% Remote Access
    Remote[Remote Device] --> Tailscale[Tailscale VPN<br/>100.98.160.1]

    %% Local Access
    LAN[Local Network<br/>192.168.1.x] --> Ethernet[Ethernet<br/>192.168.1.37]

    %% Raspberry Pi
    subgraph RPi["Raspberry Pi 4"]
        Ethernet
        Tailscale
        Docker[Docker Engine]

        subgraph Networks["Docker Networks"]
            Services[Services]
        end

        subgraph Containers["Containers"]
            Traefik[Traefik<br/>Reverse Proxy<br/>Port 80]
            Cloudflared

            subgraph Apps["Web Apps"]
                Homarr[Homarr<br/>Dashboard]
                OpenWebUI[OpenWebUI<br/>AI Chat]
                SearXNG[SearXNG<br/>Search Engine]
                N8N[N8N<br/>Workflow Automation]
                Tududi[Tududi<br/>Task Management]
                Vaultwarden[Vaultwarden<br/>Password Manager]
                Whoami[Whoami<br/>Test Service]
                PocketID[Pocket ID<br/>OIDC Provider]
            end

            subgraph Media["Media Services"]
                Jellyfin[Jellyfin<br/>Media Server]
                subgraph ARR["ARR Stack"]
                    Radarr[Radarr<br/>Movie Manager]
                    Sonarr[Sonarr<br/>TV Show Manager]
                    Lidarr[Lidarr<br/>Music Manager]
                    Bazarr[Bazarr<br/>Subtitles]
                    Prowlarr[Prowlarr<br/>Indexer]
                    Flaresolverr[Flaresolverr<br/>Anti-Captcha]
                end
            end

            subgraph Tools["Tools & Utilities"]
                QBitTorrent[qBittorrent<br/>Torrent Client]
                AdGuardHome[AdGuard Home<br/>DNS Ad Blocker]
                PiHole[Pi-hole<br/>DNS Ad Blocker]
                Dozzle[Dozzle<br/>Log Viewer]
                UptimeKuma[Uptime Kuma<br/>Monitoring]
                Trilium[Trilium<br/>Note Taking]
                Filebrowser[Filebrowser<br/>File Manager]
                it-tools[it-tools<br/>Developer Tools]
                Arcane[Arcane<br/>Unknown]
            end

            subgraph Databases["Databases"]
                Redis[Redis<br/>Cache & DB]
                Qdrant[Qdrant<br/>Vector DB]
            end
        end
    end

    %% Connections
    Ethernet --> Docker
    Tailscale --> Docker
    Docker --> Containers

    Cloudflared --> Containers
    Traefik --> Containers

    Containers --> Apps
    Containers --> Media
    Containers --> Tools
    Containers --> Databases

    %% Traffic Flow
    Cloudflared -.->|Tunnel| Traefik
    Traefik -.->|Routes| Apps
    Traefik -.->|Routes| Media
    Traefik -.->|Routes| Tools
    Traefik -.->|Routes| Databases

    Ethernet -.->|Direct| Containers
    Tailscale -.->|VPN| Containers

    %% Styling
    classDef external fill:#e1f5fe
    classDef network fill:#f3e5f5
    classDef service fill:#e8f5e8
    classDef host fill:#fff3e0

    class Internet,Cloudflare,Remote,LAN external
    class Ethernet,Tailscale,Services network
    class Traefik,Cloudflared,Apps,Media,Tools,Databases service
    class RPi,Docker host

```

## Service Inventory

This section provides a comprehensive list of all deployed services with brief descriptions:

### Core Infrastructure

- **Traefik**: Reverse proxy and load balancer for routing traffic to services.
- **Cloudflared**: Cloudflare tunnel service for secure external access.

### Web Applications

- **Homarr**: Dashboard for organizing and accessing self-hosted services.
- **OpenWebUI**: Web interface for interacting with AI models (integrates with Redis).
- **SearXNG**: Privacy-focused metasearch engine (integrates with Redis).
- **N8N**: Workflow automation and integration platform.
- **Tududi**: Task and project management application.
- **Vaultwarden**: Lightweight password manager compatible with Bitwarden.
- **Whoami**: Simple test service for verifying configurations.
- **Pocket ID**: OIDC provider for passkey-based authentication.

### Media Services

- **Jellyfin**: Media server for streaming movies, TV shows, and music.
- **ARR Stack**:
  - **Radarr**: Automated movie download and management.
  - **Sonarr**: Automated TV show download and management.
  - **Lidarr**: Automated music download and management.
  - **Bazarr**: Subtitle management for media libraries.
  - **Prowlarr**: Indexer manager for torrent and Usenet sources.
  - **Flaresolverr**: Anti-bot service for accessing protected sites.

### Tools & Utilities

- **qBittorrent**: Torrent client for downloading files.
- **AdGuard Home**: Network-wide ad and tracker blocker.
- **Pi-hole**: DNS-based ad blocker and network monitoring.
- **Dozzle**: Real-time Docker container log viewer.
- **Uptime Kuma**: Self-hosted uptime monitoring and alerting.
- **Trilium**: Hierarchical note-taking application.
- **Filebrowser**: Web-based file manager for browsing and managing files on shared storage.
- **it-tools**: Collection of online developer tools for formatting, conversion, and testing.
- **Arcane**: Custom or specialized service (purpose TBD).

### Databases

- **Redis**: In-memory data structure store used for caching and sessions.
- **Qdrant**: Vector database for similarity search and AI applications.

## Docker Compose Structure

The repository is organized using multiple docker-compose files for modularity:

- `docker-compose.yml`: Core services and primary setup.
- `docker-compose.ai.yml`: AI-related services (OpenWebUI, Qdrant).
- `docker-compose.arr.yml`: ARR media management stack.
- `docker-compose.databases.yml`: Database services (Redis, Qdrant).
- `docker-compose.monitor.yml`: Monitoring and logging services (Uptime Kuma, Dozzle).
- `docker-compose.system.yml`: System infrastructure (Traefik, Cloudflared).

Each service has its own directory under `services/` containing its docker-compose.yml and configuration files.

## Maintenance Guidelines

- **Updates**: Revise this document whenever services are added, removed, or modified.
- **Descriptions**: Ensure service descriptions remain accurate and reflect current functionality.
- **Diagram**: Update the Mermaid diagram to include new services or changes in architecture.
- **Versioning**: Consider versioning the document or linking to commit history for tracking changes.
- **Automation**: Explore tools to auto-generate service lists from docker-compose files for consistency.
