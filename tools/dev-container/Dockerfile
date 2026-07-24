# Runtime image for interactively testing the locally-built gmux 2.0 stack
# (central-store rewrite). The binaries are built on the host by build.sh and
# copied in here — they are statically linked pure-Go (CGO_ENABLED=0, modernc
# sqlite), so no runtime libraries are needed and this stays a slim image.
#
# This is a DEV/TEST image: it pins the local build and does NOT auto-update
# from GitHub releases the way examples/ do. Build+run via compose.yaml.
FROM mcr.microsoft.com/devcontainers/base:debian

# Handy tools for driving sessions inside the container during testing.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq less procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node + the pi coding agent, so the pi adapter is discovered and launchable
# from the UI. Pinned to match the host so the copied auth format matches.
# This layer sits ABOVE the binary COPY below so it stays cached across the
# frequent gmuxd rebuilds.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @earendil-works/pi-coding-agent@0.80.10 \
    && rm -rf /var/lib/apt/lists/*

# Locally-built 2.0 binaries with embedded frontend (produced by ./scripts/build.sh
# on the host; the build context is repo root, see compose.yaml `build.context`).
COPY bin/gmuxd /usr/local/bin/gmuxd
COPY bin/gmux /usr/local/bin/gmux

EXPOSE 8790

# No auto-update: run exactly the binary we copied in.
ENTRYPOINT ["gmuxd", "run"]
