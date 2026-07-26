FROM golang:1.26-bookworm
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages ./packages
COPY cli/gmux ./cli/gmux
COPY tests/e2e ./tests/e2e
COPY services/gmuxd ./services/gmuxd
RUN cd services/gmuxd && go build -trimpath -o /opt/gmuxd ./cmd/gmuxd
ENV GMUX_PRODUCTION_E2E=1 GMUX_E2E_CONTAINER_GUARD=isolated-v1 GMUXD_E2E_BINARY=/opt/gmuxd
ENTRYPOINT ["/bin/bash","/src/services/gmuxd/tools/production-e2e-inner.sh"]
