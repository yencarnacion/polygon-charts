// Shared by every news/SEC table, including rows revealed during replay.
function catalystTableOptions(selector, warnings = []) {
  const container = document.querySelector(selector);
  if (container) {
    const noticeId = container.id + '-coverage';
    let notice = document.getElementById(noticeId);
    if (!notice) {
      notice = document.createElement('div');
      notice.id = noticeId;
      notice.className = 'catalyst-coverage';
      notice.setAttribute('role', 'status');
      container.before(notice);
    }
    notice.textContent = warnings.join(' ');
    notice.hidden = warnings.length === 0;
  }
  return {
    groupBy: row => row.catalyst_period || 'unknown',
    groupStartOpen: true,
    groupToggleElement: false,
    groupHeader: (period, count, rows) => {
      const rawCutoff = rows[0]?.catalyst_cutoff;
      const cutoff = rawCutoff ? new Date(rawCutoff) : null;
      const date = cutoff && Number.isFinite(cutoff.getTime())
        ? cutoff.toLocaleDateString('en-US', {timeZone: 'America/New_York', month: 'short', day: 'numeric', year: 'numeric'}) : '';
      const label = document.createElement('span');
      label.className = 'catalyst-group catalyst-group-' + (['recent', 'older'].includes(period) ? period : 'unknown');
      label.textContent = period === 'recent' ? `AFTER ${date} · 3:59 PM ET — ${count}`
        : period === 'older' ? `AT / BEFORE ${date} · 3:59 PM ET — ${count}`
        : `TIME / CUTOFF UNAVAILABLE — ${count}`;
      return label.outerHTML;
    },
    rowFormatter: row => {
      const el = row.getElement();
      const recent = row.getData().catalyst_period === 'recent';
      el.classList.toggle('catalyst-recent', recent);
    },
  };
}

function catalystTimeFormatter(cell) {
  const raw = cell.getValue();
  const date = raw ? new Date(raw) : null;
  if (!date || !Number.isFinite(date.getTime())) return 'Time unavailable';
  return date.toLocaleString('en-US', {
    timeZone: 'America/New_York', month: '2-digit', day: '2-digit', year: 'numeric',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: true,
  }) + ' ET';
}

function catalystLinkFormatter(cell, params) {
  const data = cell.getRow().getData();
  const link = document.createElement('a');
  link.textContent = data[params.labelField] || '';
  link.target = '_blank';
  link.rel = 'noopener noreferrer';
  try {
    const raw = data[params.urlField];
    if (typeof raw !== 'string' || !raw.trim()) return link;
    const url = new URL(raw, document.baseURI);
    if (url.protocol === 'https:' || url.protocol === 'http:') link.href = url.href;
  } catch (_) { /* Keep an unreadable URL as plain text. */ }
  return link;
}
