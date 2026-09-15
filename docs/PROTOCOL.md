# Wire protocol

This document fixes how the widget, the popup and the server talk to each other.

## HTTP routes

All routes live under `server.base_path`.

| Route | Purpose |
|---|---|
| `GET james.js` | the widget module the host page embeds |
| `GET /` | the popup page |
| `GET static/...` | CSS, JS, vendored libraries, `icon.svg`, `i18n/*.json` |
| `POST chat` | one turn, answered as Server-Sent Events |
| `GET healthz` | returns `ok` |
| `GET demo` | the demo host page, served with `-dev` only |

`james.js` and everything under `static/` answer with
`Access-Control-Allow-Origin: *`, because the widget runs on the host page and
reads its icon and its interface texts from there. `chat` only allows the
origins from `server.allowed_origins`.

The browser may keep the vendored libraries under `static/vendor` for an hour.
Everything else is asked about on every load, so a new server version never
meets a kept file of an older one. `james.js` and every answer under `static/`
carry an `ETag` of their content, so a question about an unchanged file is
answered with 304 and no body.

## Token

A JWT signed with HS256 and the shared secret. Claims:

| Claim | Meaning |
|---|---|
| `sub` | user ID, required. Keys the history in `localStorage`. |
| `name` | display name, optional. Only logged. |
| `exp` | expiry as Unix seconds, required. |

## Widget and popup (`postMessage`)

The widget opens the popup with `window.open(popupURL, "james")`. It only
accepts messages whose `origin` is the agent server's origin, taken from
`import.meta.url`. The popup only accepts messages from the window that opened it,
and only when their origin is in `server.allowed_origins`, which the server
writes into the popup page, or is the agent server's own origin.

Every message is a JSON object with a `type` field.

Popup to widget:

| type | fields | meaning |
|---|---|---|
| `ready` | | popup loaded, asks for init |
| `tool` | `id`, `name`, `input` | run a browser tool |
| `confirm_result` | `id`, `ok` | the answer to a question |

Widget to popup:

| type | fields | meaning |
|---|---|---|
| `init` | `token`, `lang`, `context` | token and settings from the widget element |
| `tool_result` | `id`, `output`, `is_error` | result of a browser tool; `output` is a string |
| `confirm` | `id`, `call`, `text` | ask the user a yes or no question |

The popup posts `ready` when it loads, after a `navigate` tool moved the host
page, and after a turn failed with `unauthorized`. The widget answers every
`ready` with `init`, so a new host page and a fresh token reach the popup. The
widget element also has `open()` and `close()` methods, so the host page can
drive the popup from its own menu.

## Browser tools

Every browser tool is one module under `web/static/tools`, named after the tool.
The widget loads a module when the tool is first called and passes the input of
the call to it. A module that answers with an `after` function has that function
run once the result is posted, so a tool may leave the page. A module that
throws answers with `is_error` true and the message of the error.

A module that needs the user's agreement calls `ctx.confirm(text)`, which
answers a promise the module has to await. The widget sends the text to the
popup, the popup shows it as a form in the conversation, and the answer comes
back as `confirm_result`. The question is asked there because the popup is the
window the user looks at. While a question is on screen the popup does not time
the tool call out. A question whose tool call is already gone counts as a no,
and so does a question the widget cannot deliver. A tool call that answers
while a question of its own is still open fails, so a module that forgets to
await cannot pass an unanswered question as agreement.

`read_page` input: `{"selector": "optional CSS selector"}`. Output: a text
with the URL, the title and the rendered text of the page or the selected
element, taken from `innerText`. The browser leaves out what it does not
render, so scripts, styles and hidden elements never appear. The output is
capped at 100 000 characters. A selector that is broken, matches nothing or
matches a hidden element answers with `is_error` true.

`read_dom` input: `{"selector": "optional CSS selector", "depth": 4, "full": false}`.
Output: the markup below the selected element, indented, one element per line.
Nothing is left out for being unimportant, but three limits apply. Elements
deeper than `depth` are replaced by `<!-- n more elements -->`, texts and
attribute values longer than 500 characters are cut off and carry
`[n more characters]`, and the whole result stops at 100 000 characters with a
`<!-- cut off here -->` comment. The way to whatever a limit hid is another
call: a selector pointing at the element, a larger `depth`, or `full` true for
the values of that element. `full` does not change `depth`. The tool reads the
markup, so unlike `read_page` it also reaches the text of elements the browser
does not render, such as an inline script. A selector that is broken or matches
nothing answers with `is_error` true.

`navigate` input: `{"url": "..."}`. The widget asks the user in the popup. On
yes it sets `location.href` and answers `"navigated to <url>"`. On no it
answers `"the user declined"` with `is_error` true. Only `http:` and `https:`
URLs are allowed.

## Chat request

`POST chat` with `Content-Type: application/json`:

```json
{
  "token": "<jwt>",
  "lang": "de",
  "context": {"user": "Anna", "page": "/orders/42"},
  "messages": [ {"role": "user", "content": [ {"type": "text", "text": "..."} ]} ]
}
```

`messages` is the whole history in the `llm.Message` JSON form. The last
message must be from the user. Image blocks appear only in the last user
message. `context` is any JSON value and reaches the system prompt as text.
The server ignores `lang`; the language of the answer belongs in the
operator's system prompt.

## Chat response

`text/event-stream`. Every event has an `event:` name and one JSON `data:`
line.

| event | data | meaning |
|---|---|---|
| `text` | `{"text": "..."}` | a piece of answer text |
| `tool_start` | `{"id","name","input"}` | a server tool starts |
| `tool_end` | `{"id","name","output","is_error"}` | a server tool finished; `output` is a short summary |
| `done` | see below | the turn ended |
| `error` | `{"code","message"}` | the turn failed; the stream ends |

`done` data:

```json
{
  "stop_reason": "end_turn",
  "messages": [ ...new messages to append to the history... ],
  "browser_tools": [ {"id","name","input"} ],
  "tool_results": [ ...tool_result blocks of server tools that ran alongside... ],
  "usage": {"input_tokens": 0, "output_tokens": 0}
}
```

When `browser_tools` is empty the popup appends `messages` to the history and
the turn is over. When it is not empty the popup runs every browser tool
through the widget, builds one user message from `tool_results` followed by
its own `tool_result` blocks, appends `messages` and that user message to the
history, and posts the next turn at once.

Error codes: `unauthorized`, `bad_request`, `context_too_long`, `provider`,
`cancelled`, `internal`. The popup shows `context_too_long` as a request to
start a fresh conversation. `cancelled` means the server stopped the turn, for
example because it is shutting down.

## History in the browser

`localStorage` key `james:history:<sub>`. After a turn is answered the popup
replaces every image block in the history with a text block reading
`[image attached here, no longer available]`, both in user messages and in
tool results, and drops the thumbnail from the display in the same way.

## Chart block

A fenced code block with the language `chart` holds this JSON, drawn by uPlot:

```json
{
  "title": "Orders per month",
  "type": "line",
  "x": {"label": "Month", "values": ["2026-01", "2026-02"]},
  "y": {"label": "Orders"},
  "series": [
    {"label": "Shop", "values": [12, 15]},
    {"label": "API", "values": [3, 7]}
  ]
}
```

`type` is `line` or `bar`. `x.values` are strings or numbers and are shown as
labels. Every series has as many values as `x.values`. `null` marks a gap.
