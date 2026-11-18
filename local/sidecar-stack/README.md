# The sidecar's local stack

What §19.11's local tier needs beside `server/local`'s postgres and redis (OPS-M9-01). Today that is one thing,
the SMTP sink: a destination MX that takes every message handed to it and delivers none, so a local smtpedge
has somewhere to send and a test can read what arrived.

| Container | What | Ports |
|---|---|---|
| `sidecar-test-smtp-sink` | `smtpsink/`, this directory's own SMTP server | SMTP `127.0.0.1:52525`, control `127.0.0.1:52526` |

```sh
local/sidecar-stack/stack.sh build    # smtpsink for the docker host, into sidecar-test-smtp-sink:local
local/sidecar-stack/stack.sh up       # starts the sink and waits for its control port
local/sidecar-stack/stack.sh check    # sends it one message, reads it back and forgets it
local/sidecar-stack/stack.sh down     # stops and removes the sink's container and its network
go test ./local/sidecar-stack/smtpsink/
```

## The sink

`smtpsink` speaks enough SMTP for a sending MTA: EHLO and HELO, STARTTLS, MAIL, RCPT, DATA, RSET, NOOP and QUIT.
It has no AUTH, relays nothing and keeps what it takes in memory, at most 1,024 messages of at most 32 MiB,
dropping and counting the oldest past that. Its STARTTLS certificate is a throwaway it makes at each start for
`sink.mail.example`, which nothing chains to, so a sender that checks certificates is told to trust that one
name here and nowhere else. The control port lists what arrived:

| Route | Answer |
|---|---|
| `GET /status` | `{"message_count": n, "dropped_count": n}` |
| `GET /messages` | every kept message, oldest first: `index`, `remote_address`, `helo`, `tls`, `from`, `recipients`, `data` (base64), `received_time` |
| `DELETE /messages` | forgets them |

On SIGTERM it stops accepting, answers 421 to each open session and exits 0 once they have closed; a message
counts as taken only once its DATA was answered 250, so a stop keeps no half message. `smtpsink/main_test.go`
holds that, the limits and each refusal, in process on a loopback port.

It is a test fixture: it holds mail in the clear and is fit for loopback only. The compose file publishes its
ports on 127.0.0.1 alone, runs it as uid 65532 on a read-only root with every capability dropped, and the
image is `FROM scratch` with the one binary built here, so nothing is fetched or pulled.

## What it leaves alone

- `urnetwork-local-pg` and `urnetwork-local-redis`, `server/local`'s stores, are run-local.sh's and are not in
  this file.
- `sidecar/local`'s shared stores are the compose project `sidecar-test-stack`. This file's project and network
  are `sidecar-test-smtp-sink` so that neither file's containers are the other's orphans, and `stack.sh down`
  never passes `--remove-orphans`.

## What waits

- A local smtpedge does not reach the sink yet: its MX port is a setting no config file sets
  (`SmtpEdgeSettings.MxPort`, 25), and FS-42's M9 step, which runs the edge against a sink, is SMTP-M9-19's.
- §19.11's local row also names MinIO, Pebble ACME and a test dns edge. MinIO is `sidecar/local`'s and
  `local/file-cluster-t3`'s; the other two are not in this file.
