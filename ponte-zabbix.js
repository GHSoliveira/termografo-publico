// Ponte Termógrafo ↔ Zabbix — roda numa aba do Zabbix já logada.
//
// Sem token da API: o fetch daqui (mesma origem do Zabbix, credentials: "include") usa a
// sessão de quem está logado. O Termógrafo manda os pedidos (host + PON + período) e a
// ponte devolve a série por minuto de LOS, LOSi, LOFi, Dying Gasp e ONUs online.
//
// Para ativar, no console (F12) da aba do Zabbix:
//   s=document.createElement('script');s.src='http://127.0.0.1:8080/ponte-zabbix.js';document.head.append(s)
// ou salve como favorito:
//   javascript:(s=>{s.src='http://127.0.0.1:8080/ponte-zabbix.js';document.head.append(s)})(document.createElement('script'))
//
// Dois modos, escolhidos sozinhos:
//   api  — se o cookie de sessão for legível, usa a API oficial (api_jsonrpc.php) com a sessão como auth;
//   tela — senão, usa as rotas da própria tela: jsrpc.php (achar host e itens) e
//          history.php?action=showvalues&plaintext=1 (valores em texto).
(() => {
  // uma ponte só: nunca dentro de iframes, nunca duas na mesma página (a marca fica no DOM, que é
  // compartilhado mesmo quando o Tampermonkey roda o script num "mundo" isolado)
  if (window.top !== window) return;
  if (document.getElementById('ponteTermografo')) return;

  const VERSAO = '1.9';
  const BASE = (document.currentScript && document.currentScript.src && new URL(document.currentScript.src).origin)
    || window.TERMOGRAFO_URL || 'http://127.0.0.1:8080';

  // Nomes dos itens por PON (ex.: "PON 2/16 Total de onu Los"). A ordem importa: LOSi e LOFi antes de LOS.
  // Se no seu Zabbix os nomes forem outros, ajuste aqui.
  const ITENS = [
    ['losi', /\blosi\b/i],
    ['lofi', /\blofi\b/i],
    ['los', /\blos\b/i],
    ['semEnergia', /dying\s*gasp|dyinggasp|sem\s*energia|power\s*off/i],
    ['online', /^\s*(total de )?onus? online\s*$/i],          // não pega "online calculado" nem "online nos ultimos 5 dias"
    ['totalClientes', /autorizad|provisionad|^\s*total de onus?\s*$/i],
  ];

  // no máximo 3 pedidos ao Zabbix ao mesmo tempo: vários chamados juntos não viram rajada
  const MAX_SIMULTANEOS = 3;
  let emVoo = 0;
  const espera = [];
  async function limitado(fn) {
    while (emVoo >= MAX_SIMULTANEOS) await new Promise(r => espera.push(r));
    emVoo++;
    try { return await fn(); } finally { emVoo--; espera.shift()?.(); }
  }

  let seq = 0, modo = 'tela', sessao = null, ativo = true, consultas = 0, ultimoErro = '', estado = 'iniciando';
  const cacheHost = new Map(), cacheItens = new Map();
  const esperar = ms => new Promise(r => setTimeout(r, ms));
  const dd = n => String(n).padStart(2, '0');
  const dataZbx = ms => { const d = new Date(ms); return `${d.getFullYear()}-${dd(d.getMonth() + 1)}-${dd(d.getDate())} ${dd(d.getHours())}:${dd(d.getMinutes())}:${dd(d.getSeconds())}`; };
  const escRe = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  // ---------- sessão e chamadas ao Zabbix ----------
  function lerSessao() {
    const m = document.cookie.match(/(?:^|;\s*)zbx_session=([^;]+)/);
    if (!m) return null;
    try { return JSON.parse(atob(decodeURIComponent(m[1]))).sessionid || null; } catch { return null; }
  }
  async function api(method, params) {
    const r = await limitado(() => fetch('api_jsonrpc.php', {
      method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json-rpc' },
      body: JSON.stringify({ jsonrpc: '2.0', method, params, auth: sessao, id: ++seq })
    }));
    const j = await r.json();
    if (j.error) throw new Error(j.error.data || j.error.message);
    return j.result;
  }
  async function jsrpc(method, params) {
    const r = await limitado(() => fetch('jsrpc.php?output=json-rpc', {
      method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ jsonrpc: '2.0', method, params, id: ++seq })
    }));
    const j = await r.json().catch(() => { throw new Error('sessão expirada: faça login de novo'); });
    if (j.error) throw new Error(j.error.message || j.error.data || 'erro no jsrpc');
    return j.result;
  }
  async function detectarModo() {
    sessao = lerSessao();
    if (sessao) {
      try { await api('host.get', { output: ['hostid'], limit: 1 }); modo = 'api'; return; } catch { /* cai para a tela */ }
    }
    modo = 'tela';
    const st = await jsrpc('zabbix.status', { nocache: true });
    if (!st || st.result === false) throw new Error('servidor do monitoramento parado');
  }

  // ---------- host, itens e valores ----------
  // cache de promessas: vários pedidos do mesmo host/PON ao mesmo tempo viram uma busca só
  const umaVez = (cache, chave, fn) => {
    if (!cache.has(chave)) cache.set(chave, fn().catch(err => { cache.delete(chave); throw err; }));
    return cache.get(chave);
  };
  const hostId = nome => umaVez(cacheHost, nome, () => buscarHost(nome));
  const itensDaPon = (hid, pon) => umaVez(cacheItens, hid + '|' + pon, () => buscarItens(hid, pon));
  // telas do Zabbix 6.0 que respondem com {"body": html}: menus com hostid/itemid em data-menu-popup
  async function tela(url) {
    const r = await limitado(() => fetch(url, { credentials: 'include', cache: 'no-store' }));
    const t = await r.text();
    if (/name="enter"|id="login"/i.test(t.slice(0, 5000))) throw new Error('sessão expirada: faça login de novo');
    let html = t; try { html = JSON.parse(t).body || t; } catch { /* página inteira */ }
    return new DOMParser().parseFromString(html, 'text/html');
  }
  const menu = (el, campo) => { try { const m = JSON.parse(el.getAttribute('data-menu-popup')); return m && m.data ? m.data[campo] : null; } catch { return null; } };
  const numero = txt => { // "21", "63 %", "Enable (1)", "5.1 dBm" -> número
    const m = String(txt).match(/\((-?\d+(?:[.,]\d+)?)\)\s*$/) || String(txt).match(/-?\d+(?:[.,]\d+)?/);
    return m ? parseFloat((m[1] ?? m[0]).replace(',', '.')) : NaN;
  };

  async function buscarHost(nome) {
    let lista;
    if (modo === 'api') {
      lista = (await api('host.get', { output: ['hostid', 'host', 'name'], search: { host: nome }, limit: 10 }))
        .map(h => ({ id: h.hostid, name: h.host }));
      const h = lista.find(h => String(h.name).toUpperCase() === nome.toUpperCase());
      if (!h) throw new Error(`host ${nome} não encontrado`);
      return h.id;
    }
    // modo tela: 1º a lista de Hosts filtrada por name= (resposta pequena, confirmado no Zabbix do provedor);
    // de reserva, a página de Problemas (cada linha traz o host com o hostid). Só leitura, sem salvar filtro.
    for (const url of [
      `zabbix.php?action=host.view.refresh&name=${encodeURIComponent(nome)}`,
      'zabbix.php?action=problem.view.refresh',
    ]) {
      const d = await tela(url);
      for (const a of d.querySelectorAll('[data-menu-popup]')) {
        const id = menu(a, 'hostid');
        if (id && a.textContent.trim().toUpperCase() === nome.toUpperCase()) return id;
      }
    }
    throw new Error(`host ${nome} não encontrado (nem nos Problemas nem na lista de Hosts)`);
  }
  async function buscarItens(hid, pon) {
    const daPon = new RegExp(`\\bG?PON\\s+${escRe(pon)}(?![\\d/])`, 'i'); // "PON 1/1" (Nokia) ou "GPON 0/17/15" (Huawei), e não "PON 2/160"
    let lista = [];
    if (modo === 'api') {
      lista = (await api('item.get', { output: ['itemid', 'name', 'value_type'], hostids: [hid], search: { name: `PON ${pon}` } }))
        .map(i => ({ id: i.itemid, name: i.name, vt: +i.value_type }));
    } else { // Dados recentes do host, filtrados pelo nome do item (até 4 páginas)
      const vistos = new Set();
      for (let pagina = 1; pagina <= 4; pagina++) {
        const d = await tela(`zabbix.php?action=latest.view.refresh&hostids%5B%5D=${encodeURIComponent(hid)}&name=${encodeURIComponent('PON ' + pon)}&page=${pagina}`);
        let novos = 0;
        for (const a of d.querySelectorAll('[data-menu-popup]')) {
          const id = menu(a, 'itemid');
          if (!id || vistos.has(id)) continue;
          vistos.add(id); novos++;
          lista.push({ id, name: a.textContent.trim() });
        }
        if (!novos || !lista.some(i => daPon.test(i.name))) break;
      }
    }
    const itens = {};
    for (const it of lista.filter(i => daPon.test(i.name))) {
      const resto = it.name.replace(daPon, '');
      const tipo = ITENS.find(([, re]) => re.test(resto))?.[0];
      if (tipo && !itens[tipo]) itens[tipo] = it;
    }
    if (!Object.keys(itens).length) throw new Error(`nenhum item "PON ${pon}" reconhecido no host`);
    return itens;
  }
  // [{clock (s), value}] de um item no período
  async function valores(it, de, ate) {
    if (modo === 'api') {
      const params = { output: ['clock', 'value'], itemids: [it.id], time_from: Math.floor(de / 1000), time_till: Math.ceil(ate / 1000), sortfield: 'clock', sortorder: 'ASC' };
      let h = await api('history.get', { ...params, history: it.vt ?? 3 });
      if (!h.length && it.vt === undefined) h = await api('history.get', { ...params, history: 0 });
      return h.map(v => ({ clock: +v.clock, value: +v.value }));
    }
    // tabela de valores do histórico (o "texto puro" não existe nessa versão); período relativo a agora
    const minutos = Math.max(5, Math.ceil((Date.now() - de) / 60000));
    const d = await tela(`history.php?action=showvalues&itemids%5B%5D=${encodeURIComponent(it.id)}&from=now-${minutos}m&to=now`);
    const out = [];
    for (const tr of d.querySelectorAll('table.list-table tbody tr')) {
      if (tr.cells.length < 2) continue;
      const quando = Date.parse(tr.cells[0].textContent.trim().replace(' ', 'T'));
      const valor = numero(tr.cells[tr.cells.length - 1].textContent);
      if (!Number.isNaN(quando) && !Number.isNaN(valor)) out.push({ clock: Math.floor(quando / 1000), value: valor });
    }
    return out.sort((a, b) => a.clock - b.clock);
  }

  // série por minuto no formato do Termógrafo
  async function consultar(p) {
    if (!p.pon) throw new Error('chamado sem PON');
    const hid = await hostId(p.olt);
    const itens = await itensDaPon(hid, p.pon);
    const porMinuto = new Map();
    let total = null;
    await Promise.all(Object.entries(itens).map(async ([tipo, it]) => {
      for (const v of await valores(it, p.de, p.ate)) {
        if (tipo === 'totalClientes') { total = v.value; continue; }
        const t = Math.floor(v.clock / 60) * 60000;
        if (!porMinuto.has(t)) porMinuto.set(t, {});
        porMinuto.get(t)[tipo] = v.value;
      }
    }));
    const anterior = {};
    const serie = [...porMinuto.keys()].sort((a, b) => a - b).map(t => {
      const m = porMinuto.get(t), p2 = { t };
      for (const k of ['los', 'losi', 'lofi', 'semEnergia', 'online']) {
        if (m[k] !== undefined) anterior[k] = m[k]; // itens coletados em intervalos maiores: repete o último valor
        p2[k] = anterior[k] ?? 0;
      }
      if (!itens.online && total != null) p2.online = Math.max(0, total - p2.los - p2.losi - p2.lofi - p2.semEnergia);
      return p2;
    });
    const ult = serie[serie.length - 1];
    const totalClientes = total ?? (ult ? ult.online + ult.los + ult.losi + ult.lofi + ult.semEnergia : undefined);
    return { totalClientes, serie, itens: Object.fromEntries(Object.entries(itens).map(([k, it]) => [k, it.name])) };
  }

  // ---------- alarmes abertos (pedido "problemas": a cada 5 min ou quando pedido na página) ----------
  const SEVERIDADES = [[/n[aã]o class|not class/i, 0], [/informa/i, 1], [/warning|aten[cç][aã]o|aviso/i, 2], [/average|m[eé]dia/i, 3], [/high|alta/i, 4], [/disaster|desastre/i, 5]];
  const hashTexto = s => { let h = 2166136261; for (const c of s) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return (h >>> 0).toString(36); };
  function lerCsv(txt) { // CSV simples com aspas
    const linhas = []; let campo = '', linha = [], aspas = false;
    for (let i = 0; i < txt.length; i++) {
      const c = txt[i];
      if (aspas) { if (c === '"') { if (txt[i + 1] === '"') { campo += '"'; i++; } else aspas = false; } else campo += c; continue; }
      if (c === '"') aspas = true;
      else if (c === ',') { linha.push(campo); campo = ''; }
      else if (c === '\n' || c === '\r') { if (c === '\r' && txt[i + 1] === '\n') i++; linha.push(campo); campo = ''; if (linha.some(v => v !== '')) linhas.push(linha); linha = []; }
      else campo += c;
    }
    linha.push(campo); if (linha.some(v => v !== '')) linhas.push(linha);
    return linhas;
  }
  async function problemasAbertos(sevMin = 0) {
    if (modo === 'api') {
      const ts = await api('trigger.get', {
        output: ['triggerid', 'description', 'priority', 'lastchange', 'value'], filter: { value: 1 }, monitored: true, skipDependent: true,
        min_severity: sevMin, expandDescription: true, selectHosts: ['host'], selectLastEvent: ['eventid', 'clock', 'name', 'acknowledged'], selectTags: 'extend'
      });
      return ts.map(t => ({
        server: 'Zabbix GPON', id: t.triggerid, eventId: t.lastEvent?.eventid, description: t.lastEvent?.name || t.description,
        priority: +t.priority, lastchange: +t.lastchange, value: 1, host: t.hosts?.[0]?.host, acknowledged: +(t.lastEvent?.acknowledged || 0),
        trigger_tags: (t.tags || []).map(x => ({ name: x.tag, value: x.value }))
      }));
    }
    // tela: o mesmo CSV do botão "Exportar para CSV" da página de Problemas, só os abertos (show=3)
    const r = await limitado(() => fetch('zabbix.php?action=problem.view.csv&sort=clock&sortorder=DESC', { credentials: 'include', cache: 'no-store' }));
    const txt = await r.text();
    if (/name="enter"|id="login"|<html/i.test(txt.slice(0, 2000))) throw new Error('sessão expirada ou exportação de problemas indisponível');
    const [cab = [], ...linhas] = lerCsv(txt);
    const col = re => cab.findIndex(h => re.test(h.trim()));
    const iSev = col(/sever/i), iHora = col(/^(time|hora|tempo|data)$/i), iStatus = col(/^status$/i), iHost = col(/^host$/i), iProb = col(/^(problem|problema)$/i), iTags = col(/^tags?$/i), iAck = col(/^(ack|acknowledged|reconhecido|reconh\.?)$/i);
    if (iHost < 0 || iProb < 0) throw new Error('CSV de problemas em formato desconhecido');
    const lista = [];
    for (const l of linhas) {
      if (iStatus >= 0 && /resol/i.test(l[iStatus] || '')) continue;
      const sev = SEVERIDADES.find(([re]) => re.test(l[iSev] || ''))?.[1] ?? 0;
      if (sev < sevMin) continue;
      const quando = Date.parse(String(l[iHora] || '').trim().replace(' ', 'T'));
      const tags = String(l[iTags] || '').split(/,\s*/).filter(Boolean).map(t => { const i = t.indexOf(':'); return i < 0 ? { name: t.trim(), value: '' } : { name: t.slice(0, i).trim(), value: t.slice(i + 1).trim() }; });
      lista.push({
        server: 'Zabbix GPON', id: 'csv' + hashTexto(l[iHost] + '|' + l[iProb]), description: l[iProb], priority: sev,
        lastchange: Math.floor((Number.isNaN(quando) ? Date.now() : quando) / 1000), value: 1, host: l[iHost], problem_tags: tags,
        acknowledged: iAck >= 0 && /^(yes|sim|1)$/i.test(String(l[iAck] || '').trim()) ? 1 : 0
      });
    }
    return lista;
  }

  // ---------- laço com o Termógrafo ----------
  async function atender(p) {
    if (p.criado && Date.now() - p.criado > 55000) return; // o servidor já desistiu: não gasta o Zabbix à toa
    let corpo;
    try { corpo = { id: p.id, ...(p.tipo === 'problemas' ? { problemas: await problemasAbertos(p.sevMin), completo: true } : await consultar(p)) }; consultas++; ultimoErro = ''; }
    catch (err) { corpo = { id: p.id, erro: `monitoramento: ${err.message}` }; ultimoErro = p.tipo === 'problemas' ? `alarmes abertos: ${err.message}` : `${p.olt} PON ${p.pon}: ${err.message}`; }
    await fetch(`${BASE}/api/ponte/resposta`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(corpo) }).catch(() => {});
    pintar();
  }
  async function laco() {
    while (ativo) {
      try {
        // a primeira volta na hora (confirma a conexão); as seguintes esperam até 20 s por trabalho
        const r = await fetch(`${BASE}/api/ponte/pedidos?espera=${estado === 'conectada' ? 20 : 0}&modo=${modo}&recursos=problemas`, { cache: 'no-store' });
        if (!r.ok) throw new Error(`o Termógrafo respondeu ${r.status}`);
        estado = 'conectada'; pintar();
        const pedidos = await r.json();
        pedidos.forEach(atender); // em paralelo; o próximo pedido de trabalho não espera
      } catch (err) {
        estado = 'sem contato com o Termógrafo'; ultimoErro = err.message; pintar();
        await esperar(5000);
      }
    }
  }

  // ---------- painel discreto na aba do Zabbix ----------
  const painel = document.createElement('div');
  painel.id = 'ponteTermografo';
  painel.style.cssText = 'position:fixed;right:16px;bottom:16px;z-index:2147483647;max-width:340px;padding:10px 12px;border-radius:12px;background:#1b1b1a;color:#f2f1ec;font:12.5px/1.45 system-ui,sans-serif;box-shadow:0 6px 24px rgba(0,0,0,.35)';
  function pintar() {
    painel.innerHTML = `<div style="display:flex;align-items:center;gap:8px"><b style="font-weight:600">Ponte com o Termógrafo</b>
      <span style="margin-left:auto;opacity:.7">${modo === 'api' ? 'API' : 'tela'} · v${VERSAO}</span>
      <button type="button" style="border:1px solid #555;background:none;color:inherit;border-radius:8px;padding:1px 8px;cursor:pointer">${ativo ? 'Parar' : 'Fechar'}</button></div>
      <div style="opacity:.8">${ativo ? estado : 'parada'} · ${consultas} consulta${consultas === 1 ? '' : 's'} · ${BASE.replace(/^https?:\/\//, '')}</div>
      ${ultimoErro ? `<div style="margin-top:4px;opacity:.75"></div>` : ''}`;
    if (ultimoErro) painel.lastElementChild.textContent = 'Último erro: ' + ultimoErro;
    painel.querySelector('button').onclick = () => {
      if (ativo) { ativo = false; pintar(); } else { painel.remove(); }
    };
  }
  document.body.appendChild(painel);
  pintar();

  const iniciar = () => detectarModo()
    .then(() => { estado = 'conectando…'; pintar(); return laco(); })
    .catch(err => { estado = 'não iniciou'; ultimoErro = err.message; ativo = false; pintar(); });
  // uma ponte por navegador: com várias abas do Zabbix abertas, só uma trabalha; as outras esperam
  // e assumem sozinhas se a primeira for fechada
  if (navigator.locks) {
    estado = 'em espera: outra aba do Zabbix já está com a ponte'; pintar();
    navigator.locks.request('ponte-termografo', () => (ativo ? iniciar() : null)).catch(() => {});
  } else {
    iniciar();
  }
})();
