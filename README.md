# reddit-rss

Simple server to use reddit api and return RSS feeds.

Reddit is retiring its `.rss` endpoints. This serves the same URLs, backed by
the official Reddit API, so feed readers only need the hostname changed:

```
https://www.reddit.com/r/selfhosted/controversial/.rss
https://the-new-reddit-rss.com/r/selfhosted/controversial/.rss
```

## Endpoints

| URL | Feed |
| --- | --- |
| `/r/{sub}/.rss` | hot posts |
| `/r/{sub}/new/.rss` | new posts |
| `/r/{sub}/top/.rss` | top posts |
| `/r/{sub}/controversial/.rss` | controversial posts |
| `/r/{sub}/rising/.rss` | rising posts |
| `/r/{sub}/comments/.rss` | newest comments in the subreddit |
| `/r/{sub}/comments/{post id}/.rss` | a post followed by its comments |
| `/user/{name}/.rss` | a user's posts and comments (`/u/{name}/.rss` works too) |
| `/user/{name}/submitted/.rss` | a user's posts |
| `/user/{name}/comments/.rss` | a user's comments |

- `.rss` returns **RSS 2.0**, `.atom` returns **Atom 1.0**.
- `/r/{sub}.rss` and `/r/{sub}/new.rss` (no slash before the extension) work too, as on Reddit.
- Several subreddits at once: `/r/linux+golang/new/.rss`.
- A post's comment feed also accepts its full Reddit URL, with the title slug:
  `/r/selfhosted/comments/1abc2de/some_title/.rss`.
- `?sort=` on user feeds (`new`, `hot`, `top`, `controversial`) and comment
  threads (`confidence`, `top`, `new`, `controversial`, `old`, `qa`).
- `?t=hour|day|week|month|year|all` with the `top` and `controversial` sorts.
- `?limit=1..100` (default 25).
- `/healthz` returns `ok`.

Entries look like Reddit's own: the self text (or the thumbnail for link
posts), then "submitted by /u/x [link] [comments]". Comments are titled
"/u/x on <post title>" and contain the comment text. A comment thread feed
holds the comments Reddit returns in one page, flattened depth first; the
"load more comments" ones are not fetched.

## Caching and Reddit's rate limit

- Every listing is cached for `CACHE_TTL` (15 minutes). `.rss` and `.atom` of
  the same listing share one cache entry, and subreddit names are case-insensitive.
- Concurrent requests for an uncached feed make a single call to Reddit.
- Subreddit details (title, description, icon) are cached for 24 hours.
- Unknown, private and banned subreddits are cached as errors too, so made-up
  names cannot be used to hammer Reddit.
- If Reddit fails, the last good copy is served (for up to 6 hours) and
  Reddit is retried a minute later.
- Calls to Reddit are spaced at least `MIN_REQUEST_INTERVAL` apart (1s), and
  are held back when Reddit's `X-Ratelimit-Remaining` hits 0.
- Responses carry `Cache-Control`, `ETag` and `Last-Modified`, and answer
  conditional requests with `304 Not Modified`.

## Setup

1. Create an app at <https://www.reddit.com/prefs/apps> ("script" type is fine).
2. Copy `.env.example` to `.env` and fill in `REDDIT_CLIENT_ID` and
   `REDDIT_CLIENT_SECRET`. A username and password are optional:
   without them the app uses application-only auth, which is enough for public subreddits.
3. Build and run:

```sh
go build -o reddit-rss .
set -a; . ./.env; set +a
./reddit-rss
curl http://localhost:8080/r/selfhosted/new/.rss
```

Or install `reddit-rss.service` as a systemd unit (it reads `.env`).

### Docker Compose

Every push builds a multi-arch (amd64/arm64) image and publishes it to GitHub
Container Registry as `ghcr.io/ricardo-duarte-av/reddit-rss`, tagged with the
branch name, `sha-<commit>`, and `latest` for `main`.

1. Put `docker-compose.yaml` and `.env.example` in a directory.
2. Create `.env` and fill in at least `REDDIT_CLIENT_ID` and `REDDIT_CLIENT_SECRET`:

   ```sh
   cp .env.example .env
   chmod 600 .env   # it holds your Reddit secret
   ```

   Compose passes every variable in `.env` to the container. Use plain
   `KEY=value` lines (no `export`), and quote values with spaces, like
   `REDDIT_USER_AGENT="linux:reddit-rss:v1.0 (by /u/you)"`. `.env` is git-ignored.
3. Start it:

   ```sh
   docker compose up -d
   curl http://localhost:8080/healthz          # ok
   curl http://localhost:8080/r/selfhosted/.rss
   docker compose logs -f                      # token or Reddit errors show up here
   ```

**Ports.** The app always listens on `8080` inside the container; the compose
file pins `LISTEN_ADDR=:8080` so a stray value in `.env` cannot break that.
Choose where it is reachable with the `ports:` line, `"<host address>:<host port>:8080"`:

| `ports:` | Reachable at |
| --- | --- |
| `"127.0.0.1:8080:8080"` (default) | only this machine, e.g. for a reverse proxy (nginx, Caddy, Traefik) |
| `"127.0.0.1:9000:8080"` | only this machine, on port 9000 (if 8080 is taken) |
| `"8080:8080"` | every network interface, port 8080 |

If the reverse proxy itself runs in Docker, you can drop `ports:` and put both
containers on a shared network; the proxy then reaches it at `http://reddit-rss:8080`.

**Public URL.** Set `BASE_URL` in `.env` to the address people use, e.g.
`BASE_URL=https://the-new-reddit-rss.com`, so the feeds' self links point there
and not at `localhost:8080`.

**Updating.**

```sh
docker compose pull && docker compose up -d
```

To stay on a known build, replace `latest` with a `sha-<commit>` tag.

Without Compose:

```sh
docker run -d --name reddit-rss -p 127.0.0.1:8080:8080 --env-file .env --restart unless-stopped \
  ghcr.io/ricardo-duarte-av/reddit-rss:latest
```

Put it behind a reverse proxy for TLS. Set `BASE_URL` to the public URL (or
make sure the proxy passes `Host` and `X-Forwarded-Proto`) so the feeds' self
links are right.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `REDDIT_CLIENT_ID` | required | |
| `REDDIT_CLIENT_SECRET` | required | |
| `REDDIT_USERNAME`, `REDDIT_PASSWORD` | empty | log in as this account instead of app-only auth |
| `REDDIT_USER_AGENT` | `linux:reddit-rss:v1.0 (by /u/<username>)` | Reddit asks for a unique, descriptive one |
| `LISTEN_ADDR` | `:8080` | |
| `BASE_URL` | from the request | public URL, for self links |
| `LINK_BASE` | `https://www.reddit.com` | where post links point, e.g. `https://old.reddit.com` |
| `CACHE_TTL` | `15m` | |
| `MIN_REQUEST_INTERVAL` | `1s` | minimum spacing between Reddit calls |

## Development

```sh
go test -race ./...
```

The tests run against a fake Reddit API; no credentials needed.
