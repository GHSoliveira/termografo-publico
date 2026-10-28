// Ponte com o Zabbix pela sessão do navegador.
//
// Sem token da API, quem fala com o Zabbix é um script (ponte-zabbix.js) rodando numa aba do
// Zabbix já logada: lá o fetch(credentials: "include") leva o cookie de sessão. O Termógrafo
// enfileira os pedidos de consulta (host + PON + período); a ponte busca os pedidos aqui
// (long polling), consulta o Zabbix e devolve a série. Para o resto do servidor, a ponte é
// uma fonte de consulta igual à -monitoramento.
//
//	GET  /api/ponte/pedidos?espera=20   → [{"id", "olt", "pon", "de", "ate"}]
//	POST /api/ponte/resposta            ← {"id", "erro"?, "totalClientes", "serie", "itens"?}
//	GET  /ponte-zabbix.js               → o script da ponte
//
// Como a aba do Zabbix é outra origem, as rotas da ponte respondem com CORS só para as
// origens de -ponte-zabbix.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	esperaRespostaPonte = 60 * time.Second // quanto um pedido espera a ponte responder (a ponte descarta os mais velhos)
	ponteSilenciosa     = 45 * time.Second // sem contato há mais que isso = desconectada
)

type pedidoPonte struct {
	ID     int    `json:"id"`
	Tipo   string `json:"tipo,omitempty"` // "" = série de uma PON | "problemas" = alarmes abertos
	OLT    string `json:"olt,omitempty"`
	PON    string `json:"pon,omitempty"`
	De     int64  `json:"de,omitempty"`
	Ate    int64  `json:"ate,omitempty"`
	SevMin int    `json:"sevMin,omitempty"` // problemas: severidade mínima
	Criado int64  `json:"criado"`           // ms; a ponte descarta pedido que o servidor já desistiu de esperar

	resposta chan respostaPonte
}

type respostaPonte struct {
	dados map[string]any
	err   error
}

type Ponte struct {
	mu         sync.Mutex
	seq        int
	fila       []*pedidoPonte       // ainda não entregues à ponte
	aguardando map[int]*pedidoPonte // entregues, esperando resposta
	novo       chan struct{}        // fechado e trocado a cada pedido novo (acorda o long polling)
	contato    time.Time            // último pedido de trabalho da ponte
	modo       string               // "api" | "tela", informado pela ponte
	origens    map[string]bool
	script     string
}

func novaPonte(origens, script string) *Ponte {
	p := &Ponte{aguardando: map[int]*pedidoPonte{}, novo: make(chan struct{}), origens: map[string]bool{}, script: script}
	for _, o := range strings.Split(origens, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			p.origens[o] = true
		}
	}
	return p
}

func (p *Ponte) conectada() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.contato) < ponteSilenciosa
}

// Consultar enfileira o pedido e espera a ponte responder (mesma assinatura da fonte HTTP).
func (p *Ponte) Consultar(olt, pon string, de, ate int64) (map[string]any, time.Duration, error) {
	return p.pedir(&pedidoPonte{OLT: olt, PON: pon, De: de, Ate: ate})
}

func (p *Ponte) pedir(ped *pedidoPonte) (map[string]any, time.Duration, error) {
	ini := time.Now()
	if !p.conectada() {
		return nil, 0, errors.New("ponte com o monitoramento desconectada: abra o monitoramento no navegador e ative a ponte")
	}
	p.mu.Lock()
	p.seq++
	ped.ID, ped.resposta, ped.Criado = p.seq, make(chan respostaPonte, 1), time.Now().UnixMilli()
	p.fila = append(p.fila, ped)
	p.aguardando[ped.ID] = ped
	close(p.novo)
	p.novo = make(chan struct{})
	p.mu.Unlock()

	select {
	case r := <-ped.resposta:
		return r.dados, time.Since(ini), r.err
	case <-time.After(esperaRespostaPonte):
		p.mu.Lock()
		delete(p.aguardando, ped.ID)
		p.mu.Unlock()
		return nil, time.Since(ini), errors.New("a ponte com o monitoramento não respondeu a tempo")
	}
}

// cors libera as rotas da ponte para as origens configuradas (a aba do Zabbix).
func (p *Ponte) cors(w http.ResponseWriter, r *http.Request) bool {
	origem := r.Header.Get("Origin")
	if origem != "" {
		if !p.origens[origem] && !p.origens["*"] {
			http.Error(w, "origem não autorizada para a ponte: "+origem, http.StatusForbidden)
			return false
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origem)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Allow-Private-Network", "true") // página pública chamando 127.0.0.1 (Chrome)
		h.Set("Access-Control-Max-Age", "600")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

func (s *Servidor) pontePedidos(w http.ResponseWriter, r *http.Request) {
	p := s.ponte
	if !p.cors(w, r) {
		return
	}
	espera, _ := strconv.Atoi(r.URL.Query().Get("espera"))
	espera = min(max(espera, 0), 25)
	p.mu.Lock()
	primeiroContato := time.Since(p.contato) >= ponteSilenciosa
	p.contato = time.Now()
	if m := r.URL.Query().Get("modo"); m != "" {
		p.modo = m
	}
	modo := p.modo
	p.mu.Unlock()
	if primeiroContato {
		log.Printf("ponte: conectada (%s, modo %s)", r.Header.Get("Origin"), modo)
		go func() { time.Sleep(2 * time.Second); s.buscarAlarmes("conexão da ponte", false) }() // busca os alarmes assim que a ponte liga
	}

	recursos := map[string]bool{}
	for _, rec := range strings.Split(r.URL.Query().Get("recursos"), ",") {
		recursos[strings.TrimSpace(rec)] = true
	}
	prazo := time.After(time.Duration(espera) * time.Second)
	for {
		p.mu.Lock()
		// cada ponte só recebe o que sabe fazer (uma versão antiga do script não recebe o pedido de alarmes)
		lista, resto := []*pedidoPonte{}, []*pedidoPonte{}
		for _, ped := range p.fila {
			if ped.Tipo == "" || recursos[ped.Tipo] {
				lista = append(lista, ped)
			} else {
				resto = append(resto, ped)
			}
		}
		if len(lista) > 0 || espera == 0 {
			p.fila = resto
			p.mu.Unlock()
			responder(w, http.StatusOK, lista)
			return
		}
		novo := p.novo
		p.mu.Unlock()
		select {
		case <-novo:
		case <-prazo:
			espera = 0
		case <-r.Context().Done():
			return
		}
		p.mu.Lock()
		p.contato = time.Now()
		p.mu.Unlock()
	}
}

func (s *Servidor) ponteResposta(w http.ResponseWriter, r *http.Request) {
	p := s.ponte
	if !p.cors(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	id, _ := inteiro(m["id"])
	p.mu.Lock()
	ped, ok := p.aguardando[id]
	delete(p.aguardando, id)
	p.contato = time.Now()
	p.mu.Unlock()
	if !ok {
		responder(w, http.StatusGone, map[string]any{"erro": "pedido expirado"})
		return
	}
	if e := texto(m["erro"]); e != "" {
		ped.resposta <- respostaPonte{err: errors.New(e)}
	} else {
		ped.resposta <- respostaPonte{dados: m}
	}
	responder(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /ponte-zabbix.js: o script que roda na aba do Zabbix
func (s *Servidor) ponteScript(w http.ResponseWriter, r *http.Request) {
	if !s.ponte.cors(w, r) {
		return
	}
	js, err := os.ReadFile(s.ponte.script)
	if err != nil {
		http.Error(w, "script da ponte não encontrado: "+s.ponte.script, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(js)
}

// fonteMista: GPON pela ponte com o Zabbix; Core e IVR pela API de -monitoramento.
type fonteMista struct {
	gpon, outras Fonte
}

func (f fonteMista) Consultar(olt, pon string, de, ate int64) (map[string]any, time.Duration, error) {
	if normalizarCategoria("", olt) == "gpon" || f.outras == nil {
		return f.gpon.Consultar(olt, pon, de, ate)
	}
	return f.outras.Consultar(olt, pon, de, ate)
}
