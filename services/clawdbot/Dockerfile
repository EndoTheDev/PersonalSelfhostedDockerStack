FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends     curl ca-certificates git     && rm -rf /var/lib/apt/lists/*

RUN npm install -g clawdbot@latest

RUN usermod -u 1001 node && groupmod -g 1001 node && chown -R node:node /home/node

USER node
WORKDIR /home/node

EXPOSE 18789

HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3     CMD curl -f http://localhost:18789/ || exit 1

CMD ["clawdbot", "gateway", "--port", "18789", "--bind", "lan"]
