# gmuxd REST v1 (draft)

> Source draft migrated from `agent-cockpit` docs; refine during implementation.

Base path: `/v1`

Core endpoints:

- `GET /health`
- `GET /capabilities`
- `GET /sessions`
- `POST /sessions/launch`
- `POST /sessions/{id}/attach`
- `POST /sessions/{id}/kill`
- `POST /sessions/{id}/read`
- `GET /events` (SSE)

Follow-up: formalize request/response JSON examples and error codes after API package bootstrap.
