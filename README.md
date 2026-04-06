# matterbridge-signal-relay

A lightweight relay that bridges [Signal](https://signal.org) groups to [matterbridge](https://github.com/42wim/matterbridge) via [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api). This enables bidirectional message bridging between Signal and any platform matterbridge supports (Slack, Discord, Telegram, IRC, Matrix, Nextcloud Talk, WhatsApp, and [many more](https://github.com/42wim/matterbridge#features)).

## Why?

Matterbridge has no built-in Signal bridge. The original [signald](https://gitlab.com/signald/signald) project was archived in 2023 and no longer works with Signal's current protocol. This relay fills the gap by connecting signal-cli-rest-api (which does work) to matterbridge's API gateway.

## Architecture

```text
Signal servers
      |
signal-cli-rest-api (:8080)    <-- WebSocket + REST API
      |
matterbridge-signal-relay      <-- this project
      |
matterbridge API gateway (:4242)
      |
Other bridges (Slack, WhatsApp, Nextcloud Talk, etc.)
```

The relay runs two concurrent loops:

- **Signal -> matterbridge**: Connects to signal-cli-rest-api's WebSocket endpoint, receives group messages, and POSTs them to matterbridge's API gateway.
- **Matterbridge -> Signal**: Streams from matterbridge's `/api/stream` endpoint and forwards messages to Signal groups via signal-cli-rest-api's `/v2/send` endpoint.

## Prerequisites

1. **[matterbridge](https://github.com/42wim/matterbridge)** running with an `[api.*]` gateway configured:

   ```toml
   [api.signal]
   BindAddress = "0.0.0.0:4242"
   Buffer = 1000
   ```

   And the API account added as an `[[gateway.inout]]` on your gateways:

   ```toml
   [[gateway]]
   name = "my-bridge"
   enable = true

   [[gateway.inout]]
   account = "slack.myteam"
   channel = "#general"

   [[gateway.inout]]
   account = "api.signal"
   channel = "api"
   ```

2. **[signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api)** running in `json-rpc` mode with a registered phone number.

## Quick start

### 1. Register a Signal phone number

```bash
# Start signal-cli-rest-api
docker run -d --name signal-api \
  -e MODE=json-rpc \
  -v signal-data:/home/.local/share/signal-cli \
  -p 8080:8080 \
  bbernhard/signal-cli-rest-api:latest

# Register (you'll need a CAPTCHA first)
# Visit https://signalcaptchas.org/registration/generate.html
# Solve the CAPTCHA, right-click "Open Signal", copy the link
curl -X POST "http://localhost:8080/v1/register/+15551234567" \
  -H "Content-Type: application/json" \
  -d '{"captcha":"signalcaptcha://signal-hcaptcha...."}'

# Verify with the SMS code you receive
curl -X POST "http://localhost:8080/v1/register/+15551234567/verify/123456"
```

### 2. Get your Signal group IDs

Add your registered number to Signal groups, then send a message in each group to trigger sync:

```bash
curl -s "http://localhost:8080/v1/groups/+15551234567" | jq '.[].id'
```

### 3. Run the relay

```bash
docker run -d --name signal-relay \
  -e SIGNAL_NUMBER="+15551234567" \
  -e SIGNAL_API="http://signal-api:8080" \
  -e MATTERBRIDGE_API="http://matterbridge:4242" \
  -e GATEWAY_MAP="my-bridge=group.ABC123def456..." \
  ghcr.io/junovy-hosting/matterbridge-signal-relay:latest
```

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `SIGNAL_NUMBER` | Yes | | Phone number registered with Signal (e.g. `+15551234567`) |
| `SIGNAL_API` | No | `http://signal-cli-rest-api:8080` | URL of the signal-cli-rest-api instance |
| `MATTERBRIDGE_API` | No | `http://localhost:4242` | URL of the matterbridge API gateway |
| `API_ACCOUNT` | No | `api.signal` | The matterbridge API account name (must match `[api.XXX]` in your matterbridge config) |
| `GATEWAY_MAP` | Yes | | Comma-separated gateway-to-group mappings (see below) |

### GATEWAY_MAP format

Maps matterbridge gateway names to Signal group IDs:

```bash
GATEWAY_MAP="gateway-name=group.ABC123...,other-gateway=group.DEF456..."
```

- **Gateway name**: must match the `name` field in your matterbridge `[[gateway]]` block
- **Group ID**: the Signal group ID from `/v1/groups/<number>` (the `id` field, starts with `group.`)

Multiple mappings are separated by commas.

## Kubernetes deployment

The relay works well as a sidecar container on your matterbridge pod, sharing `localhost` for the API gateway:

```yaml
containers:
  - name: matterbridge
    image: 42wim/matterbridge:latest
    # ...
  - name: signal-relay
    image: ghcr.io/junovy-hosting/matterbridge-signal-relay:latest
    env:
      - name: SIGNAL_NUMBER
        value: "+15551234567"
      - name: SIGNAL_API
        value: "http://signal-cli-rest-api:8080"
      - name: MATTERBRIDGE_API
        value: "http://localhost:4242"
      - name: GATEWAY_MAP
        value: "my-bridge=group.ABC123..."
    livenessProbe:
      httpGet:
        path: /healthz
        port: 8081
      initialDelaySeconds: 5
      periodSeconds: 30
```

## How it handles group ID formats

signal-cli-rest-api uses two different group ID formats:

- **REST API** (`/v1/groups`): `group.XXX...` (base64-encoded, prefixed)
- **WebSocket messages**: raw base64 `internal_id` (no prefix)

The relay automatically resolves both formats at startup by fetching group info from signal-cli-rest-api and building a dual lookup map. You only need to configure the `group.XXX` format in `GATEWAY_MAP`.

## Health check

The relay exposes a health endpoint at `:8081/healthz` for liveness probes.

## Building from source

```bash
go build -o matterbridge-signal-relay .
```

Or with Docker:

```bash
docker build -t matterbridge-signal-relay .
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
