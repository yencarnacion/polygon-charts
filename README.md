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
