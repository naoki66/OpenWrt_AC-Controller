// 用最小 DOM 替身验证内嵌页面交互，无需额外安装浏览器。
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
class Element {
  constructor() { this.children = []; this.value = ''; this.listeners = {}; }
  appendChild(child) { this.children.push(child); }
  replaceChildren() { this.children = []; }
  addEventListener(event, fn) { this.listeners[event] = fn; }
}
const elements = new Map();
const get = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
const template = {id:'test',name:'Test',mesh:{mode:'80211s',band:'5g',id:'test',key:'secret-secret',network:'lan'}};
const now = Math.floor(Date.now()/1000);
let response = {nodes:[{ap_mac:'a',updated_at:now,interfaces:[{radio:'radio1',ifname:'mesh0',up:true,peers:[{mac:'02:00:00:00:00:01',established:true,signal_dbm:-60}]}]}]};
let saved;
const context = vm.createContext({
  $, document:{createElement:()=>new Element()}, templates:[template], loadTemplates:async()=>{},
  loggedIn:true,currentView:'mesh',deviceCache:[{mac:'a',hostname:'<img src=x onerror=alert(1)>',ip:'192.168.1.2'}],
  api:async(method,path,body)=>{if(method==='PUT')saved={method,path,body};return response;},
  Date,Map,Error,encodeURIComponent,
});
function $(id) {return get(id);}
vm.runInContext(fs.readFileSync('controller/internal/api/web/mesh.js','utf8'),context);
(async()=>{
  get('mesh-template').value='test';
  context.fillMeshForm();
  assert.equal(get('mesh-id').value,'test');
  await context.refreshMeshStatus();
  const card=get('mesh-topology').children[0];
  assert.equal(card.children[0].textContent,'<img src=x onerror=alert(1)>');
  assert.ok(card.children.some(e=>e.textContent && e.textContent.includes('-60 dBm')));
  response.nodes[0].updated_at=now-1000;
  await context.refreshMeshStatus();
  assert.ok(get('mesh-topology').children[0].children.some(e=>e.textContent==='状态未知 / 上报已过期'));
  get('mesh-mode').value='wired';
  await get('mesh-save').listeners.click();
  assert.equal(saved.path,'/api/v1/templates/test/mesh');
  assert.deepEqual(JSON.parse(JSON.stringify(saved.body)),{mesh:{mode:'wired'}});
  assert.equal(get('mesh-save').disabled,false);
  console.log('Mesh UI: config save, measured links, stale state and literal node labels passed');
})().catch(e=>{console.error(e);process.exitCode=1;});
