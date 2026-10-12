/* Rôle : logique de l'interface (Alpine.js) : état temps réel, graphique, actions sur les torrents, réglages. */
/* RatioPilot UI — Alpine.js + uPlot, aucune étape de build. */
document.addEventListener('alpine:init', () => {
  Alpine.data('app', () => ({
    view: 'loading',
    authEnabled: false,
    version: '',
    connected: false,
    theme: 'dark',
    tab: 'torrents',
    snap: { running: false, torrents: [], speed: 0, globalRate: 0, sessionUp: 0, totalUp: 0, active: 0, port: 0, client: '' },
    config: {},
    form: {},
    clients: [],
    history: [],
    events: [],
    lastEventId: 0,
    unseenErrors: 0,
    open: {},
    dragOver: false,
    filter: { q: '', status: 'all' },
    logFilter: 'all',
    settingsDirty: false,
    settingsError: '',
    busy: { run: false, save: false },
    loginForm: { password: '', remember: true, error: '', busy: false },
    toasts: [],
    es: null,
    chart: null,
    chartEl: null,
    resizeObs: null,
    statusFilters: [
      { id: 'all', label: 'Tous' },
      { id: 'active', label: 'Actifs' },
      { id: 'queued', label: 'En attente' },
      { id: 'paused', label: 'En pause' },
      { id: 'ended', label: 'Terminés' },
      { id: 'problem', label: 'Problèmes' },
    ],
    logLevels: [
      { id: 'all', label: 'Tout' },
      { id: 'ok', label: 'Succès' },
      { id: 'warn', label: 'Alertes' },
      { id: 'error', label: 'Erreurs' },
    ],
    presets: [
      { label: 'Discret', min: 20, max: 80 },
      { label: 'ADSL', min: 30, max: 120 },
      { label: 'Fibre', min: 150, max: 600 },
      { label: 'Seedbox', min: 800, max: 3000 },
    ],

    // ---------- démarrage ----------
    async boot() {
      let saved = null;
      try { saved = localStorage.getItem('rp-theme'); } catch (_) {}
      this.theme = saved || (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
      document.documentElement.dataset.theme = this.theme;
      setInterval(() => { this.snap.now = Math.floor(Date.now() / 1000); }, 1000);
      const me = await fetch('api/me').then(r => r.json()).catch(() => ({}));
      this.authEnabled = !!me.authEnabled;
      this.version = me.version || '';
      if (!me.authenticated) { this.view = 'login'; return; }
      await this.loadState();
    },

    async loadState() {
      const r = await fetch('api/state');
      if (r.status === 401) { this.view = 'login'; return; }
      const s = await r.json();
      this.snap = s.snapshot;
      this.config = s.config;
      this.form = { ...s.config };
      this.clients = s.clients;
      this.history = s.history || [];
      this.events = s.events || [];
      this.lastEventId = this.events.length ? this.events[this.events.length - 1].id : 0;
      this.version = s.version;
      this.view = 'app';
      this.$nextTick(() => this.connect());
    },

    connect() {
      if (this.es) this.es.close();
      this.es = new EventSource('api/stream?since=' + this.lastEventId);
      this.es.onopen = () => { this.connected = true; };
      this.es.onerror = async () => {
        this.connected = false;
        const me = await fetch('api/me').then(r => r.json()).catch(() => null);
        if (me && !me.authenticated) { this.es.close(); this.view = 'login'; }
      };
      this.es.onmessage = (m) => {
        const d = JSON.parse(m.data);
        this.connected = true;
        this.snap = d.snapshot;
        if (!this.settingsDirty && JSON.stringify(d.config) !== JSON.stringify(this.config)) {
          this.config = d.config; this.form = { ...d.config };
        } else { this.config = d.config; }
        for (const ev of d.events || []) {
          this.events.push(ev);
          this.lastEventId = ev.id;
          if (ev.level === 'error' && this.tab !== 'log') this.unseenErrors++;
        }
        if (this.events.length > 300) this.events.splice(0, this.events.length - 300);
        this.pushSample(d.snapshot);
      };
    },

    // ---------- graphique ----------
    pushSample(s) {
      const last = this.history[this.history.length - 1];
      if (!last || s.now - last.t >= 5) {
        this.history.push({ t: s.now, s: s.speed });
        if (this.history.length > 720) this.history.shift();
        this.updateChart();
      }
    },

    cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); },

    initChart(el) {
      el = el || this.chartEl;
      if (!el) return;
      this.chartEl = el;
      if (typeof uPlot === 'undefined') { setTimeout(() => this.initChart(el), 100); return; }
      if (this.chart) { this.chart.destroy(); this.chart = null; }
      const accent = this.cssVar('--amber');
      const grid = this.cssVar('--grid');
      const ink = this.cssVar('--ink-2');
      const self = this;
      const opts = {
        width: el.clientWidth, height: el.clientHeight || 180,
        padding: [8, 4, 0, 0],
        legend: { show: false },
        cursor: { y: false, points: { size: 7, fill: accent } },
        scales: { x: { time: true }, y: { range: (u, min, max) => [0, Math.max(max * 1.15, 1000)] } },
        axes: [
          { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { show: false }, font: '12px Barlow, sans-serif',
            space: 70,
            values: (u, v) => {
              const span = u.scales.x.max - u.scales.x.min;
              const o = span < 600 ? { hour: '2-digit', minute: '2-digit', second: '2-digit' } : { hour: '2-digit', minute: '2-digit' };
              return v.map(t => new Date(t * 1000).toLocaleTimeString('fr-FR', o));
            } },
          { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { show: false }, size: 70, font: '12px Barlow, sans-serif',
            values: (u, v) => v.map(x => self.fmtSpeed(x)) },
        ],
        series: [{}, {
          stroke: accent, width: 2,
          fill: (u) => { const g = u.ctx.createLinearGradient(0, u.bbox.top, 0, u.bbox.top + u.bbox.height); g.addColorStop(0, accent + '55'); g.addColorStop(1, accent + '00'); return g; },
          points: { show: false },
        }],
        hooks: { setCursor: [u => {
          const i = u.cursor.idx; const tip = el.querySelector('.tip');
          if (i == null || !tip) { if (tip) tip.style.display = 'none'; return; }
          tip.style.display = 'block';
          tip.textContent = self.clock(u.data[0][i]) + '  ' + self.fmtSpeed(u.data[1][i]);
          tip.style.left = Math.min(u.cursor.left + 10, u.bbox.width / devicePixelRatio - 120) + 'px';
        }] },
      };
      this.chart = new uPlot(opts, this.chartData(), el);
      const tip = document.createElement('div'); tip.className = 'tip'; el.querySelector('.u-over').appendChild(tip);
      if (!this.resizeObs) { this.resizeObs = new ResizeObserver(() => { if (this.chart) this.chart.setSize({ width: this.chartEl.clientWidth, height: this.chartEl.clientHeight || 180 }); }); }
      this.resizeObs.disconnect(); this.resizeObs.observe(el);
    },

    chartData() { return [this.history.map(p => p.t), this.history.map(p => p.s)]; },
    updateChart() { if (this.chart) this.chart.setData(this.chartData()); },

    // ---------- actions ----------
    async api(method, url, body) {
      const opts = { method, headers: {} };
      if (body instanceof FormData) opts.body = body;
      else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
      const r = await fetch(url, opts);
      const data = await r.json().catch(() => ({}));
      if (r.status === 401) { this.view = 'login'; throw new Error('Session expirée'); }
      if (!r.ok) throw new Error(data.error || ('Erreur ' + r.status));
      return data;
    },

    async login() {
      this.loginForm.busy = true; this.loginForm.error = '';
      try {
        await this.api('POST', 'api/login', { password: this.loginForm.password, remember: this.loginForm.remember });
        this.loginForm.password = '';
        await this.loadState();
      } catch (e) { this.loginForm.error = e.message; }
      this.loginForm.busy = false;
    },

    async logout() {
      if (this.es) this.es.close();
      await this.api('POST', 'api/logout').catch(() => {});
      this.view = 'login';
    },

    async toggleRun() {
      this.busy.run = true;
      try {
        await this.api('POST', this.snap.running ? 'api/stop' : 'api/start');
        this.toast(this.snap.running ? 'Seed arrêté' : 'Seed démarré', 'ok');
      } catch (e) { this.toast(e.message, 'error'); }
      this.busy.run = false;
    },

    async act(t, action) {
      try {
        await this.api('POST', `api/torrents/${t.hash}/${action}`);
        const msg = { pause: 'Mis en pause', resume: 'Repris', announce: 'Annonce envoyée', reset: 'Réactivé' }[action];
        if (msg) this.toast(msg + ' : ' + t.name, 'ok');
      } catch (e) { this.toast(e.message, 'error'); }
    },

    async remove(t) {
      if (!confirm(`Supprimer « ${t.name} » ?\nLe fichier .torrent est effacé et le tracker reçoit une annonce « stopped ».`)) return;
      try { await this.api('POST', `api/torrents/${t.hash}/remove`); this.toast('Supprimé : ' + t.name, 'ok'); }
      catch (e) { this.toast(e.message, 'error'); }
    },

    dropFiles(ev) { this.dragOver = false; this.upload(ev.dataTransfer.files); },
    pickFiles(ev) { this.upload(ev.target.files); ev.target.value = ''; },

    async upload(files) {
      const list = [...files].filter(f => f.name.toLowerCase().endsWith('.torrent'));
      if (!list.length) { this.toast('Seuls les fichiers .torrent sont acceptés', 'error'); return; }
      const fd = new FormData();
      list.forEach(f => fd.append('files', f));
      try {
        const res = await this.api('POST', 'api/torrents', fd);
        const ok = res.filter(r => !r.error).length;
        if (ok) this.toast(ok === 1 ? 'Torrent ajouté' : `${ok} torrents ajoutés`, 'ok');
        res.filter(r => r.error).forEach(r => this.toast(`${r.file} : ${r.error}`, 'error'));
      } catch (e) { this.toast(e.message, 'error'); }
    },

    async saveSettings() {
      this.busy.save = true; this.settingsError = '';
      try {
        const cfg = await this.api('PUT', 'api/config', this.form);
        this.config = cfg; this.form = { ...cfg }; this.settingsDirty = false;
        this.toast('Réglages enregistrés', 'ok');
      } catch (e) { this.settingsError = e.message; }
      this.busy.save = false;
    },

    resetSettings() { this.form = { ...this.config }; this.settingsDirty = false; this.settingsError = ''; },

    toggleTheme() {
      this.theme = this.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.dataset.theme = this.theme;
      try { localStorage.setItem('rp-theme', this.theme); } catch (_) {}
      this.$nextTick(() => this.initChart());
    },

    toggleOpen(h) { this.open[h] = !this.open[h]; },

    toast(msg, kind = 'info') {
      const id = Date.now() + Math.random();
      this.toasts.push({ id, msg, kind });
      setTimeout(() => { this.toasts = this.toasts.filter(t => t.id !== id); }, kind === 'error' ? 6000 : 3000);
    },

    // ---------- listes ----------
    group(status) {
      if (['starting', 'seeding', 'retrying', 'stopping'].includes(status)) return 'active';
      if (['queued', 'idle'].includes(status)) return 'queued';
      if (status === 'paused') return 'paused';
      if (status === 'done') return 'ended';
      return 'problem';
    },
    countBy(id) { return id === 'all' ? this.snap.torrents.length : this.snap.torrents.filter(t => this.group(t.status) === id).length; },
    visibleTorrents() {
      const q = this.filter.q.trim().toLowerCase();
      return this.snap.torrents.filter(t =>
        (this.filter.status === 'all' || this.group(t.status) === this.filter.status) &&
        (!q || t.name.toLowerCase().includes(q) || t.tracker.toLowerCase().includes(q)));
    },
    filteredEvents() {
      const evs = this.logFilter === 'all' ? this.events : this.events.filter(e => e.level === this.logFilter);
      return evs.slice().reverse();
    },
    clientGroups() {
      const groups = {};
      for (const c of this.clients) {
        const fam = this.clientLabel(c).split(' ')[0];
        (groups[fam] ||= []).push(c);
      }
      return Object.keys(groups).sort().map(name => ({ name, items: groups[name].slice().reverse() }));
    },
    clientLabel(c) {
      if (!c) return '';
      const names = { qbittorrent: 'qBittorrent', utorrent: 'µTorrent', bittorrent: 'BitTorrent', transmission: 'Transmission', deluge: 'Deluge', rtorrent: 'rTorrent', vuze: 'Vuze', leap: 'Vuze Leap' };
      const base = c.replace(/\.client$/, '');
      const i = base.indexOf('-');
      const fam = base.slice(0, i), ver = base.slice(i + 1).replace(/_/g, ' build ');
      return (names[fam] || fam) + ' ' + ver;
    },
    statusLabel(s) {
      return { queued: 'En attente', starting: 'Démarrage', seeding: 'En seed', retrying: 'Nouvel essai', stopping: 'Arrêt…', paused: 'En pause', done: 'Ratio atteint', nopeers: 'Sans pairs', error: 'En erreur', idle: 'À l’arrêt' }[s] || s;
    },

    // ---------- formats ----------
    fmtBytes(b) {
      b = Number(b) || 0;
      const u = ['o', 'ko', 'Mo', 'Go', 'To', 'Po'];
      let i = 0;
      while (b >= 1000 && i < u.length - 1) { b /= 1000; i++; }
      return (i === 0 ? b.toFixed(0) : b.toFixed(b < 10 ? 2 : b < 100 ? 1 : 0)).replace('.', ',') + ' ' + u[i];
    },
    fmtSpeed(b) { return this.fmtBytes(b) + '/s'; },
    speedParts(b) { const s = this.fmtSpeed(b); const i = s.indexOf(' '); return [s.slice(0, i), s.slice(i + 1)]; },
    countdown(ts) {
      const d = Math.max(0, ts - (this.snap.now || Math.floor(Date.now() / 1000)));
      const m = Math.floor(d / 60), s = d % 60;
      return m >= 60 ? `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')}` : `${m}:${String(s).padStart(2, '0')}`;
    },
    duration(sec) {
      sec = Math.max(0, Math.floor(sec));
      const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
      if (d) return `${d} j ${h} h`;
      if (h) return `${h} h ${String(m).padStart(2, '0')}`;
      if (m) return `${m} min`;
      return `${sec} s`;
    },
    ago(ts) { return 'il y a ' + this.duration((this.snap.now || Date.now() / 1000) - ts); },
    clock(ts) { return new Date(ts * 1000).toLocaleTimeString('fr-FR'); },
  }));
});
