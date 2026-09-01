ARG GO_VERSION
FROM golang:${GO_VERSION}-bookworm
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages ./packages
# packages/scrollback and cli/gmux replace charmbracelet/x/vt with this
# vendored copy; without it every `go build` in the image dangles.
COPY third_party ./third_party
COPY cli/gmux ./cli/gmux
COPY services/gmuxd ./services/gmuxd
RUN cd services/gmuxd && go build -trimpath -o /opt/gmuxd ./cmd/gmuxd \
    && cd /src/cli/gmux && go build -trimpath -o /opt/gmux ./cmd/gmux
ENV GMUX_PRODUCTION_E2E=1 GMUX_E2E_CONTAINER_GUARD=isolated-v1 GMUXD_E2E_BINARY=/opt/gmuxd GMUX_E2E_BINARY=/opt/gmux
ENTRYPOINT ["/bin/bash","/src/services/gmuxd/tools/production-e2e-inner.sh"]
