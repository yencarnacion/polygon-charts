(() => {
  'use strict';

  const STATUS_ENDPOINT = '/api/integrations/tradingview/status';
  const TICKER_ENDPOINT = '/api/integrations/tradingview/ticker';
  const SYMBOL_PATTERN = /^[A-Z][A-Z0-9.-]{0,15}$/;

  const chartParams = new URLSearchParams(location.search);
  const replayPort = chartParams.get('gap_replay');
  const localTimedChart = chartParams.has('time') && ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname);
  let discoveredReplay = false;
  let selecting = false;
  let integrationEnabled = null;
  let lastSymbol = '';
  let activeRequest = null;
  let requestGeneration = 0;

  function currentSymbol() {
    const symbol = String(new URLSearchParams(window.location.search).get('ticker') || '').trim().toUpperCase();
    return SYMBOL_PATTERN.test(symbol) ? symbol : '';
  }

  function actionButton() {
    return document.getElementById('runSymbolAction');
  }

  function ensureMarketSurgeButton() {
    let button = document.getElementById('openMarketSurge');
    if (button) return button;

    const symbolButton = actionButton();
    const feedback = document.getElementById('symbolActionFeedback');
    const insertionPoint = feedback || symbolButton;
    if (!insertionPoint) return null;

    button = document.createElement('a');
    button.id = 'openMarketSurge';
    button.className = 'btn btn-outline-warning';
    button.textContent = 'MarketSurge';
    button.target = '_blank';
    button.rel = 'noopener noreferrer';
    button.title = 'Open this symbol in MarketSurge';
    button.style.marginLeft = '0.35rem';
    insertionPoint.insertAdjacentElement('afterend', button);
    return button;
  }

  function refreshMarketSurgeButton() {
    const button = ensureMarketSurgeButton();
    if (!button) return;
    const symbol = currentSymbol();
    if (!symbol) {
      button.classList.add('disabled');
      button.setAttribute('aria-disabled', 'true');
      button.removeAttribute('href');
      return;
    }
    button.classList.remove('disabled');
    button.removeAttribute('aria-disabled');
    button.href = `https://marketsurge.investors.com/mstool?chart=${encodeURIComponent(`symbol:${symbol}`)}`;
  }

  function ensureStatusElement() {
    let status = document.getElementById('tradingViewIntegrationStatus');
    if (status) return status;

    const feedback = document.getElementById('symbolActionFeedback');
    const button = actionButton();
    if (!feedback && !button) return null;

    status = document.createElement('span');
    status.id = 'tradingViewIntegrationStatus';
    status.className = 'symbol-action-feedback';
    status.setAttribute('aria-live', 'polite');
    status.style.marginLeft = '0.35rem';
    status.hidden = true;

    if (feedback) feedback.insertAdjacentElement('afterend', status);
    else button.insertAdjacentElement('afterend', status);
    return status;
  }

  function exposeActions(replay = Boolean(replayPort) || discoveredReplay) {
    const button = actionButton();
    if (!button) return;
    const configured = button.dataset.configured === 'true';
    if (!configured && !integrationEnabled && !replay) return;
    button.classList.remove('d-none');
    document.getElementById('symbolActionFeedback')?.classList.remove('d-none');
    if (!configured) button.textContent = replay ? 'Load in Tape + Trading' : 'Load in TradingView';
    if (replay) button.title = 'Load this ticker at the current Tape replay time';
  }

  function replayURL(port) {
    return `http://127.0.0.1:${port}/api/polygon/replay`;
  }

  async function replayDestination() {
    if (replayPort) return replayURL(replayPort);
    if (!localTimedChart) return '';
    // Older local chart links have a date and time but no replay port.
    const url = replayURL(8176);
    try {
      const response = await fetch(url, { credentials: 'omit', cache: 'no-store', signal: AbortSignal.timeout(900) });
      if (response.ok) {
        const desk = await response.json();
        if (desk.app === 'daidai-gap-replay' && desk.date === chartParams.get('date')) return url;
      }
    } catch { /* A standalone chart can still use its configured action. */ }
    return '';
  }

  function feedback(message, kind = '') {
    const target = document.getElementById('symbolActionFeedback');
    if (!target) return;
    target.textContent = message;
    target.classList.remove('is-error', 'is-success');
    if (kind) target.classList.add('is-' + kind);
  }

  async function confirmQueuedSelection(url, ticket, symbol) {
    for (let attempt = 0; attempt < 60; attempt++) {
      await new Promise(resolve => setTimeout(resolve, 1000));
      const response = await fetch(url, { credentials: 'omit', cache: 'no-store', signal: AbortSignal.timeout(3000) });
      if (!response.ok) throw new Error('Replay status unavailable.');
      const desk = await response.json();
      if (desk.date !== chartParams.get('date')) throw new Error('The replay session changed.');
      if (desk.command_id < ticket) continue;
      if (desk.state === 'ready' && desk.symbol === symbol) return true;
      throw new Error(desk.message || 'The replay selection changed or could not finish.');
    }
    return false;
  }

  async function loadSymbol(symbol) {
    const button = actionButton();
    if (!symbol || !button || selecting) return;
    const configured = button.dataset.configured === 'true';
    if (!configured && !replayPort && !localTimedChart) return;
    selecting = true;
    button.disabled = true;
    let url = '';
    try {
      url = await replayDestination();
      if (!url && !configured) return;
      feedback(url ? `Loading ${symbol} in Tape replay…` : `Sending ${symbol}…`);
      const response = await fetch(url || '/api/symbol-action', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        credentials: url ? 'omit' : 'same-origin',
        body: JSON.stringify(url ? { symbol, date: chartParams.get('date') } : { symbol })
      });
      const body = await response.text();
      let result = {};
      try { result = JSON.parse(body) || {}; } catch { /* Generic receivers can return plain text. */ }
      if (!response.ok) throw new Error(result.error || body.trim() || response.statusText);
      if (url) {
        const loaded = response.status === 202
          ? await confirmQueuedSelection(url, result.command_id, symbol)
          : result.loaded === true && result.symbol === symbol;
        if (loaded) feedback(`Tape replay · ${symbol}`, 'success');
        else feedback(`Tape replay has not confirmed ${symbol} yet.`);
      } else {
        if (result.tape_ok === false) throw new Error(result.tape_error || 'Tape unavailable.');
        feedback(`${symbol} sent`, 'success');
      }
    } catch (error) {
      feedback((url ? 'Tape replay: ' : '') + error.message, 'error');
    } finally {
      selecting = false;
      button.disabled = false;
    }
  }

  function renderStatus({ enabled, connected, symbol = '', error = '' }) {
    integrationEnabled = Boolean(enabled);
    const status = ensureStatusElement();
    if (!status) return;

    status.classList.remove('is-error', 'is-success');
    if (!integrationEnabled) {
      lastSymbol = '';
      status.hidden = true;
      return;
    }

    exposeActions();
    status.hidden = false;
    if (connected) {
      if (symbol) lastSymbol = String(symbol).toUpperCase();
      status.textContent = lastSymbol ? `TV ${lastSymbol}` : 'TV READY';
      status.title = 'TradingView Desktop chart is connected';
      status.classList.add('is-success');
      return;
    }

    lastSymbol = '';
    status.textContent = error ? 'TV ERROR' : 'TV OFFLINE';
    status.title = error || 'TradingView Desktop chart is unavailable';
    status.classList.add('is-error');
  }

  async function refreshStatus() {
    try {
      const response = await fetch(STATUS_ENDPOINT, { cache: 'no-store' });
      if (!response.ok) throw new Error((await response.text()).trim() || response.statusText);
      renderStatus(await response.json());
    } catch (error) {
      renderStatus({ enabled: integrationEnabled !== false, connected: false, error: error.message });
    }
  }

  async function ensureIntegrationState() {
    if (integrationEnabled === null) await refreshStatus();
    return integrationEnabled === true;
  }

  async function sendToTradingView(symbol) {
    if (!symbol || !(await ensureIntegrationState())) return;

    if (activeRequest) activeRequest.abort();
    activeRequest = new AbortController();
    const generation = ++requestGeneration;
    renderStatus({ enabled: true, connected: true, symbol: `${symbol}…` });

    try {
      const response = await fetch(TICKER_ENDPOINT, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ symbol }),
        keepalive: true,
        signal: activeRequest.signal
      });
      if (!response.ok) throw new Error((await response.text()).trim() || response.statusText);
      const payload = await response.json();
      if (generation !== requestGeneration) return;
      renderStatus({ enabled: true, connected: true, symbol: payload.symbol || symbol });
    } catch (error) {
      if (error.name === 'AbortError' || generation !== requestGeneration) return;
      renderStatus({ enabled: true, connected: false, error: error.message || 'TradingView Desktop is unavailable' });
    }
  }

  document.addEventListener('click', event => {
    if (!(event.target instanceof Element) || !event.target.closest('#runSymbolAction')) return;
    const symbol = currentSymbol();
    void loadSymbol(symbol);
    void sendToTradingView(symbol);
  }, true);

  function init() {
    refreshMarketSurgeButton();
    exposeActions();
    if (localTimedChart && !replayPort && actionButton()?.dataset.configured !== 'true') {
      void replayDestination().then(url => { if (url) { discoveredReplay = true; exposeActions(); } });
    }
    void refreshStatus();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init, { once: true });
  else init();
  setInterval(() => void refreshStatus(), 5000);
})();
