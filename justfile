set shell := ["bash", "-euo", "pipefail", "-c"]

root  := justfile_directory()
bin   := root / "bin"
web   := root / "apps/gmux-web"
embed := root / "services/gmuxd/cmd/gmuxd/web"

export CGO_ENABLED := "0"
export VERSION     := env_var_or_default("VERSION", "dev")

# Default: full build
default: build

# Full build: frontend → embed → go binaries
build: build-frontend build-go

# Build the Vite frontend and copy into the go:embed directory
build-frontend:
    cd {{web}} && npx vite build
    rm -rf {{embed}}/assets {{embed}}/favicon.svg {{embed}}/manifest.json \
           {{embed}}/icon-192.png {{embed}}/icon-512.png
    cp -r {{web}}/dist/* {{embed}}/
    @echo "Embedded $(du -sh {{embed}} | cut -f1) of frontend assets"

# Compile gmuxd and gmux
build-go:
    mkdir -p {{bin}}
    cd {{root}}/services/gmuxd && go build -ldflags "-s -w -X main.version=${VERSION}" -o {{bin}}/gmuxd ./cmd/gmuxd
    cd {{root}}/cli/gmux      && go build -ldflags "-s -w -X main.version=${VERSION}" -o {{bin}}/gmux  ./cmd/gmux
    @ls -lh {{bin}}/gmuxd {{bin}}/gmux

# Run tests (Go + JS)
test:
    cd {{root}}/services/gmuxd && go test ./...
    cd {{root}}/cli/gmux       && go test ./...
    cd {{root}}/packages/adapter && go test ./...
    cd {{web}} && npx vitest run --passWithNoTests

# Start gmuxd (requires built binary)
start:
    {{bin}}/gmuxd start

# Start gmuxd with Vite dev server proxied in
dev:
    GMUXD_DEV_PROXY=http://localhost:5173 {{bin}}/gmuxd start &
    cd {{web}} && npx vite
