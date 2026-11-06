'use strict';
'require view';
'require poll';
'require rpc';
'require form';
'require uci';
'require ui';

/*
 * AC agent status page.
 *
 * All device state comes from the luci.ac_agent rpcd backend, which merges the
 * agent's status file with the process and rc.d state. The view never reads a
 * file or executes a command itself: the ACL then grants named methods instead
 * of "run this binary", and the merge of "process alive" and "file fresh"
 * happens in one place instead of being re-derived in JavaScript.
 *
 * The badge wording lives here, not in the backend, because it has to be
 * translatable: the backend returns a closed set of state names and this file
 * maps them through _().
 */

var POLL_INTERVAL = 10;

var callStatus = rpc.declare({ object: 'luci.ac_agent', method: 'status' });
var callRestart = rpc.declare({ object: 'luci.ac_agent', method: 'restart' });
var callApplyEnabled = rpc.declare({ object: 'luci.ac_agent', method: 'applyEnabled' });

/* The agent's closed state set; see internal/agent/status.go. */
var STATES = {
	managed:     { label: _('Managed'),                       color: '#2e7d32' },
	connected:   { label: _('Connected, awaiting adoption'),  color: '#ef6c00' },
	unreachable: { label: _('Controller unreachable'),        color: '#c62828' },
	discovering: { label: _('No controller found'),           color: '#c62828' },
	starting:    { label: _('Starting'),                      color: '#546e7a' },
	stale:       { label: _('Agent not responding'),          color: '#455a64' },
	stopped:     { label: _('Agent stopped'),                 color: '#455a64' }
};

/* A status file older than three heartbeats means the process stopped writing:
 * either it is gone or it is wedged. Ninety seconds is the floor so a very short
 * interval cannot make the page flicker. */
function isStale(status) {
	if (!status || !status.updated_at)
		return true;

	var written = Date.parse(status.updated_at);
	if (isNaN(written))
		return true;

	var limit = Math.max(3 * (status.interval_seconds || 20), 90) * 1000;
	return (Date.now() - written) > limit;
}

function effectiveState(data) {
	var status = data && data.status;

	if (!data || data.running !== true)
		return 'stopped';
	if (!status)
		return 'starting';
	if (isStale(status))
		return 'stale';
	return STATES[status.state] ? status.state : 'starting';
}

function sourceLabel(source) {
	switch (source) {
	case 'config':     return _('configured address');
	case 'discovered': return _('discovered by broadcast');
	default:           return '-';
	}
}

var STEERING_MODES = {
	ac_priority: _('Controller first'),
	ac_only:     _('Controller only'),
	autonomous:  _('Autonomous')
};

function fmtTime(iso) {
	if (!iso)
		return '-';

	var t = Date.parse(iso);
	return isNaN(t) ? iso : new Date(t).toLocaleString();
}

function fmtRev(status) {
	if (!status)
		return '-';
	if (!status.applied_rev && !status.desired_rev)
		return _('nothing applied yet');
	if (status.desired_rev !== status.applied_rev)
		return _('rev %d applied, rev %d pending').format(status.applied_rev, status.desired_rev);
	return _('rev %d').format(status.applied_rev);
}

/* One sentence explaining the badge and, when it is bad, the most likely cause.
 * A red badge with no reason is worse than no badge. */
function stateHint(state, status) {
	switch (state) {
	case 'managed':
		return status && status.ac_id
			? _('This access point is running a revision issued by controller %s.').format(status.ac_id)
			: _('This access point is running a revision issued by the controller.');
	case 'connected':
		return (status && status.applied_rev > 0)
			? _('The controller no longer knows this access point, but rev %d is still running here. Adopt it again to resume management.').format(status.applied_rev)
			: _('The controller answers but has not adopted this access point yet. Adopt it in the controller interface; until then no configuration is pushed.');
	case 'unreachable':
		return status && status.last_hello_error
			? _('Last error: %s').format(status.last_hello_error)
			: _('The configured address does not answer.');
	case 'discovering':
		return _('No controller address is configured and no UDP answer arrived on the local segment. A controller behind a router never sees the broadcast, so enter its address above.');
	case 'stale':
		return _('The agent has not updated its status for more than three heartbeats. Check the process; a restart usually recovers it.');
	default:
		return _('The agent process is not running, so this access point is not under any controller.');
	}
}

function badge(text, color) {
	return E('span', {
		style: 'display:inline-block;padding:2px 10px;border-radius:10px;' +
			'color:#fff;font-weight:bold;background:' + color
	}, text);
}

function valueRow(title, value) {
	return E('div', { class: 'cbi-value' }, [
		E('label', { class: 'cbi-value-title' }, title),
		E('div', { class: 'cbi-value-field' }, value)
	]);
}

return view.extend({
	load: function() {
		/* Opening the page is the switch's apply action: reconcile the agent
		 * process and the WAN rules with the saved "enabled" value before
		 * reading the status, so a factory-fresh AP is usable from the first
		 * visit. A backend failure must not hide the status card. */
		return uci.load('ac').then(function() {
			return callApplyEnabled();
		}).catch(function() {
			/* ignored: the status card still renders and the poll retries */
		}).then(function() {
			return callStatus();
		}).then(function(data) {
			return { data: data || {} };
		});
	},

	render: function(res) {
		var self = this;

		this.data = res.data || {};
		this.card = E('div', { class: 'cbi-section' });

		/* The heading is rendered above the status card instead of by form.Map:
		 * the page order is title, status, settings. A map title would land
		 * after the card, because the map renders where the form is. */
		var m = new form.Map('ac', '', '');

		var s = m.section(form.NamedSection, 'agent', 'agent', _('Controller connection'));
		s.anonymous = true;
		s.addremove = false;

		/* One switch owns everything a fresh AP needs: start the agent at boot
		 * and open the WAN zone so an AC on either side can adopt it. It is on
		 * by default; the page applies it on open and on save. */
		var o = s.option(form.Flag, 'enabled', _('Enable'),
			_('Starts the agent, enables it at boot, and opens TCP 8090 plus UDP 29820/29821 on the WAN zone, ' +
			  'so an AC on the upstream or LAN side can discover and manage this access point during initial setup. ' +
			  'On by default.'));
		o.default = '1';
		o.rmempty = false;

		o = s.option(form.Value, 'ac_addr', _('Controller address'),
			_('host:port of the controller, for example 10.0.30.5:8090. ' +
			  'Leave empty to find the controller by UDP broadcast on the local segment; ' +
			  'a controller on the other side of a router must be entered explicitly, ' +
			  'because broadcast never crosses layer 3.'));
		o.placeholder = '10.0.30.5:8090';
		o.rmempty = true;

		o = s.option(form.Value, 'secret', _('Shared secret'),
			_('Must match AC_SHARED_SECRET on the controller. Leave empty when the controller does not check one.'));
		o.password = true;
		o.rmempty = true;

		o = s.option(form.Value, 'interval', _('Heartbeat interval'),
			_('Seconds between two polls. The default is 20.'));
		o.datatype = 'uinteger';
		o.placeholder = '20';
		o.rmempty = true;

		o = s.option(form.Flag, 'dry_run', _('Dry run'),
			_('Render and stage the configuration but never write /etc/config and never reload radios. ' +
			  'Use this to check that the controller can be reached before letting it take over.'));
		o.default = '0';
		o.rmempty = false;

		o = s.option(form.Value, 'peer_port', _('Peer coordination port'),
			_('UDP port used to exchange 802.11k data between access points directly. 0 disables it.'));
		o.datatype = 'uinteger';
		o.placeholder = '29821';
		o.rmempty = true;

		o = s.option(form.Value, 'peer_interval', _('Peer announcement interval'),
			_('How often this access point announces its BSS and client state to its peers, for example 30s.'));
		o.placeholder = '30s';
		o.rmempty = true;

		o = s.option(form.Flag, 'peer_steer', _('Local roam suggestion'),
			_('Let the agent itself send one 802.11v transition suggestion (BTM) when a client is very weak ' +
			  'and a peer serves the same SSID. This is built into the agent and needs no steering daemon. ' +
			  'It stays silent whenever a steering daemon (usteer or dawn) is enabled, so two decision-makers ' +
			  'never fight over the same client; nrsyncd only distributes 11k neighbour lists and does not compete.'));
		o.default = '0';
		o.rmempty = false;

		o = s.option(form.ListValue, 'steering_mode', _('Roaming steering policy'),
			_('Decides who runs client steering. While the controller answers, its directive always wins: ' +
			  'the daemon it names is enabled and dawn/usteer are stopped, so the two never fight over a client. ' +
			  '"Controller first" additionally hands steering to a local daemon when the controller is lost.'));
		o.value('ac_priority', _('Controller first (recommended)'));
		o.value('ac_only', _('Controller only, keep last state on loss'));
		o.value('autonomous', _('Autonomous: local daemons only'));
		o.default = 'ac_priority';

		o = s.option(form.ListValue, 'steering_fallback', _('Steering fallback on controller loss'),
			_('Which local daemon takes over after the controller has been unreachable for about a minute. ' +
			  'Steering returns to the controller automatically once it answers again. ' +
			  'Only used with the "Controller first" policy; "none" keeps the last controller state instead.'));
		o.value('none', _('No fallback'));
		o.value('usteer', 'usteer');
		o.value('dawn', 'dawn');
		o.value('nrsyncd', 'nrsyncd (11k only)');
		o.default = 'none';
		o.depends('steering_mode', 'ac_priority');

		/* Save, then apply: applying the ac config raises procd's reload trigger
		 * for this service, which restarts the agent with the new values.
		 *
		 * form.js before 24.10 renders its own Save button and calls
		 * map.handleSave; newer versions render no footer at all. Both are
		 * covered, and only one Save button ever appears. */
		var save = function() {
			return m.save().then(function() {
				return uci.apply();
			}).then(function() {
				/* The switch is applied after the config is committed:
				 * applyEnabled reads /etc/config/ac, so it must see the
				 * value just saved, not the pre-save one. */
				return callApplyEnabled();
			}).then(function(res) {
				if (res && res.ok === false)
					throw new Error((res && res.message) || 'unknown error');
				ui.addNotification(null, E('p', {},
					_('Settings saved. The agent and the WAN rules now follow the switch.')), 'info');
				return self.refresh();
			}).catch(function(err) {
				ui.addNotification(null, E('p', {},
					_('Could not save the settings: %s').format(err)), 'error');
			});
		};

		var hasBuiltinSave = (typeof m.handleSave === 'function');
		if (hasBuiltinSave)
			m.handleSave = save;

		return m.render().then(function(formNode) {
			/* Title, then the live status card, then the settings form. */
			var wrapper = E('div', {}, [
				E('h2', { name: 'content' }, _('AC Agent')),
				E('div', { class: 'cbi-map-descr' },
					_('Shows whether this access point is under controller management, and how it reaches the controller.')),
				self.card,
				formNode
			]);

			if (!hasBuiltinSave)
				wrapper.appendChild(E('div', { class: 'cbi-page-actions' }, [
					E('button', {
						class: 'btn cbi-button cbi-button-save',
						click: function(ev) {
							ev.preventDefault();
							return save();
						}
					}, _('Save & apply')),
					' ',
					E('button', {
						class: 'btn cbi-button',
						click: function(ev) {
							ev.preventDefault();
							return m.reset();
						}
					}, _('Reset'))
				]));

			self.fillCard();
			poll.add(function() {
				return self.refresh();
			}, POLL_INTERVAL);
			return wrapper;
		});
	},

	refresh: function() {
		var self = this;

		return callStatus().then(function(d) {
			self.data = d || {};
			self.fillCard();
		}, function() {
			/* A failing backend must not stop the poll loop: the next tick
			 * retries and the card keeps its previous contents. */
		});
	},

	fillCard: function() {
		var data = this.data || {};
		var status = data.status;
		var state = effectiveState(data);
		var meta = STATES[state] || STATES.starting;

		while (this.card.firstChild)
			this.card.removeChild(this.card.firstChild);

		this.card.appendChild(E('h3', {}, _('Control status')));

		this.card.appendChild(valueRow(_('Status'), [
			badge(meta.label, meta.color),
			E('div', { style: 'margin-top:6px;max-width:52em' }, stateHint(state, status))
		]));

		if (status) {
			this.card.appendChild(valueRow(_('Controller'),
				status.ac_addr
					? [ status.ac_addr, ' ', E('em', {}, '(' + sourceLabel(status.ac_addr_source) + ')') ]
					: '-'));

			this.card.appendChild(valueRow(_('Controller ID'), status.ac_id || '-'));
			this.card.appendChild(valueRow(_('Configuration'), fmtRev(status)));

			this.card.appendChild(valueRow(_('Last heartbeat'),
				status.last_hello_at
					? [ fmtTime(status.last_hello_at), ' ', status.last_hello_ok ? _('(accepted)') : _('(failed)') ]
					: '-'));

			this.card.appendChild(valueRow(_('Access point'),
				[ status.mac || '-', ' ', E('em', {}, status.model || '') ]));

			if (status.steering_mode || status.steering)
				this.card.appendChild(valueRow(_('Roaming steering'), [
					STEERING_MODES[status.steering_mode] || status.steering_mode || '-',
					status.steering ? ' · ' + _('running %s').format(status.steering) : ''
				]));

			if (status.steering_fallback_active)
				this.card.appendChild(E('div', { class: 'alert-message warning' },
					_('The controller was lost, so local steering daemon %s took over. ' +
					  'Steering returns to the controller automatically once it answers again.')
						.format(status.steering || '-')));

			if (status.bss)
				this.card.appendChild(valueRow(_('BSS reported'), String(status.bss)));

			this.card.appendChild(valueRow(_('Agent'),
				[ status.version || '-', ' ', _('since'), ' ', fmtTime(status.started_at) ]));

			if (status.dry_run)
				this.card.appendChild(E('div', { class: 'alert-message warning' },
					_('Dry run is on: the controller is polled and packages are staged, but this access point is not modified.')));

			/* Shown whenever the last exchange failed for a reason the badge does
			 * not already spell out (for instance a changed controller key). */
			if (status.last_hello_error && state !== 'unreachable')
				this.card.appendChild(E('div', { class: 'alert-message warning' },
					_('Last error: %s').format(status.last_hello_error)));
		}

		this.card.appendChild(E('div', { class: 'cbi-page-actions' }, [
			E('button', {
				class: 'btn cbi-button cbi-button-action',
				click: ui.createHandlerFn(this, 'handleRestart')
			}, _('Restart agent'))
		]));
	},

	/* Restarting runs one sync cycle immediately, which is what an operator wants
	 * after editing the controller address. There is deliberately no separate
	 * "sync now": a one-shot agent would race the resident process. */
	handleRestart: function() {
		var self = this;

		return callRestart().then(function(res) {
			ui.addNotification(null, E('p', {}, (res && res.ok)
				? _('Agent restarted.')
				: _('Could not restart the agent: %s').format((res && res.message) || 'unknown error')),
				(res && res.ok) ? 'info' : 'error');
			return self.refresh();
		}, function(err) {
			ui.addNotification(null, E('p', {},
				_('Could not restart the agent: %s').format(err)), 'error');
		});
	}
});
