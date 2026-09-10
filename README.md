# polygon-charts

## Setup

```bash
go mod init charts       # only first time
go get github.com/joho/godotenv
go mod tidy
```

Set your API keys in `.env` (see `env.example`), then run:

```bash
go run .
```

Default app URL: `http://localhost:8081`

`MARKETAUX_API_KEY` is optional. When it is not set, the Marketaux Catalyst Radar panels (`/api/market-stats`) are hidden on both pages so the UI stays uncluttered.

## News and catalyst cutoff

Set `RTPR_API_KEY` in `.env` to add RTPR news and press releases, then restart
with `go run .` or `./go.sh`. An empty or omitted key disables RTPR. The integration
uses the [documented Article Feed API](https://rtpr.io/docs#article-feed-api), which
requires RTPR Pro API access. Historical coverage starts in May 2026 and is subject
to RTPR's retention window. Feed pages are followed until the requested New York
date range is covered, with a 30-second request budget. Partial coverage is labeled
in the UI; cached articles and other providers remain available on RTPR failures.

All news views deduplicate canonical URLs (ignoring common tracking parameters)
and matching headlines across sources, normalizing case, whitespace, and punctuation.
Headline matches use a 48-hour window and preserve decimal values and numeric signs;
different reporting periods and figures remain separate. The earliest publication
is retained so a later syndicated copy does not look like a fresh catalyst. This
is conservative deduplication, not semantic clustering of differently worded reporting.
The existing class-action solicitation filter still applies.

In every news and SEC table, items **after the previous trading day's 3:59 p.m.
New York time** have a blue left stripe and shaded background. Labeled groups mark
**AFTER** and **AT / BEFORE** that cutoff; rows remain newest first within each group.
The cutoff uses SPY trading sessions to account for weekends/market holidays and
New York timezone rules for daylight saving. Intervening calendar days are included.
Missing times or an unavailable cutoff are labeled separately. Replay still shows
only items available at its current time. All displayed news/filing times use ET,
even when the browser is in another timezone.

The design combines a distinct color feature with spatial grouping and explicit
labels, informed by [visual-search and feature-integration research](https://pmc.ncbi.nlm.nih.gov/articles/PMC6513732/).
It does not depend on color alone and does not encode bullish/bearish sentiment.

News and filing links open in new tabs. RTPR releases open in a local reader that
keeps the API key on the server, preserves text/tables, removes active page content,
and opens links inside the article in new tabs too.

Validation: `go test -race ./...`; opt-in live RTPR check:
`RTPR_LIVE_TEST=1 go test -run TestRTPRLive -v .`.
`node scripts/catalyst-browser-check.mjs` checks grouping, replay refresh, ET timestamps
and new-tab links in headless Chrome (Node 22+); it downloads the same Tabulator
version as the app. For offline checks, set `TABULATOR_JS` to a local
`tabulator.min.js` alongside `tabulator.min.css`.

## ntfy Push Notes

The ntfy integration is disabled by default. Enable it in `.env`:

```bash
NTFY_ENABLED=true
NTFY_SERVER=push.example.com
NTFY_TOPIC=hello
```

If you need to post to a non-TLS server, include the scheme explicitly, e.g. `NTFY_SERVER=http://push.example.com`.

The quick-send checkboxes on the chart page come from `config.yaml`:

```yaml
ntfy:
  tags:
    - morning top
    - morning bottom
    - rubber band
    - right side of the v
    - flush base
```

## Optional Symbol Action

Polygon Charts can show a generic action beside `Push to ntfy` that sends the
current chart symbol to another local tool. It is disabled unless configured:

```bash
SYMBOL_ACTION_URL=http://127.0.0.1:8090/api/symbol
SYMBOL_ACTION_LABEL="Send symbol"
```

The receiver gets `POST` JSON in the portable form `{"symbol":"AAPL"}`. The
browser calls Polygon Charts on the same origin; the Go server forwards the
request with a short timeout, avoiding cross-origin setup in local receivers.
This hook is intentionally tool-agnostic so dashboards, journals, scanners,
and other local workflows can use it without application-specific coupling.

Candlestick charts also show a high-contrast OHLCV readout when the pointer is
over a candle.

## Optional TradingView Desktop Integration

The same symbol-action button can also change the active chart in a locally
running TradingView Desktop app. This is useful when the generic action is
labeled **Load in Tape + Trading**: one click can keep its existing Tape/Yamir
Trading Tools behavior while independently updating TradingView.

Enable the integration in `.env`:

```dotenv
POLYGON_CHARTS_TRADINGVIEW_ENABLED=true
POLYGON_CHARTS_TRADINGVIEW_CDP_URL=http://127.0.0.1:9222
POLYGON_CHARTS_TRADINGVIEW_TIMEOUT=3s
```

Start one shared debug-enabled TradingView instance before Polygon Charts:

```bash
./scripts/launch-tradingview-debug-mac.sh
```

The implementation is native Go and does not require Node.js or a separately
running MCP server. The existing `/api/symbol-action` request and the new
TradingView request are independent, so either destination can be offline
without blocking the other. A single TradingView instance can be shared with
Watchlist Tool, DaiDai, Yamir Trading Tools, and other local applications.

The feature is disabled by default because this repository is public. It accepts
only loopback CDP addresses and refuses remote TradingView-control requests.
Full macOS setup, direct tests, multi-tool operation, and troubleshooting are in
[`docs/TRADINGVIEW_DESKTOP_INTEGRATION.md`](docs/TRADINGVIEW_DESKTOP_INTEGRATION.md).
Third-party attribution is in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

## URL API For Deep Links

You can open a chart directly from a clickable URL using ticker/date, with optional time.

### Endpoint

`/api/open-chart`

This endpoint validates parameters, then redirects to `/chart`.

### URL formats

Path-style:

`/api/open-chart/{ticker}/{date}`

`/api/open-chart/{ticker}/{date}/{time}`

Query-style:

`/api/open-chart?ticker={ticker}&date={date}`

`/api/open-chart?ticker={ticker}&date={date}&time={time}`

### Required and optional params

- `ticker` (required): stock symbol, e.g. `TSLA`
- `date` (required): `YYYY-MM-DD`, e.g. `2026-02-18`
- `time` (optional): `HHMM` or `HH:MM`, e.g. `0945` or `09:45`
- `resolution` (optional): one of `1m`, `2m`, `3m`, `5m`, `10m`, `15m`, `30m`, `1h` (default `1m`)
- `signal` (optional): `buy` or `sell` (default `buy`)

### Behavior

- With only `ticker` + `date`, the chart opens for the full day.
- If `time` is provided, the chart opens with the signal marker at that time.
- Timed and current-day charts expose `Next candle` for review; `↻ Reset`
  returns the tab to the candle cutoff it had when it first opened, including
  its as-of indicators, catalysts, filings, and TPO view.
- If `signal` is not provided, it defaults to `buy`.

### Examples

- Full day:
  - `http://localhost:8081/api/open-chart/AAPL/2026-02-18`
- Buy signal at time:
  - `http://localhost:8081/api/open-chart/AAPL/2026-02-18/0945`
- Query-style with `HH:MM`:
  - `http://localhost:8081/api/open-chart?ticker=AAPL&date=2026-02-18&time=09:45`
- Query-style with custom resolution:
  - `http://localhost:8081/api/open-chart?ticker=AAPL&date=2026-02-18&time=0945&resolution=5m`

## Direct `/chart` URLs

You can also link directly to `/chart`:

- `http://localhost:8081/chart?ticker=AAPL&date=2026-02-18`
- `http://localhost:8081/chart?ticker=AAPL&date=2026-02-18&time=0945`
