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
