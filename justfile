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
    #!/usr/bin/env bash
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    exec {{bin}}/gmuxd-${os}-arm64 start

# Install binaries to $(brew --prefix)/bin and restart gmuxd
# Works on macOS (installs darwin-arm64 build); can also be run on Linux.
install:
    #!/usr/bin/env bash
    set -euo pipefail
    goos=$(go env GOOS)
    goarch=$(go env GOARCH)
    prefix=$(brew --prefix 2>/dev/null || echo /usr/local)
    src_gmux="{{bin}}/gmux-${goos}-${goarch}"
    src_gmuxd="{{bin}}/gmuxd-${goos}-${goarch}"
    if [ ! -f "$src_gmux" ] || [ ! -f "$src_gmuxd" ]; then
      echo "Arch-specific binaries not found: $src_gmux / $src_gmuxd"
      echo "Run 'just build' first."
      exit 1
    fi
    cp "$src_gmux"  "$prefix/bin/gmux"
    cp "$src_gmuxd" "$prefix/bin/gmuxd"
    if command -v codesign >/dev/null 2>&1; then
      codesign --sign - --force "$prefix/bin/gmux"
      codesign --sign - --force "$prefix/bin/gmuxd"
    fi
    echo "Restarting gmuxd..."
    nohup gmuxd restart >/dev/null 2>&1 &
    echo "Done. gmux and gmuxd installed to $prefix/bin/"
