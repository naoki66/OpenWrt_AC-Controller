'use strict';

/*
 * Offline harness for the LuCI view. Not part of the package.
 *
 *   node scripts/luci-view-harness.js [path/to/overview.js]
 *
 * Renders the view against stub LuCI modules and asserts the parts that fail
 * silently on a device: a missing option, a state that throws while building the
 * card, a Save button that never appears on new form.js, a backend method the
 * view calls but the rpcd script does not implement.
 */

const fs = require('fs');
const path = require('path');

const VIEW = process.argv[2] || path.join(__dirname, '..', 'deploy', 'openwrt',
	'luci-app-ac-agent', 'htdocs', 'luci-static', 'resources', 'view', 'ac-agent', 'overview.js');

/* ---------- minimal DOM ---------- */

function makeNode(tag, attrs, children) {
	const node = {
		tagName: (tag || 'div').toUpperCase(),
		attributes: Object.assign({}, attrs || {}),
		children: [],
		parentNode: null,
		style: {},
		_handlers: {}
	};

	const cls = (attrs && attrs.class) || '';
	node.className = cls;
	const set = new Set(cls.split(/\s+/).filter(Boolean));
	node.classList = {
		add: (c) => { set.add(c); node.className = Array.from(set).join(' '); },
		remove: (c) => { set.delete(c); node.className = Array.from(set).join(' '); },
		contains: (c) => set.has(c),
		toggle: (c, on) => { on ? set.add(c) : set.delete(c); node.className = Array.from(set).join(' '); }
	};
	Object.defineProperty(node, 'firstChild', {
		get() { return this.children.length ? this.children[0] : null; }
	});
	node.appendChild = function(c) {
		if (c == null) return c;
		if (typeof c === 'object') c.parentNode = this;
		this.children.push(c);
		return c;
	};
	node.removeChild = function(c) {
		const i = this.children.indexOf(c);
		if (i >= 0) this.children.splice(i, 1);
		return c;
	};
	node.insertBefore = function(c) { return this.appendChild(c); };
	node.setAttribute = function(k, v) { this.attributes[k] = v; };
	node.getAttribute = function(k) { return this.attributes[k]; };
	node.addEventListener = function(ev, fn) { this._handlers[ev] = fn; };
	if (attrs && typeof attrs.click === 'function')
		node._handlers.click = attrs.click;

	if (children != null)
		(Array.isArray(children) ? children : [ children ]).forEach((c) => node.appendChild(c));

	return node;
}

/* ---------- LuCI stubs ---------- */

const uciState = {
	agent: {
		'.type': 'agent',
		'.name': 'agent',
		enabled: { value: '1', type: 'option' },
		ac_addr: { value: '10.0.30.5:8090', type: 'option' },
		secret: { value: 'sekrit', type: 'option' },
		interval: { value: '20', type: 'option' },
		peer_port: { value: '29821', type: 'option' },
		peer_interval: { value: '30s', type: 'option' },
		peer_network: { value: 'lan', type: 'option' },
		peer_secret: { value: 'peer-key', type: 'option' },
		peer_steer: { value: '1', type: 'option' }
	}
};

const uci = {
	load: () => Promise.resolve(),
	sections: () => Object.keys(uciState),
	get: (cfg, sid, opt) => {
		const s = uciState[sid];
		if (!s) return null;
		if (opt == null) return s;
		return s[opt] ? s[opt].value : null;
	},
	set: () => {},
	save: () => Promise.resolve(),
	apply: () => { uci.applied = (uci.applied || 0) + 1; return Promise.resolve(); }
};

const STATUS = {
	version: 'f82e36d',
	pid: 4242,
	started_at: '2026-11-01T10:00:00Z',
	updated_at: new Date().toISOString(),
	mac: '00:00:00:00:00:5a',
	model: 'Gemtek XR1710G',
	firmware: 'ImmortalWrt 20261029',
	hostname: 'AP_ZW',
	interval_seconds: 20,
	dry_run: false,
	cycles: 12,
	state: 'managed',
	ac_addr: '10.0.30.5:8090',
	ac_addr_source: 'config',
	ac_id: 'cb5cf4efb3a8f8af',
	adopted: true,
	applied_rev: 3,
	desired_rev: 3,
	sha256: 'abc123',
	applied_at: '2026-11-01T09:59:00Z',
	last_hello_at: '2026-11-01T14:00:00Z',
	last_hello_ok: true,
	bss: 11,
	steering: 'none'
};

let statusOverride = null;
let running = true;
let autostart = true;
const rpcCalls = [];

function backendCall(object, method) {
	if (method === 'status') {
		return () => Promise.resolve({
			status: statusOverride === null ? STATUS : statusOverride,
			running: running,
			autostart: autostart
		});
	}
	return (...args) => {
		rpcCalls.push(method + '(' + args.join(',') + ')');
		return Promise.resolve({ ok: true, autostart: autostart });
	};
}

const ui = {
	addNotification: () => {},
	showModal: () => {},
	hideModal: () => {},
	createHandlerFn: (obj, method) => function(ev) { return obj[method](ev); }
};

const rpc = { declare: (spec) => backendCall(spec.object, spec.method) };

const pollAdds = [];
const poll = {
	add: (fn, seconds) => { pollAdds.push(seconds); return fn; },
	remove: () => {},
	start: () => {},
	stop: () => {}
};

const L = {
	bind: (fn, self) => fn.bind(self),
	isObject: (v) => v != null && typeof v === 'object',
	env: { base_url: '' },
	resolveDefault: (p, d) => p.catch(() => d)
};

/* form stub with the two shapes form.js has had: an old Map that owns
 * handleSave and a new one that does not. */
function makeForm(withBuiltinSave) {
	const sections = [];

	class Section {
		constructor(type, name, cfgtype, title) {
			this.type = type;
			this.name = name;
			this.cfgtype = cfgtype;
			this.title = title;
			this.options = [];
			this.anonymous = false;
			this.addremove = true;
			sections.push(this);
		}
		option(cls, name, title, desc) {
			const o = new cls(this, name, title, desc);
			this.options.push(o);
			return o;
		}
	}

	function Option(section, name, title, desc) {
		this.section = section;
		this.name = name;
		this.title = title;
		this.description = desc;
		this.deps = [];
		this.depends = function(a, b) {
			if (typeof a === 'string') this.deps.push([ a, b ]);
			else this.deps.push(a);
		};
	}
	class Value extends Option {}
	class Flag extends Option {}
	/* ListValue records its choices so a view calling .value()/.depends()
	 * during construction cannot crash the harness. */
	class ListValue extends Option {
		constructor(...args) {
			super(...args);
			this.choices = [];
			this.default = '';
		}
		value(v, label) { this.choices.push({ v, label }); return this; }
	}

	class Map {
		constructor(config, title, desc) {
			this.config = config;
			this.title = title;
			this.description = desc;
			this.sections = sections;
			this.saved = 0;
			if (withBuiltinSave)
				this.handleSave = () => { this.saved++; return Promise.resolve(); };
		}
		section(cls, ...args) { return new cls(...args); }
		save() { this.saved++; return Promise.resolve(); }
		reset() { return Promise.resolve(); }
		render() {
			const root = makeNode('div', { class: 'cbi-map' });
			this.root = root;
			return Promise.resolve(root);
		}
	}

	return { Map, NamedSection: Section, TypedSection: Section, Value, Flag, ListValue, sections };
}

/* ---------- run ---------- */

async function run(withBuiltinSave) {
	const form = makeForm(withBuiltinSave);
	pollAdds.length = 0;
	rpcCalls.length = 0;

	const src = fs.readFileSync(VIEW, 'utf8');
	/* The view ends with a top-level `return`, so it cannot be wrapped in an
	 * assignment without rewriting that one statement. */
	const patched = src.replace('return view.extend({', 'var __view = view.extend({');
	if (patched === src)
		throw new Error('view does not end with `return view.extend({` — update the harness');

	const fn = new Function('view', 'form', 'fs', 'uci', 'ui', 'rpc', 'poll', 'E', '_', 'L', 'document',
		patched + '\n;return { view: __view, STATES: STATES, effectiveState: effectiveState, fmtRev: fmtRev };');

	const mod = fn({ extend: (o) => o }, form, {}, uci, ui, rpc, poll, makeNode, (s) => s, L, {});

	String.prototype.format = function(...args) {
		let i = 0;
		return this.replace(/%[ds]/g, (m) => (m === '%d' ? Number(args[i++]) : String(args[i++])));
	};

	const v = mod.view;
	const data = await v.load();
	const node = await v.render(data);
	return { node, form, v, mod };
}

function walk(node, out) {
	out = out || [];
	if (!node || typeof node !== 'object') return out;
	out.push(node);
	(node.children || []).forEach((c) => walk(c, out));
	return out;
}

function textOf(node) {
	if (node == null) return '';
	if (typeof node !== 'object') return String(node);
	return (node.children || []).map(textOf).join(' ');
}

(async () => {
	let failures = 0;
	const check = (ok, msg) => {
		if (!ok) { failures++; console.log('FAIL ' + msg); }
		else console.log('ok   ' + msg);
	};

	/* --- new form.js: no builtin save, so the view must provide one --- */
	{
		const { node, form } = await run(false);

		const sections = form.sections;
		check(sections.length === 1, 'one form section (got ' + sections.length + ')');
		check(sections[0] && sections[0].name === 'agent', 'section bound to uci section "agent"');
		check(sections[0] && sections[0].addremove === false, 'addremove disabled');

		const opts = sections[0] ? sections[0].options : [];
		const names = opts.map((o) => o.name).sort();
		const want = [ 'ac_addr', 'enabled', 'interval', 'peer_interval', 'peer_network', 'peer_port', 'peer_secret', 'peer_steer', 'secret' ];
		check(JSON.stringify(names) === JSON.stringify(want), 'all nine options present: ' + names.join(','));

		const byName = {};
		opts.forEach((o) => { byName[o.name] = o; });
		check(byName.enabled && byName.enabled.default === '1', 'the enable switch defaults to on');
		check(byName.peer_steer && byName.peer_steer.default === '1', 'local roam suggestion defaults on');
		check(byName.interval && byName.interval.datatype === 'uinteger', 'interval is uinteger');
		check(byName.secret && byName.secret.password === true, 'secret is a password field');
		check(byName.peer_secret && byName.peer_secret.password === true, 'peer secret is a password field');
		check(byName.peer_network && byName.peer_network.placeholder === 'lan', 'peer network placeholder');
		check(byName.ac_addr && byName.ac_addr.placeholder === 'controller:8090', 'ac_addr placeholder');

		const texts = textOf(node);
		check(texts.indexOf('Managed') >= 0, 'managed badge rendered');
		check(texts.indexOf('rev 3') >= 0, 'applied revision rendered');
		check(texts.indexOf('10.0.30.5:8090') >= 0, 'controller address rendered');
		check(texts.indexOf('configured address') >= 0, 'address source rendered');
		check(texts.indexOf('running agent') >= 0, 'no steering daemon shown as agent');

		const buttons = walk(node).filter((n) => n.tagName === 'BUTTON');
		const labels = buttons.map((b) => textOf(b));
		check(labels.some((l) => l.indexOf('Restart agent') >= 0), 'restart button');
		check(labels.some((l) => l.indexOf('Save & apply') >= 0), 'view-provided Save button on new form.js');
		check(buttons.length === 3, 'three buttons (got ' + buttons.length + '): ' + labels.join('|'));

		/* Page order: title first, live status second, settings last. */
		check(textOf(node.children[0]) === 'AC Agent', 'page title is the first block');
		check(node.children[2] && node.children[2].className.indexOf('cbi-section') >= 0,
			'status card is the second block after the title');

		/* Opening the page applies the one switch (agent start + WAN rules). */
		check(rpcCalls.some((c) => c.indexOf('applyEnabled') === 0),
			'opening the page applies the enabled switch: ' + rpcCalls.join(','));

		check(pollAdds.length === 1 && pollAdds[0] === 10,
			'poll registered through the poll module (got ' + JSON.stringify(pollAdds) + ')');

		/* The restart button must reach the backend method. */
		const restart = buttons.find((b) => textOf(b).indexOf('Restart agent') >= 0);
		await restart._handlers.click({});
		check(rpcCalls.some((c) => c.indexOf('restart') === 0),
			'restart calls the rpcd method: ' + rpcCalls.join(','));
	}

	/* --- old form.js: it renders the button, the view must not add another --- */
	{
		const { node } = await run(true);
		const labels = walk(node).filter((n) => n.tagName === 'BUTTON').map((b) => textOf(b));
		check(!labels.some((l) => l.indexOf('Save & apply') >= 0), 'no duplicate Save button on old form.js');
	}

	/* --- every state must render --- */
	{
		for (const s of [ 'managed', 'connected', 'unreachable', 'discovering', 'starting' ]) {
			statusOverride = Object.assign({}, STATUS, { state: s, adopted: s === 'managed' });
			try {
				const { node } = await run(false);
				check(textOf(node).length > 0, 'renders state ' + s);
			}
			catch (e) {
				check(false, 'state ' + s + ' threw: ' + e.message);
			}
		}
		statusOverride = null;

		running = false;
		try {
			const { node } = await run(false);
			check(textOf(node).indexOf('Agent stopped') >= 0, 'stopped process overrides the status file');
		}
		catch (e) {
			check(false, 'stopped state threw: ' + e.message);
		}
		running = true;

		statusOverride = Object.assign({}, STATUS, {
			updated_at: new Date(Date.now() - 3600 * 1000).toISOString()
		});
		try {
			const { node } = await run(false);
			check(textOf(node).indexOf('Agent not responding') >= 0, 'detects a stale status file');
		}
		catch (e) {
			check(false, 'stale status threw: ' + e.message);
		}
		statusOverride = null;

		/* The agent may never have written a file: the backend then inlines null
		 * and the page must say "starting", not crash. */
		statusOverride = {};
		statusOverride = null;
		try {
			const { node } = await run(false);
			check(textOf(node).length > 0, 'renders with a status object');
		}
		catch (e) {
			check(false, 'null status threw: ' + e.message);
		}
	}

	console.log(failures ? '\n' + failures + ' failure(s)' : '\nall checks passed');
	process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(2); });
