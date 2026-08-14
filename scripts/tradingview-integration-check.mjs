#!/usr/bin/env node

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(here, '..', 'tradingview-integration.js'), 'utf8');
const failures = [];

function check(name, ok, detail = '') {
  if (!ok) failures.push(`${name}${detail ? `: ${detail}` : ''}`);
}

class FakeClassList {
  constructor(...names) { this.names = new Set(names); }
  add(...names) { names.forEach(name => this.names.add(name)); }
  remove(...names) { names.forEach(name => this.names.delete(name)); }
  contains(name) { return this.names.has(name); }
}

class FakeElement {
  constructor(id = '') {
    this.id = id;
    this.hidden = false;
    this.textContent = '';
    this.title = '';
    this.style = {};
    this.classList = new FakeClassList();
    this.attributes = {};
  }
  setAttribute(name, value) { this.attributes[name] = value; }
  insertAdjacentElement(_position, element) { this.owner.set(element.id, element); }
  closest(selector) { return selector === '#runSymbolAction' && this.id === 'runSymbolAction' ? this : null; }
}

async function flush() {
  for (let i = 0; i < 30; i += 1) await Promise.resolve();
}

async function run({ enabled, connected = enabled, hiddenButton = false }) {
  const elements = new Map();
  const button = new FakeElement('runSymbolAction');
  button.owner = elements;
  if (hiddenButton) button.classList.add('d-none');
  button.textContent = hiddenButton ? 'Send symbol' : 'Load in Tape + Trading';
  const feedback = new FakeElement('symbolActionFeedback');
  feedback.owner = elements;
  if (hiddenButton) feedback.classList.add('d-none');
  elements.set(button.id, button);
  elements.set(feedback.id, feedback);

  let clickHandler = null;
  const document = {
    readyState: 'complete',
    getElementById: id => elements.get(id) || null,
    createElement: () => {
      const element = new FakeElement();
      element.owner = elements;
      return element;
    },
    addEventListener(type, handler) {
      if (type === 'click') clickHandler = handler;
    }
  };

  const calls = [];
  const fetch = async (url, options = {}) => {
    calls.push({ url: String(url), method: options.method || 'GET', body: options.body || '' });
    if (String(url).endsWith('/status')) {
      return {
        ok: true,
        json: async () => ({ enabled, connected }),
        text: async () => ''
      };
    }
    if (String(url).endsWith('/ticker')) {
      return {
        ok: true,
        json: async () => ({ symbol: 'AAPL' }),
        text: async () => ''
      };
    }
    throw new Error(`unexpected URL ${url}`);
  };

  const context = {
    window: { location: { search: '?ticker=aapl&date=2026-08-14' } },
    document,
    fetch,
    Element: FakeElement,
    URLSearchParams,
    AbortController,
    setInterval: () => 0,
    console
  };
  vm.createContext(context);
  vm.runInContext(source, context);
  await flush();

  return {
    calls,
    button,
    status: elements.get('tradingViewIntegrationStatus'),
    async click() {
      clickHandler({ target: button });
      await flush();
    }
  };
}

let harness = await run({ enabled: true, hiddenButton: false });
check('enabled status is visible', harness.status && harness.status.textContent === 'TV READY', harness.status?.textContent);
await harness.click();
const tickerCalls = harness.calls.filter(call => call.url.endsWith('/ticker'));
check('click sends one TradingView request', tickerCalls.length === 1, JSON.stringify(harness.calls));
check('click normalizes URL symbol', tickerCalls[0]?.body === '{"symbol":"AAPL"}', tickerCalls[0]?.body);
check('success shows selected symbol', harness.status?.textContent === 'TV AAPL', harness.status?.textContent);

harness = await run({ enabled: false, connected: false });
await harness.click();
check('disabled integration stays hidden', harness.status?.hidden === true);
check('disabled integration sends no ticker request', harness.calls.every(call => !call.url.endsWith('/ticker')), JSON.stringify(harness.calls));

harness = await run({ enabled: true, hiddenButton: true });
check('TradingView-only setup reveals action button', !harness.button.classList.contains('d-none'));
check('TradingView-only setup gives clear label', harness.button.textContent === 'Load in TradingView', harness.button.textContent);

if (failures.length) {
  console.error(`tradingview-integration-check FAILED:\n  ${failures.join('\n  ')}`);
  process.exit(1);
}
console.log('tradingview-integration-check ok');
