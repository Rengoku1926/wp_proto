# Eraser.io Diagram Code

Paste the block below into Eraser.io → New Diagram → Cloud Architecture.

```
direction right
colorMode light

// ─── CLIENTS ─────────────────────────────────────────────────────────────────
group clients [Clients] {
  alice [icon: monitor, label: "Alice"]
  bob [icon: monitor, label: "Bob (offline)"]
}

// ─── BACKEND ─────────────────────────────────────────────────────────────────
group backend [Backend · Go] {
  server [icon: globe, label: "HTTP Server\n:8080"]

  group ws [WebSocket Layer] {
    wsHandler [icon: wifi, label: "WS Handler\nGET /ws"]
    readPump [icon: arrow-down-circle, label: "readPump\n(per user)"]
    writePump [icon: arrow-up-circle, label: "writePump\n(per user)"]
    registry [icon: server, label: "ConnRegistry\nmap[userID]*Client"]
  }

  group api [REST API] {
    userHandler [icon: user, label: "User Handler\nPOST /api/users\nGET /api/users/:id"]
    health [icon: heart, label: "GET /health"]
  }
}

// ─── REDIS ───────────────────────────────────────────────────────────────────
group redis [Redis] {
  pubsub [icon: radio, label: "Pub/Sub\nchat:{userID}"]
  offlineBuffer [icon: layers, label: "Offline Buffer\noffline:{userID}"]
}

// ─── POSTGRES ────────────────────────────────────────────────────────────────
group postgres [PostgreSQL] {
  users [icon: users, label: "users"]
  messages [icon: message-square, label: "messages"]
}

// ─── FLOW ─────────────────────────────────────────────────────────────────────
alice --> wsHandler: WebSocket upgrade
bob --> wsHandler: reconnect + drain buffer

wsHandler --> registry: register Client
wsHandler --> readPump: go readPump()
wsHandler --> writePump: go writePump()

readPump --> messages: INSERT message
readPump --> pubsub: PUBLISH chat:{bobID}
readPump --> offlineBuffer: LPUSH (bob offline)

pubsub --> writePump: message arrives → send chan
offlineBuffer --> writePump: drain on reconnect

writePump --> bob: deliver message

userHandler --> users: INSERT / SELECT
server --> userHandler
server --> wsHandler
server --> health
```
