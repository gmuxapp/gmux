set shell := ["bash", "-euo", "pipefail", "-c"]

root := justfile_directory()
bin  := root / "bin"

export CGO_ENABLED := "0"
export VERSION     := env_var_or_default("VERSION", "dev")

# Default: full build
default: build

# Full build: frontend → embed → go binaries
build:
    cd {{root}} && pnpm install
    bash {{root}}/scripts/build.sh

# Build Go binaries only (skip frontend)
build-go:
    bash {{root}}/scripts/build.sh --skip-frontend

# Run tests (Go + JS)
test:
    cd {{root}}/services/gmuxd  && go test ./...
    cd {{root}}/cli/gmux        && go test ./...
    cd {{root}}/packages/adapter && go test ./...
    cd {{root}}/apps/gmux-web   && npx vitest run --passWithNoTests

# Start the dev stack (vite + gmuxd + file watcher)
dev:
    bash {{root}}/scripts/dev-server.sh

# Start gmuxd against built binaries
start:
    {{bin}}/gmuxd start

# Install binaries to $(brew --prefix)/bin and restart gmuxd
install:
    #!/usr/bin/env bash
    set -euo pipefail
    prefix=$(brew --prefix)
    cp {{bin}}/gmux "$prefix/bin/gmux"
    cp {{bin}}/gmuxd "$prefix/bin/gmuxd"
    echo "Restarting gmuxd..."
    nohup gmuxd restart >/dev/null 2>&1 &
    echo "Done. gmux and gmuxd installed to $prefix/bin/"
