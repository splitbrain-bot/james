# james

james is a single Go binary that serves a chat agent for one deployed
application. One YAML file tells it where the source code, the databases and the
allowed websites are, and it reads them to answer questions about that
application. It never writes anything: no file, no database row, no server side
conversation store.

## Quick start

Build the binary. It is static, so it runs on any Linux host:

```sh
CGO_ENABLED=0 go build -o james .
```

Copy `james.example.yaml` to `james.yaml`, copy `prompt.example.md` to
`prompt.md`, and describe your own application in both. Then start the server:

```sh
./james -config james.yaml
```

`-config` defaults to `james.yaml`. `./james -version` prints the version and
exits. `./james -dev` adds the demo host page described under
[Development](#trying-it-out), for trying james out without a host application.

james speaks plain HTTP and binds to `127.0.0.1` by default. Put a reverse proxy
with TLS in front of it. The proxy path and `server.base_path` have to match:

```nginx
location /james/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;

    # answers stream, so no buffering and a generous read timeout
    proxy_buffering off;
    proxy_read_timeout 300s;
}
```

With this block, set `base_path: /james` in the configuration file. All routes
live under it: `/` (the popup page), `james.js` (the widget), `static/...`,
`chat` and `healthz`.

### Docker

Every push to `main` publishes `ghcr.io/splitbrain/james:latest`. The image
carries the binary alone and reads its configuration from
`/etc/james/james.yaml`:

```sh
docker run -v ./conf:/etc/james:ro -p 8080:8080 ghcr.io/splitbrain/james
```

Two settings differ inside a container: `server.listen` has to be `0.0.0.0:8080`
because the default address is reachable from the container only, and the
container needs a user that may read the files and the database of your
application. The image runs as `65534` until the deployment says otherwise.

## Integration

The host application loads the widget module and adds one element, filled by
its own backend:

```html
<script type="module" src="https://agent.example.com/james.js"></script>

<james-widget token="<jwt signed with the shared key>"
              lang="de"
              context='{"user":"Anna","page":"/orders/42"}'></james-widget>
```

The element shows a round button in the bottom right corner. Clicking it opens
the agent in a popup window. The host page can also open and close the popup
from its own menu:

```js
document.querySelector("james-widget").open();
document.querySelector("james-widget").close();
```

Button and styles live in a shadow root, so the widget neither inherits nor
changes the styles of the host page.

`lang` picks the language of the interface texts, `de` or `en`. Without it the
browser setting decides. The language of the answers is not set here, it belongs
in your system prompt.

`icon` is the address of the picture on the button. It defaults to the icon the
server ships and is resolved against the host page:

```html
<james-widget token="…" icon="/img/assistant.svg"></james-widget>
```

`context` is any JSON object. It reaches the system prompt as text, so the agent
knows who asks and what page they are on. It is unverified input and grants no
access.

The widget reads `token`, `lang` and `context` every time the popup asks for
them. A page that refreshes the token in the attribute hands the fresh one to a
popup that is already open.

### Signing the token

The token is a JWT signed with HS256 and the secret from `auth.secret`. It needs
the claims `sub` (the user ID), `exp` (expiry as Unix seconds) and optionally
`name` (a display name). No library is needed.

PHP:

```php
function james_b64(string $data): string {
    return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
}

$secret    = getenv('JAMES_SECRET');
$header    = james_b64(json_encode(['alg' => 'HS256', 'typ' => 'JWT']));
$payload   = james_b64(json_encode([
    'sub'  => $userId,
    'name' => $userName,
    'exp'  => time() + 3600,
]));
$signature = james_b64(hash_hmac('sha256', "$header.$payload", $secret, true));
$token     = "$header.$payload.$signature";
```

Python:

```python
import base64, hashlib, hmac, json, os, time

def b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

secret = os.environ["JAMES_SECRET"].encode()
header = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
payload = b64(json.dumps({
    "sub": user_id, "name": user_name, "exp": int(time.time()) + 3600,
}).encode())
signature = b64(hmac.new(secret, f"{header}.{payload}".encode(), hashlib.sha256).digest())
token = f"{header}.{payload}.{signature}"
```

Give the token a short lifetime and mint a fresh one with every page that embeds
the widget.

## Configuration

Every value may come from an environment variable through `${NAME}`, or from a
file through `@path`. A relative path is resolved against the directory of the
configuration file. Both work for every value and work together, so
`"@${SECRET_FILE}"` reads the file named by the variable. File content loses its
trailing whitespace. A missing environment variable stops the start with an
error. Changes take effect on restart, which costs nothing because the server
holds no state.

| Key | Meaning |
|---|---|
| `server.listen` | Address to bind. Defaults to `127.0.0.1:8080`. |
| `server.base_path` | URL prefix all routes live under. Defaults to `/`. |
| `server.allowed_origins` | Origins of host pages that may call the API, written as the browser sends them, for example `https://app.example.com` without a path. At least one origin is required. |
| `server.max_body_bytes` | Largest chat request body. Defaults to 20 MiB. |
| `server.max_image_bytes` | Largest image the popup may attach. Defaults to 5 MiB. |
| `auth.secret` | HS256 signing key, shared with the host application. Required. |
| `llm.provider` | `anthropic` or `openai`. Required. |
| `llm.base_url` | API root. Defaults to the provider's own. |
| `llm.model` | Model name. Required. |
| `llm.api_key` | API key. Required. |
| `llm.max_tokens` | Longest answer of one model call. Defaults to 8192. |
| `agent.system_prompt` | The prompt describing your application, tone and answer language. Usually `"@prompt.md"`. |
| `agent.max_tool_steps` | Model calls per turn. Defaults to 20. |
| `tools.files[].path` | A directory the file tools may read. Required per entry. |
| `tools.files[].exclude` | Names and glob patterns that stay hidden, matched per path segment, for example `.git`, `vendor`, `*.env`. |
| `tools.files[].max_file_bytes` | Largest file one read returns. Defaults to 1 MiB. |
| `tools.databases.<name>.driver` | `mysql`, `postgres` or `sqlite`. Required per connection. |
| `tools.databases.<name>.dsn` | Connection string for that driver. Required. |
| `tools.databases.<name>.max_rows` | Rows one query returns. Defaults to 200. |
| `tools.databases.<name>.timeout` | Run time of one query. Defaults to `10s`. |
| `tools.fetch.allow` | Host names the fetch tool may read. A leading `*.` allows all subdomains. At least one host is required when the section is present. |
| `tools.fetch.max_bytes` | Largest response. Defaults to 2 MiB. |
| `tools.fetch.timeout` | Time one fetch may take. Defaults to `15s`. |
| `tools.browser` | Names of the browser tools to offer, each one a module in `web/static/tools`, for example `read_page`, `read_dom` and `navigate`. |
| `log.format` | `text` or `json`. Defaults to `text`. |

Each section under `tools` switches its own tools on: `files` gives
`list_files`, `read_file`, `glob` and `grep`, `databases` gives `db_schema` and
`db_query`, `fetch` gives `fetch_url`, and `browser` gives the tools it names. A
section that is missing or empty switches those tools off. The agent then does
not see them at all.

## Tools

| Tool | Runs | Does |
|---|---|---|
| `list_files` | server | list a directory |
| `read_file` | server | read a text file, or an image file as picture, with a size cap |
| `glob` | server | find files by name pattern |
| `grep` | server | find files by content |
| `db_schema` | server | full schema of one connection |
| `db_query` | server | one query in the read-only grammar, row limit and timeout enforced |
| `fetch_url` | server | fetch an allowed URL, as readable text or raw HTML |
| `read_page` | browser | URL, title and rendered text of the host page, optionally under a CSS selector |
| `read_dom` | browser | markup of the host page down to a given depth, to find out how it is built |
| `navigate` | browser | open a URL in the host page, after the user agrees |

`db_query` does not send SQL to the database. It takes a small SQL-looking
grammar of its own, parsed here and translated into a real `SELECT`. The grammar
covers joins, `WHERE` with `AND`, `OR` and `NOT`, comparisons, `IN`, `LIKE`,
`IS NULL`, `BETWEEN`, `GROUP BY`, `ORDER BY`, `LIMIT` with `OFFSET`, and the
aggregates `COUNT`, `SUM`, `AVG`, `MIN` and `MAX`. It has no subqueries, no
`UNION`, no `HAVING`, no `DISTINCT`, no computed expressions, no other functions
and no comments, and it takes one statement per call. There is no write
production, so a write is a syntax error rather than something the server has to
detect. Identifiers are checked against the live catalog and quoted, values are
always bound as parameters.

Use a database user with read-only rights anyway. It costs nothing and it is the
second line of defence.

## Images and charts

The user can attach or paste images, for example a screenshot of the problem.
They are sent as they are, because the API scales them itself; only images above
`server.max_image_bytes` are rejected. `read_file` can return an image file as a
picture in the same way.

An image reaches the model once, with the message it belongs to. Later turns see
only the note `[image attached here, no longer available]`, and the popup drops
the thumbnail at the same moment. A question about the picture needs it attached
again.

Answers may contain diagrams and charts. A fenced `mermaid` block is drawn by
Mermaid, which also covers single series bar, line and pie charts. A fenced
`chart` block holds a JSON specification drawn by uPlot, for charts with several
series and a legend; `docs/PROTOCOL.md` describes it. Both libraries load only
when a block of their kind appears, and your system prompt should tell the agent
that they exist.

## Security model

- The token proves the request comes from your application. It carries user ID,
  name and expiry. Signature and expiry are checked on every chat request, and
  the claims reach the log only.
- The context object is unverified input. It shapes the conversation and never
  grants access.
- The history comes from the browser and can be edited by the user. It cannot
  widen access either, because the file roots, the database connections and the
  URL allowlist live in the server configuration.
- File paths are made absolute and symlinks are resolved before a read, so `..`
  and links cannot leave the configured roots. Excluded names stay invisible.
- SQLite connections are opened with the `query_only` pragma, on top of the
  read-only grammar.
- CORS answers only the configured origins and the server's own, and the chat
  body has a size limit.
- Rendered markdown is sanitised with DOMPurify before it reaches the page, and
  Mermaid runs in its strict mode, because the answer text passes through the
  model and can contain anything.
- The popup accepts messages only from the configured origins, and the widget
  only from the origin its own script came from.

## Logging

One line per turn goes to standard output through `log/slog`: the user ID and
name from the token, the tools that were called, the input and output token
counts, the duration, and the error when the turn failed. Message texts and
answers are never logged. `log.format: json` switches the text format to JSON.

## Development

Run the tests with `go test ./...`. `go vet ./...` and `gofmt -l .` should stay
quiet.

The browser libraries are vendored under `web/static/vendor` and embedded into
the binary, so it needs no network at runtime and loads nothing from a CDN.
`web/static/vendor/VERSIONS` lists each library with its version and the URL it
came from. To update one, fetch the same path from the newer version and note
the new version in that file.

`.github/workflows/ci.yml` runs the format check, `go vet`, the tests and a
static build on every push to `main` and on every pull request. It then builds
the image from the `Dockerfile`. On `main` it also pushes the image to GHCR and
asks watchtower to pull it, which needs the repository variable
`WATCHTOWER_URL` and the secret `WATCHTOWER_HTTP_API_TOKEN`.

### Trying it out

`-dev` serves a demo host page at `<base_path>/demo`:

```sh
./james -config james.yaml -dev
```

The page stands in for your application. It embeds the widget the way the
[Integration](#integration) section describes, so the button, the popup, the
token and the browser tools all run on their usual path, and it holds a bit of
text for the browser tools to read. The token in the page is signed by the
server itself with `auth.secret` and is valid for an hour; reloading the page
mints a fresh one.

The route exists only with `-dev`, because anyone who can reach it gets a token
that the chat route accepts.

### Adding a browser tool

A browser tool is one module under `web/static/tools`. Its file name is the tool
name, so `read_page.js` is the tool `read_page`. The module opens with a JSON
comment that tells the model what the tool does, and it exports the function
that runs it:

```js
/*{
	"description": "Marks a text on the page the user is looking at.",
	"schema": {
		"type": "object",
		"properties": {"text": {"type": "string", "description": "the text to mark"}},
		"required": ["text"]
	}
}*/

/**
 * Mark a text on the host page.
 * @param {{text: string}} input the tool input
 * @param {ToolContext} ctx texts and dialogs of the widget
 * @returns {string} what the model is told
 */
export default function highlight(input, ctx) {
	return `marked ${input.text}`;
}
```

Besides its input the function gets a context: `t(key)` gives an interface text,
`confirm(text)` asks the user to agree, and `element` is the widget element the
call came through. The function may be asynchronous. What it returns becomes the
tool result, either a string or an object with an `output` and an `after`
function, which runs once the result is on its way. `navigate` uses `after` to
leave the page. An error the function throws becomes a failed tool result with
its message.

The server reads the JSON comment and offers the tool to the model. So a new
tool needs nothing but its file and its name under `tools.browser`.

`docs/PROTOCOL.md` fixes the wire protocol between widget, popup and server.
