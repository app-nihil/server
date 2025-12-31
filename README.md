# nihil server

Ephemeral encrypted messaging server — zero logs, zero storage.

## What is nihil?

[nihil](https://nihil.app) is an end-to-end encrypted messaging app where messages truly disappear. No database. No backups. No logs. A subpoena would find an empty server.

## Why is this code public?

We claim we don't store your messages. This code proves it.

You can audit this repository to verify:
- No database connections
- No message logging
- No persistent storage of content
- Messages exist only in Redis with automatic TTL expiration
- Server handles encrypted blobs it cannot decrypt

## Architecture

```
┌─────────────────────────────────────────────────┐
│              GO SERVER (this repo)              │
│  ┌───────────────────────────────────────────┐  │
│  │  HTTP/2 + WebSocket Server               │  │
│  │  - REST API (activation, chat mgmt)      │  │
│  │  - WebSocket (real-time messaging)       │  │
│  │  - Handles encrypted blobs only          │  │
│  └───────────────────────────────────────────┘  │
│                      │                          │
│                      ▼                          │
│  ┌───────────────────────────────────────────┐  │
│  │  REDIS (in-memory only)                  │  │
│  │  - Message queue with TTL auto-expire    │  │
│  │  - No persistence of message content     │  │
│  └───────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

## What the server sees

- Encrypted blobs (cannot decrypt)
- Device UUIDs (anonymous, randomly generated)
- Timestamps
- Message sizes

## What the server CANNOT see

- Message content
- Who you are
- Who you're talking to (only anonymous device IDs)

## Key files to audit

| File | What to verify |
|------|----------------|
| `internal/redis/chat.go` | No persistent message storage |
| `internal/redis/client.go` | Redis configuration, TTL settings |
| `internal/websocket/hub.go` | Message routing without logging |
| `internal/api/handlers.go` | No content logging in endpoints |

## License

This code is **source available** for audit purposes only. You may view and verify the code, but you may not use, copy, modify, or run it. See [LICENSE](LICENSE) for details.

## Security

Found a vulnerability? Email security@nihil.app

## Links

- Website: https://nihil.app
- FAQ: https://nihil.app/faq
- Privacy Policy: https://nihil.app/privacy

