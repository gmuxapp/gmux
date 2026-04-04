FROM mcr.microsoft.com/devcontainers/base:debian

# Project-specific tools (add your own here)
RUN apt-get update && apt-get install -y --no-install-recommends \
    fish \
    && rm -rf /var/lib/apt/lists/*

# gmux (auto-updated on each container start)
ARG GMUX_VERSION=0.8.0
ADD https://github.com/gmuxapp/gmux/releases/download/v${GMUX_VERSION}/gmux_${GMUX_VERSION}_linux_amd64.tar.gz /tmp/gmux.tar.gz
RUN tar xzf /tmp/gmux.tar.gz -C /usr/local/bin/ gmux gmuxd && rm /tmp/gmux.tar.gz

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 8790

ENTRYPOINT ["entrypoint.sh"]
