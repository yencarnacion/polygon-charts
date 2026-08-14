(() => {
  'use strict';

  const STATUS_ENDPOINT = '/api/integrations/tradingview/status';
  const TICKER_ENDPOINT = '/api/integrations/tradingview/ticker';
  const SYMBOL_PATTERN = /^[A-Z][A-Z0-9.-]{0,15}$/;

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

  function exposeTradingViewOnlyAction() {
    const button = actionButton();
    if (!button || !integrationEnabled) return;

    const feedback = document.getElementById('symbolActionFeedback');
    const wasHidden = button.classList.contains('d-none');
    button.classList.remove('d-none');
    if (feedback) feedback.classList.remove('d-none');
    if (wasHidden) {
      button.textContent = 'Load in TradingView';
      button.title = 'Change the active chart in TradingView Desktop';
    } else {
      button.title = 'Run the configured symbol action and also change TradingView Desktop';
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

    exposeTradingViewOnlyAction();
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

  // Capture before the existing symbol-action listener. The original
  // Tape/Yamir Trading Tools request and this TradingView request then proceed
  // independently; either destination can be offline without blocking the other.
  document.addEventListener('click', event => {
    const target = event.target instanceof Element ? event.target.closest('#runSymbolAction') : null;
    if (!target) return;
    void sendToTradingView(currentSymbol());
  }, true);

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => void refreshStatus(), { once: true });
  } else {
    void refreshStatus();
  }
  setInterval(() => void refreshStatus(), 5000);
})();
