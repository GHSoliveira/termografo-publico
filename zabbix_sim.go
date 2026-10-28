// Zabbix simulado (no simulador, porta 8081): só o necessário para testar a ponte, com os
// nomes do Zabbix real (host EMP.CMP.POP2.OLT-FH-001, itens "PON 2/16 Total de onu Los").
//
//	GET  /                      página "Zabbix (simulado)": faz o login (cookie zbx_session) e ativa a ponte
//	POST /jsrpc.php             zabbix.status e multiselect.get (hosts, items) — usa o cookie
//	GET  /history.php           action=showvalues&plaintext=1 — valores em texto, usa o cookie
//	POST /api_jsonrpc.php       apiinfo.version, host.get, item.get, history.get — usa "auth"
//
// O cookie é HttpOnly (como no Zabbix real), então a ponte usa o modo "tela". Com /?modo=api o
// cookie fica legível e a ponte usa a API com a sessão como auth.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sessaoSim = "5f1c0de1b2a34c5d6e7f8091a2b3c4d5"

var metricasSim = []struct{ chave, nome string }{
	{"total", "Total de onu autorizadas"}, {"los", "Total de onu Los"}, {"offline", "Total de onu off-line"},
	{"online", "Total de onu online"}, {"calculado", "Total de onu online calculado"}, {"semEnergia", "Total de onu sem energia"},
	{"portaOnline", "Port Online Status"},
}

type itemSim struct {
	id, nome, metrica string
	rs                *redeSim
}

func idSim(partes ...string) string {
	h := fnv.New32a()
	h.Write([]byte(strings.Join(partes, "|")))
	return strconv.Itoa(int(h.Sum32()%9_000_000) + 1_000_000)
}

// hosts e itens das quedas GPON em andamento no simulador
func (mm *monitorMock) catalogoZabbix() (map[string]string, []itemSim) {
	hosts, itens := map[string]string{}, []itemSim{}
	for _, r := range mm.ativos() {
		if r.categoria != "gpon" {
			continue
		}
		hosts[idSim(r.olt)] = r.olt
		for _, m := range metricasSim {
			itens = append(itens, itemSim{id: idSim(r.olt, r.pon, m.chave), nome: fmt.Sprintf("PON %s %s", r.pon, m.nome), metrica: m.chave, rs: r})
		}
	}
	return hosts, itens
}

func valorSim(it itemSim, t time.Time) int {
	p := it.rs.ponto(t)
	switch it.metrica {
	case "los":
		return p.LOS
	case "losi":
		return p.LOSi
	case "lofi":
		return p.LOFi
	case "semEnergia":
		return p.SemEnergia
	case "online":
		return p.Online
	case "offline":
		return it.rs.total - p.Online
	case "calculado":
		return p.Online * 100 / max(it.rs.total, 1)
	case "portaOnline":
		return 1
	}
	return it.rs.total
}

func sessaoValida(r *http.Request) bool {
	c, err := r.Cookie("zbx_session")
	if err != nil {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(c.Value)
	var s struct {
		SessionID string `json:"sessionid"`
	}
	return err == nil && json.Unmarshal(b, &s) == nil && s.SessionID == sessaoSim
}

// "now", "now-2h", "2026-10-23 08:40:41" ou epoch
func tempoZbx(v string, padrao time.Time) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return padrao
	}
	if strings.HasPrefix(v, "now") {
		if d, err := time.ParseDuration(strings.TrimPrefix(strings.TrimPrefix(v, "now"), "-")); err == nil {
			return time.Now().Add(-d)
		}
		return time.Now()
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", v, time.Local); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	return padrao
}

func (mm *monitorMock) registrarZabbix(mux *http.ServeMux, termografo string) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		valor := base64.StdEncoding.EncodeToString([]byte(`{"sessionid":"` + sessaoSim + `","sign":"sim"}`))
		http.SetCookie(w, &http.Cookie{Name: "zbx_session", Value: valor, Path: "/", HttpOnly: r.URL.Query().Get("modo") != "api", SameSite: http.SameSiteLaxMode})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><title>Zabbix (simulado)</title>
<style>body{margin:0;font:14px system-ui,sans-serif;background:#0e1012;color:#f2f2f2}header{padding:12px 20px;background:#1f2c33;font-weight:600}main{padding:24px 20px;max-width:760px}code{background:#20262b;padding:1px 6px;border-radius:4px}button{font:inherit;padding:8px 14px;border-radius:8px;border:1px solid #555;background:#20262b;color:inherit;cursor:pointer}</style></head>
<body><header>ZABBIX · zabbix (simulado)</header><main>
<p>Logado como <b>noc.plantao</b> (cookie de sessão%s).</p>
<p>Esta aba faz o papel do Zabbix real. A ponte roda aqui e usa a sessão desta aba, sem token.</p>
<p><button id="b">Ativar ponte com o Termógrafo</button></p>
<p style="opacity:.7">No Zabbix real, o mesmo pelo console: <code>s=document.createElement('script');s.src='%s/ponte-zabbix.js';document.head.append(s)</code></p>
<script>document.getElementById('b').onclick=()=>{const s=document.createElement('script');s.src=%q;document.head.append(s)};
if(location.hash==='#ponte')document.getElementById('b').click();</script>
</main></body></html>`, map[bool]string{true: " legível: a ponte usa a API", false: " HttpOnly, como no real: a ponte usa a tela"}[r.URL.Query().Get("modo") == "api"], termografo, termografo+"/ponte-zabbix.js")
	})

	mux.HandleFunc("POST /jsrpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			ID     any            `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json-rpc")
		if !sessaoValida(r) {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "error": map[string]any{"message": "Session terminated, re-login, please."}, "id": req.ID})
			return
		}
		var res any = []any{}
		switch req.Method {
		case "zabbix.status":
			res = map[string]any{"result": true, "message": ""}
		case "multiselect.get":
			busca := strings.ToLower(texto(req.Params["search"]))
			hosts, itens := mm.catalogoZabbix()
			lista := []map[string]any{}
			switch texto(req.Params["object_name"]) {
			case "hosts":
				for id, nome := range hosts {
					if strings.Contains(strings.ToLower(nome), busca) {
						lista = append(lista, map[string]any{"id": id, "name": nome})
					}
				}
			case "items":
				hid := texto(req.Params["hostid"])
				for _, it := range itens {
					if (hid == "" || idSim(it.rs.olt) == hid) && strings.Contains(strings.ToLower(it.nome), busca) {
						lista = append(lista, map[string]any{"id": it.id, "name": it.nome, "prefix": it.rs.olt + ": "})
					}
				}
			}
			res = lista
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": res, "id": req.ID})
	})

	// valores em texto: uma linha "data clock valor" por coleta (1 por minuto aqui)
	// problemas abertos como o CSV do botão "Exportar para CSV" da página de Problemas
	mux.HandleFunc("GET /zabbix.php", func(w http.ResponseWriter, r *http.Request) {
		acao := r.URL.Query().Get("action")
		if acao == "host.view.refresh" || acao == "latest.view.refresh" || acao == "problem.view.refresh" {
			if !sessaoValida(r) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, `<html><body><form id="login"><button name="enter">Sign in</button></form></body></html>`)
				return
			}
			q := r.URL.Query()
			hosts, itens := mm.catalogoZabbix()
			var b strings.Builder
			b.WriteString(`<table class="list-table"><tbody>`)
			menu := func(campo, id, texto string) string {
				return fmt.Sprintf(`<a class="link-action" data-menu-popup="{&quot;type&quot;:&quot;%s&quot;,&quot;data&quot;:{&quot;%sid&quot;:&quot;%s&quot;}}">%s</a>`, campo, campo, id, texto)
			}
			if acao == "problem.view.refresh" { // linhas de problema com o host (hostid no menu), como a página de Problemas
				for _, p := range mm.problemasSim() {
					nome := p["host"].(string)
					b.WriteString("<tr><td>" + menu("host", idSim(nome), nome) + "</td><td>" + p["description"].(string) + "</td></tr>")
				}
			} else if acao == "host.view.refresh" {
				busca := strings.ToLower(q.Get("filter_name") + q.Get("name"))
				for id, nome := range hosts {
					if strings.Contains(strings.ToLower(nome), busca) {
						b.WriteString("<tr><td>" + menu("host", id, nome) + "</td></tr>")
					}
				}
			} else {
				hid, busca := q.Get("hostids[]"), strings.ToLower(q.Get("name"))
				pagina, _ := strconv.Atoi(q.Get("page"))
				n := 0
				for _, it := range itens {
					if (hid == "" || idSim(it.rs.olt) == hid) && strings.Contains(strings.ToLower(it.nome), busca) {
						n++
						if pagina <= 1 && n <= 50 { // 50 por página, como a tela
							b.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%d</td></tr>", menu("item", it.id, it.nome), valorSim(it, time.Now())))
						}
					}
				}
			}
			b.WriteString("</tbody></table>")
			corpo, _ := json.Marshal(map[string]string{"body": b.String()})
			w.Header().Set("Content-Type", "application/json")
			w.Write(corpo)
			return
		}
		if acao != "problem.view.csv" {
			http.NotFound(w, r)
			return
		}
		if !sessaoValida(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><body><form id="login"><button name="enter">Sign in</button></form></body></html>`)
			return
		}
		nomesSev := []string{"Not classified", "Information", "Warning", "Average", "High", "Disaster"}
		csv := func(campos ...string) string {
			for i, c := range campos {
				campos[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
			}
			return strings.Join(campos, ",") + "\n"
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		var b strings.Builder
		b.WriteString(csv("Severity", "Time", "Recovery time", "Status", "Host", "Problem", "Duration", "Ack", "Actions", "Tags"))
		for _, p := range mm.problemasSim() {
			tags := []string{}
			for _, t := range p["problem_tags"].([]map[string]any) {
				tags = append(tags, fmt.Sprintf("%v: %v", t["name"], t["value"]))
			}
			ini := time.Unix(p["lastchange"].(int64), 0)
			b.WriteString(csv(nomesSev[p["priority"].(int)], ini.Format("2006-01-02 15:04:05"), "", "PROBLEM", p["host"].(string), p["description"].(string),
				time.Since(ini).Round(time.Second).String(), map[bool]string{true: "Yes", false: "No"}[p["acknowledged"] == 1], "", strings.Join(tags, ", ")))
		}
		fmt.Fprint(w, b.String())
	})

	mux.HandleFunc("GET /history.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !sessaoValida(r) {
			fmt.Fprint(w, `<html><body><form id="login"><input name="name"><input name="password"><button name="enter">Sign in</button></form></body></html>`)
			return
		}
		q := r.URL.Query()
		ids := map[string]bool{}
		for _, id := range q["itemids[]"] {
			ids[id] = true
		}
		ate := tempoZbx(q.Get("to"), time.Now())
		if ate.After(time.Now()) {
			ate = time.Now()
		}
		de := tempoZbx(q.Get("from"), ate.Add(-time.Hour))
		_, itens := mm.catalogoZabbix()
		var b strings.Builder
		b.WriteString(`<html><body><table class="list-table"><thead><tr><th>Timestamp</th><th>Value</th></tr></thead><tbody>`)
		linhas := 0
		for _, it := range itens {
			if !ids[it.id] {
				continue
			}
			for t := ate.Truncate(time.Minute); !t.Before(de) && linhas < 50; t = t.Add(-time.Minute) {
				fmt.Fprintf(&b, "<tr><td>%s</td><td>%d</td></tr>", t.Format("2006-01-02 15:04:05"), valorSim(it, t))
				linhas++
			}
		}
		if linhas == 0 {
			b.WriteString(`<tr><td colspan="2" class="nothing-to-show">No data found.</td></tr>`)
		}
		b.WriteString("</tbody></table></body></html>")
		fmt.Fprint(w, b.String())
	})

	mux.HandleFunc("POST /api_jsonrpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			Auth   string         `json:"auth"`
			ID     any            `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json-rpc")
		erro := func(msg string) {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": -32602, "message": "Invalid params.", "data": msg}, "id": req.ID})
		}
		if req.Method == "apiinfo.version" {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": "6.0.46", "id": req.ID})
			return
		}
		if req.Auth != sessaoSim {
			erro("Not authorised.")
			return
		}
		hosts, itens := mm.catalogoZabbix()
		busca := func(campo string) string {
			s, _ := req.Params["search"].(map[string]any)
			return strings.ToLower(texto(s[campo]))
		}
		var res any = []any{}
		switch req.Method {
		case "host.get":
			lista := []map[string]any{}
			for id, nome := range hosts {
				if strings.Contains(strings.ToLower(nome), busca("host")) {
					lista = append(lista, map[string]any{"hostid": id, "host": nome, "name": nome})
				}
			}
			res = lista
		case "item.get":
			hids := map[string]bool{}
			if l, ok := req.Params["hostids"].([]any); ok {
				for _, h := range l {
					hids[texto(h)] = true
				}
			}
			lista := []map[string]any{}
			for _, it := range itens {
				if (len(hids) == 0 || hids[idSim(it.rs.olt)]) && strings.Contains(strings.ToLower(it.nome), busca("name")) {
					lista = append(lista, map[string]any{"itemid": it.id, "name": it.nome, "value_type": "3"})
				}
			}
			res = lista
		case "trigger.get": // problemas abertos (filter value=1)
			lista := []map[string]any{}
			minSev, _ := inteiro(req.Params["min_severity"])
			for _, p := range mm.problemasSim() {
				if p["priority"].(int) < minSev {
					continue
				}
				tags := []map[string]any{}
				for _, t := range p["problem_tags"].([]map[string]any) {
					tags = append(tags, map[string]any{"tag": t["name"], "value": t["value"]})
				}
				lc := strconv.FormatInt(p["lastchange"].(int64), 10)
				lista = append(lista, map[string]any{"triggerid": p["id"], "description": p["description"], "priority": strconv.Itoa(p["priority"].(int)),
					"lastchange": lc, "value": "1", "hosts": []map[string]any{{"host": p["host"]}},
					"lastEvent": map[string]any{"eventid": p["eventId"], "clock": lc, "name": p["description"], "acknowledged": strconv.Itoa(p["acknowledged"].(int))}, "tags": tags})
			}
			res = lista
		case "history.get":
			ids := map[string]bool{}
			if l, ok := req.Params["itemids"].([]any); ok {
				for _, i := range l {
					ids[texto(i)] = true
				}
			}
			de, _ := inteiro(req.Params["time_from"])
			ate, _ := inteiro(req.Params["time_till"])
			if ate == 0 || int64(ate) > time.Now().Unix() {
				ate = int(time.Now().Unix())
			}
			lista := []map[string]any{}
			for _, it := range itens {
				if !ids[it.id] {
					continue
				}
				for t := time.Unix(int64(de), 0).Truncate(time.Minute); t.Unix() <= int64(ate); t = t.Add(time.Minute) {
					if t.Unix() >= int64(de) {
						lista = append(lista, map[string]any{"itemid": it.id, "clock": strconv.FormatInt(t.Unix(), 10), "value": strconv.Itoa(valorSim(it, t)), "ns": "0"})
					}
				}
			}
			sort.SliceStable(lista, func(i, j int) bool { return lista[i]["clock"].(string) < lista[j]["clock"].(string) })
			res = lista
		default:
			erro("método não simulado: " + req.Method)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": res, "id": req.ID})
	})
}

// problemasSim: quedas GPON em andamento, no formato da trigger que o simulador manda por webhook
func (mm *monitorMock) problemasSim() []map[string]any {
	agora := time.Now()
	lista := []map[string]any{}
	for _, r := range mm.ativos() {
		if r.categoria != "gpon" || agora.Before(r.inicio) || !agora.Before(r.fimQueda) {
			continue
		}
		t := r.trigger(1, nil)
		t["lastchange"] = r.inicio.Unix()
		t["acknowledged"] = 0
		if agora.After(r.inicio.Add(3 * time.Minute)) {
			t["acknowledged"] = 1
		}
		lista = append(lista, t)
	}
	return lista
}
