# Ollama-Compatible Fallback Proxy (local-first, cloud-fallback, offline cache)

API design matching Ollama's schema, Go service engineering, Redis-backed caching for offline resilience, AWS Bedrock as a cloud fallback showing no vendor lock-in.

**Live demo:** https://ollama.ashanpraba.com

The demo runs entirely in the browser against seeded data — no API keys,
no accounts, and no external services required.

## Stack

- Go
- Redis
- AWS Bedrock
- Docker Compose

## How it works

- Docker-compose up a local Ollama container with a small model (e.g. llama3.2:1b) plus a Redis container.
- Write a Go HTTP server exposing POST /api/generate matching Ollama's JSON request/response shape.
- On each request: try local Ollama first; on timeout/error, call Bedrock via AWS SDK and normalize the response to the same schema.
- Before returning, write the prompt+response to Redis keyed by a hash of the prompt; on total failure (both local and cloud down), serve the cached response and flag it as 'offline mode'.
- Record a 60-90s take: normal request hits local Ollama, kill the container mid-demo to show Bedrock fallback, then kill network access to show Redis-cached offline response.

## Running locally

```bash
cd src
bash run.sh
```

Then open the printed URL. A prebuilt static version of the UI lives in
`src/web/` and can be opened directly with no server.
