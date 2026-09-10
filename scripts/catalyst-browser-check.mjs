// Requires Chrome and Node 22+. Set TABULATOR_JS to a local Tabulator 5.4.4 bundle for offline checks.
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const root = fileURLToPath(new URL('../', import.meta.url));
const read = name => readFileSync(join(root, name), 'utf8');
const helper = read('catalyst-ui.js');
new vm.Script(helper);
for (const file of ['index.html', 'chart.html']) {
  const page = read(file);
  for (const match of page.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)) {
    new vm.Script(match[1].replace(/\{\{\w+\}\}/g, 'null'), {filename: file});
  }
  const tables = [...page.matchAll(/new Tabulator\("(#[\w-]*(?:news|filings)-sheet)", \{([\s\S]*?)data:/g)];
  assert.equal(tables.length, file === 'index.html' ? 4 : 6);
  for (const [,selector, options] of tables) assert.ok(options.includes(`catalystTableOptions("${selector}"`));
  assert.ok(!page.includes('formatter: "link"'), 'every article/filing link must use the new-tab formatter');
}
const tabulator = process.env.TABULATOR_JS ? readFileSync(process.env.TABULATOR_JS, 'utf8') : await (await fetch('https://cdn.jsdelivr.net/npm/tabulator-tables@5.4.4/dist/js/tabulator.min.js')).text();
const tabulatorCSS = process.env.TABULATOR_JS ? readFileSync(process.env.TABULATOR_JS.replace('.js', '.css'), 'utf8') : await (await fetch('https://cdn.jsdelivr.net/npm/tabulator-tables@5.4.4/dist/css/tabulator.min.css')).text();
const css = read('index.html').match(/<style>([\s\S]*?)<\/style>/)[1];
const fixture = `<!doctype html><html><head><base href="http://127.0.0.1:8081/"><style>${tabulatorCSS}\n${css}\n${read('catalyst-ui.css')}\nbody{padding:24px}h2{font-size:20px;margin:16px 0}.tabulator{font-size:14px}.tabulator-tableholder{overflow:auto}.tabulator-cell{display:inline-block;padding:8px}.tabulator-row{white-space:nowrap}.tabulator-col{display:inline-block;padding:8px}.tabulator-header{overflow:hidden}.tabulator-row.tabulator-group{white-space:normal}.tabulator-col-content{position:relative}.tabulator-col-resize-handle{display:none}</style></head><body><h2>News · AAPL · Sep 9, 2026</h2><div id="news"></div><h2>SEC filings</h2><div id="filings"></div><script>${tabulator}</script><script>${helper}</script><script>
const cutoff = '2026-09-08T15:59:00-04:00';
const news = [
 {title:'Apple unveils iPhone Duo',source:'Business Wire · RTPR',url:'/api/rtpr/article?id=sample',published:'2026-09-09T18:15:00Z',catalyst_period:'recent',catalyst_cutoff:cutoff},
 {title:'Company announces expanded partnership',source:'News wire',url:'https://example.com/partnership',published:'2026-09-08T20:15:00Z',catalyst_period:'recent',catalyst_cutoff:cutoff},
 {title:'Company releases quarterly business update',source:'Benzinga',url:'https://example.com/update',published:'2026-09-08T19:30:00Z',catalyst_period:'older',catalyst_cutoff:cutoff},
 {title:'Time unavailable <img src=x onerror=alert(1)>',source:'News wire',url:'javascript:alert(1)',published:'',catalyst_period:'unknown',catalyst_cutoff:cutoff}
];
const cols=[{title:'PUBLISHED',field:'published',width:250,formatter:catalystTimeFormatter},{title:'TITLE',field:'title',widthGrow:3,formatter:catalystLinkFormatter,formatterParams:{labelField:'title',urlField:'url'}},{title:'SOURCE',field:'source',width:190}];
window.newsTable=new Tabulator('#news',{...catalystTableOptions('#news'),data:news,layout:'fitColumns',columns:cols});
window.filingsTable=new Tabulator('#filings',{...catalystTableOptions('#filings'),data:[{...news[1],title:'8-K · Material agreement',source:'SEC'},{...news[2],title:'4 · Insider transaction',source:'SEC'}],layout:'fitColumns',columns:cols});
</script></body></html>`;
const profile = mkdtempSync(join(tmpdir(),'polygon-catalyst-chrome-'));
const port = 19371;
const browser = spawn(process.env.CHROME || 'google-chrome',['--headless=new','--no-sandbox','--disable-gpu','--remote-allow-origins=*',`--remote-debugging-port=${port}`,`--user-data-dir=${profile}`,'about:blank'],{stdio:'ignore'});
const pending=new Map();let seq=0;let socket;
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
async function command(method,params={}) { return await new Promise((resolve,reject)=>{ const id=++seq;const timer=setTimeout(()=>{pending.delete(id);reject(new Error('CDP timed out: '+method));},10000);pending.set(id,{resolve:r=>{clearTimeout(timer);resolve(r)},reject});socket.send(JSON.stringify({id,method,params})); }); }
async function evaluate(expression) { const r=await command('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});if(r.exceptionDetails)throw new Error(JSON.stringify(r.exceptionDetails));return r.result.value; }
try {
 let pages;
 for(let n=0;n<100;n++){try{pages=await(await fetch(`http://127.0.0.1:${port}/json/list`)).json();break;}catch{await delay(50)}}
 assert.ok(pages?.length,'Chrome did not start');
 socket=new WebSocket(pages.find(page => page.type === 'page').webSocketDebuggerUrl);
 await new Promise((resolve,reject)=>{socket.onopen=resolve;socket.onerror=reject});
 socket.onmessage=event=>{const msg=JSON.parse(event.data);if(msg.method==='Runtime.exceptionThrown')console.error(JSON.stringify(msg.params));if(msg.id&&pending.has(msg.id)){const p=pending.get(msg.id);pending.delete(msg.id);msg.error?p.reject(msg.error):p.resolve(msg.result)}};
 await command('Runtime.enable');
 await command('Emulation.setTimezoneOverride',{timezoneId:'Asia/Tokyo'});
 await command('Emulation.setDeviceMetricsOverride',{width:1400,height:900,deviceScaleFactor:1,mobile:false});
 const {frameTree}=await command('Page.getFrameTree');
 await command('Page.setDocumentContent',{frameId:frameTree.frame.id,html:fixture});
 for(let n=0;n<100;n++){if(await evaluate('!!window.newsTable && document.querySelectorAll("#news .tabulator-cell").length >= 12'))break;await delay(50)}

 const state=await evaluate(`({groups:[...document.querySelectorAll('#news .catalyst-group')].map(e=>e.textContent),recent:document.querySelectorAll('#news .catalyst-recent').length,links:[...document.querySelectorAll('.tabulator-cell a')].map(a=>({target:a.target,rel:a.rel})),stripe:getComputedStyle(document.querySelector('#news .catalyst-recent .tabulator-cell')).boxShadow,text:document.querySelector('#news').textContent,images:document.querySelectorAll('#news img').length})`);
 assert.equal(state.groups.length,3);assert.equal(state.recent,2);assert.ok(state.groups[0].startsWith('AFTER Sep 8, 2026'));
 assert.ok(state.groups[1].startsWith('AT / BEFORE Sep 8, 2026'));assert.ok(state.text.includes('02:15:00 PM ET'),'timestamps must stay in NY when browser is in Tokyo');
 assert.ok(state.stripe.includes('inset'));assert.equal(state.images,0);assert.ok(state.links.every(a=>a.target==='_blank'&&a.rel.includes('noopener')));
 const screenshot=await command('Page.captureScreenshot',{format:'png'});writeFileSync('/tmp/polygon-catalyst-preview.png',Buffer.from(screenshot.data,'base64'));
 await evaluate(`newsTable.setData([{title:'Replay older row',published:'2026-09-08T19:00:00Z',catalyst_period:'older',catalyst_cutoff:'2026-09-08T15:59:00-04:00'}])`);
 assert.equal(await evaluate(`document.querySelectorAll('#news .catalyst-recent').length`),0,'replay update must clear old highlights');
 console.log('PASS: all 10 table integrations, JS syntax, grouping, NY timezone, new-tab links, escaped titles, replay refresh. Screenshot: /tmp/polygon-catalyst-preview.png');
} finally { socket?.close();browser.kill('SIGTERM');await delay(200);rmSync(profile,{recursive:true,force:true}); }
