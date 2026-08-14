# TradingView Desktop ticker integration on macOS

Polygon Charts can make the existing symbol-action button control three independent parts of the local trading workflow:

1. The receiver configured by `SYMBOL_ACTION_URL` — currently used for the existing **Load in Tape + Trading** workflow.
2. The active chart in TradingView Desktop.
3. Polygon Charts itself, which remains on the chart already displayed in the browser.

The TradingView request is separate from the existing symbol-action request. TradingView being unavailable does not stop Tape Reading Tool or Yamir Trading Tools, and an unavailable symbol-action receiver does not stop the TradingView attempt.

## How it works

TradingView Desktop is an Electron application. When it is launched with a loopback Chrome DevTools Protocol (CDP) port, Polygon Charts can:

- query the local target list at `127.0.0.1:9222`;
- select a TradingView chart page;
- connect to that page's local CDP WebSocket;
- evaluate the active chart's internal `setSymbol()` method.

The backend implementation is native Go. It does not require Node.js, npm, Claude Code, an MCP client, or a separately running `tradingview-mcp` process.

The technique and internal TradingView API path were informed by the MIT-licensed `tradesdontlie/tradingview-mcp` project. TradingView's internal chart API is undocumented and may change in a later TradingView Desktop release. See `THIRD_PARTY_NOTICES.md`.

## Public-repository defaults

This is a public repository, so the feature is disabled by default and no machine-specific values or secrets are committed. The CDP address and timeout are ordinary local configuration values, not credentials.

Polygon Charts also applies the following protections:

- CDP HTTP and WebSocket destinations must be loopback addresses.
- Proxy use is disabled for CDP traffic.
- TradingView status and ticker-control endpoints reject non-loopback callers.
- The supplied launcher binds CDP to `127.0.0.1`, not to the LAN.

Do not expose or forward the CDP port through a router, tunnel, public interface, or remote port-forward. CDP can control the running application.

## One-time setup

### 1. Install and sign in to TradingView Desktop

Install TradingView in one of the normal macOS locations:

- `/Applications/TradingView.app`
- `~/Applications/TradingView.app`

Open TradingView normally once and sign in to your account.

### 2. Enable TradingView in Polygon Charts

Add the following values to the real `.env` file in the Polygon Charts repository:

```dotenv
POLYGON_CHARTS_TRADINGVIEW_ENABLED=true
POLYGON_CHARTS_TRADINGVIEW_CDP_URL=http://127.0.0.1:9222
POLYGON_CHARTS_TRADINGVIEW_TIMEOUT=3s
```

Keep the existing generic integration that powers **Load in Tape + Trading**, for example:

```dotenv
SYMBOL_ACTION_URL=http://127.0.0.1:8090/api/symbol
SYMBOL_ACTION_LABEL=Load in Tape + Trading
```

The exact `SYMBOL_ACTION_URL` should remain whatever your working Yamir Trading Tools/Tape integration already uses. Enabling TradingView does not replace that receiver.

Restart Polygon Charts after changing `.env`.

### 3. Make the launcher executable

```bash
chmod +x scripts/launch-tradingview-debug-mac.sh
```

Git normally preserves the executable bit, so this is only necessary if it was lost during a copy or checkout.

## Start one shared TradingView instance

A normal Dock or Finder launch does not expose the CDP endpoint. From the Polygon Charts repository, run:

```bash
./scripts/launch-tradingview-debug-mac.sh
```

The script:

- detects and reuses an already-ready debug-enabled TradingView instance;
- otherwise finds `TradingView.app`;
- closes a normally launched TradingView process;
- relaunches TradingView with CDP bound only to `127.0.0.1:9222`;
- waits for a TradingView chart target;
- writes the process log to `~/.polygon-charts/tradingview-debug.log`.

Save unsaved Pine Editor work before the first relaunch.

A different port is supported:

```bash
./scripts/launch-tradingview-debug-mac.sh 9333
```

When using another port, update every participating local application's CDP URL to the same value, including Polygon Charts:

```dotenv
POLYGON_CHARTS_TRADINGVIEW_CDP_URL=http://127.0.0.1:9333
```

## Sharing TradingView with the complete local stack

Only one debug-enabled TradingView Desktop process is needed. The same instance can be shared by Polygon Charts, Watchlist Tool, DaiDai, Yamir Trading Tools, and other local programs.

Typical local endpoints are:

| Component | Default address | Purpose |
| --- | --- | --- |
| TradingView CDP | `127.0.0.1:9222` | Shared local chart-control endpoint |
| Polygon Charts | `127.0.0.1:8081` | Historical/intraday charts and symbol action |
| Tape Reading Tool | `127.0.0.1:8097` | Tape display and ticker selection |
| Watchlist Tool | `127.0.0.1:8098` | Watchlists and ticker selection |
| DaiDai | configured app port | Scanner/dashboard and ticker selection |
| Yamir Trading Tools | configured app ports | Trading controls and integrations |

The Polygon Charts integration creates no additional listening port. It uses the existing Polygon Charts server and makes an outbound loopback connection to TradingView on port `9222`.

The TradingView integration itself does not connect to IBKR. Continue using unique IBKR client IDs for applications that do connect to IB Gateway or TWS.

When two applications send different symbols almost simultaneously, TradingView shows the command that completes last. During normal manual use, this is usually the ticker most recently clicked.

## Verify TradingView before starting the tools

Check the CDP version endpoint:

```bash
curl --noproxy '*' -s http://127.0.0.1:9222/json/version | python3 -m json.tool
```

Check the chart targets:

```bash
curl --noproxy '*' -s http://127.0.0.1:9222/json/list | python3 -m json.tool
```

The target list should contain a page whose URL or title identifies TradingView, normally a URL containing `tradingview.com/chart`.

Python is used only to format the diagnostic JSON. It is not required by the integration itself.

## Start Polygon Charts

```bash
go run .
```

Or use the repository helper:

```bash
./go.sh
```

Open a chart page containing the configured **Load in Tape + Trading** button. When TradingView is enabled, a small status appears beside the existing action feedback:

- `TV READY` — a TradingView chart target was found;
- `TV AAPL` — AAPL was sent successfully;
- `TV OFFLINE` — the local debug port or chart target is unavailable;
- `TV ERROR` — the connection or internal chart command failed.

When TradingView integration is disabled, no TradingView status is shown and the existing button behaves exactly as before.

## What happens when the button is clicked

The browser starts two independent same-origin requests:

1. The existing `/api/symbol-action` request, which Polygon Charts forwards to `SYMBOL_ACTION_URL`.
2. A new `/api/integrations/tradingview/ticker` request, which changes the active TradingView chart.

Neither request waits for the other. The existing button label and original success/error feedback remain intact. TradingView has its own adjacent status indicator.

If no `SYMBOL_ACTION_URL` is configured but TradingView is enabled, Polygon Charts reveals the same action button as **Load in TradingView**, allowing a TradingView-only setup without changing the HTML template.

## Direct status and ticker tests

Check whether Polygon Charts can see TradingView:

```bash
curl -s http://127.0.0.1:8081/api/integrations/tradingview/status | python3 -m json.tool
```

Expected when ready:

```json
{
  "enabled": true,
  "connected": true
}
```

Change the active TradingView chart directly to AAPL:

```bash
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  -d '{"symbol":"AAPL"}' \
  http://127.0.0.1:8081/api/integrations/tradingview/ticker
```

Expected response:

```json
{
  "symbol": "AAPL"
}
```

Test the existing generic action separately:

```bash
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  -d '{"symbol":"NVDA"}' \
  http://127.0.0.1:8081/api/symbol-action
```

The two direct tests make it easy to determine which destination is unavailable without changing the behavior of the combined UI button.

## Daily startup sequence

Start the shared TradingView instance once, then start the local applications in any order:

```bash
cd /path/to/polygon-charts
./scripts/launch-tradingview-debug-mac.sh
./go.sh
```

After a full TradingView quit, crash, update, or Mac restart, use the launcher again. Reopening TradingView from the Dock normally does not include the required debug flags.

## Troubleshooting

### No TradingView status appears

Confirm `.env` contains:

```dotenv
POLYGON_CHARTS_TRADINGVIEW_ENABLED=true
```

Then restart Polygon Charts and reload the chart page.

### Status shows `TV OFFLINE`

Run:

```bash
curl --noproxy '*' -i http://127.0.0.1:9222/json/version
curl --noproxy '*' -i http://127.0.0.1:9222/json/list
```

If either request fails, quit TradingView completely and run:

```bash
./scripts/launch-tradingview-debug-mac.sh
```

If `/json/version` works but `/json/list` has no TradingView chart page, open a chart inside TradingView and retry.

### Port 9222 is already occupied

```bash
lsof -nP -iTCP:9222 -sTCP:LISTEN
```

Use a different port only when the listener is not the intended TradingView instance. Update all applications that control TradingView to use the same replacement port.

### Tape/Yamir Trading Tools works but TradingView does not

The two paths are independent. Check:

```bash
curl -s http://127.0.0.1:8081/api/integrations/tradingview/status | python3 -m json.tool
```

Then inspect:

```bash
tail -100 ~/.polygon-charts/tradingview-debug.log
```

### TradingView works but Tape/Yamir Trading Tools does not

Leave the TradingView configuration in place and verify the existing receiver:

```bash
grep '^SYMBOL_ACTION_' .env
```

Then test `/api/symbol-action` directly with the command shown above. A failure in that receiver does not indicate a TradingView problem.

### The wrong TradingView pane or window changes

The integration selects the first CDP page whose URL identifies a TradingView chart and changes that page's active chart widget. For predictable behavior during initial testing:

- use one TradingView Desktop window;
- activate the pane you want Polygon Charts to control;
- close unrelated TradingView chart windows while diagnosing.

### It stopped working after a TradingView update

First confirm that `/json/version` and `/json/list` still respond. If CDP is available but ticker changes fail, TradingView may have changed its undocumented internal chart API. Compare the current behavior with the upstream `tradingview-mcp` project and inspect the process log.
