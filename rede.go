// Eventos de rede: quedas de OLT recebidas por webhook (POST /webhook/rede).
//
// Um evento chega quando a OLT cai, normalmente só com host (OLT) e PON; os contadores e
// os gráficos vêm da consulta ao monitoramento (consulta.go). O mesmo "id" também pode
// chegar de novo com mais pontos e, no fim, o horário em que normalizou. As
// ações dos operadores (acompanhar, abrir O.S. técnica, encerrar) ficam aqui no
// servidor para todos verem o mesmo estado. Tudo é gravado em eventos-rede.jsonl
// como um diário: na partida o arquivo é relido e as linhas reaplicadas em ordem.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ponto é uma amostra por minuto do monitoramento, para os gráficos de antes e depois da queda.
type Ponto struct {
	T           int64   `json:"t"`
	Online      int     `json:"online"`
	LOS         int     `json:"los"`
	LOSi        int     `json:"losi"`
	LOFi        int     `json:"lofi"`
	SemEnergia  int     `json:"semEnergia"`
	SinalAlto   int     `json:"sinalAlto,omitempty"`   // IVR: clientes com sinal alto / ruído / interferência
	TempAlta    int     `json:"tempAlta,omitempty"`    // IVR: clientes afetados por temperatura alta no concentrador
	Temperatura float64 `json:"temperatura,omitempty"` // IVR: temperatura do concentrador/torre em °C
	Rx          float64 `json:"rx,omitempty"`          // potência óptica média em dBm (GPON / Core)
}

type AcaoRede struct {
	Acao    string `json:"acao"`
	Em      int64  `json:"em"`
	Detalhe string `json:"detalhe,omitempty"`
}

type EventoRede struct {
	ID              string   `json:"id"`
	Categoria       string   `json:"categoria"` // "gpon" | "core" | "ivr"
	OLT             string   `json:"olt"`       // GPON: OLT | Core: equipamento de backbone | IVR: concentrador ou torre
	Cidade          string   `json:"cidade,omitempty"`
	CidadesAfetadas []string `json:"cidadesAfetadas,omitempty"` // Core: cidades atendidas pelo enlace de backbone
	Local           string   `json:"local,omitempty"`
	Marca           string   `json:"marca,omitempty"`
	PON             string   `json:"pon,omitempty"` // GPON: PON | Core: enlace/porta | IVR: setor/concentrador
	OcorridoEm      int64    `json:"ocorridoEm"`
	NormalizadoEm   int64    `json:"normalizadoEm,omitempty"`
	TotalClientes   int      `json:"totalClientes,omitempty"`
	LOS             int      `json:"los"`
	LOSi            int      `json:"losi"`
	LOFi            int      `json:"lofi"`
	SemEnergia      int      `json:"semEnergia"`
	SinalAlto       int      `json:"sinalAlto,omitempty"`   // IVR: sinal alto / interferência
	TempAlta        int      `json:"tempAlta,omitempty"`    // IVR: temperatura alta
	Temperatura     float64  `json:"temperatura,omitempty"` // IVR: temperatura do concentrador em °C
	Serie           []Ponto  `json:"serie"`
	// do alarme no Zabbix
	Severidade *int        `json:"severidade,omitempty"` // 0–5: não classificada … desastre
	Alarme     string      `json:"alarme,omitempty"`     // nome do problema
	Tags       []TagAlarme `json:"tags,omitempty"`
	// em tratativa: quem pegou o chamado
	Tratando      string `json:"tratando,omitempty"`
	TratandoDesde int64  `json:"tratandoDesde,omitempty"`
	// tratamento: novo (triagem, equipe 1) | acompanhamento (equipe 2) | recuperado | os_aberta | encerrado
	Status            string     `json:"status"`
	OS                string     `json:"os,omitempty"`
	OSAbertaEm        int64      `json:"osAbertaEm,omitempty"`
	AcompanhandoDesde int64      `json:"acompanhandoDesde,omitempty"`
	EncerradoEm       int64      `json:"encerradoEm,omitempty"`
	Historico         []AcaoRede `json:"historico"`
	RecebidoEm        int64      `json:"recebidoEm"`
	// consulta no monitoramento
	UltimaConsulta int64  `json:"ultimaConsulta,omitempty"`
	ConsultaMs     int    `json:"consultaMs,omitempty"`
	ErroConsulta   string `json:"erroConsulta,omitempty"`
	// acompanhamento: o chamado vira "recuperado" quando passa a ter mais clientes ON que a margem
	Margem       float64 `json:"margem"` // % ON que valia quando o chamado foi aberto
	RecuperadoEm int64   `json:"recuperadoEm,omitempty"`
	AutoPausado  bool    `json:"autoPausado,omitempty"` // a equipe 2 ajustou o status na mão
}

// ---------- armazenamento ----------

type linhaDiario struct {
	Tipo     string          `json:"tipo"` // "evento" | "acao" | "config" | "os" | "os-status" | "os-fim" | "os-wpp"
	Margem   float64         `json:"margem,omitempty"`
	Clientes *int            `json:"clientes,omitempty"` // os-status: clientes afetados recalculados
	Dados    json.RawMessage `json:"dados,omitempty"`
	ID       string          `json:"id,omitempty"`
	Acao     string          `json:"acao,omitempty"`
	Em       int64           `json:"em,omitempty"`
	Detalhe  string          `json:"detalhe,omitempty"`
	Fim      int64           `json:"fim,omitempty"` // os-fim: quando o incidente acabou
	Por      string          `json:"por,omitempty"` // os-fim: supervisor | operador
}

type Rede struct {
	mu       sync.RWMutex
	eventos  map[string]*EventoRede
	ordem    []string
	osSeq    int
	margem   float64               // % ON abaixo do qual a PON está em incidente
	oss      map[string]*OSTecnica // O.S. técnicas pelo número
	ossOrdem []string
	arquivo  *os.File
}

func abrirRede(caminho string) (*Rede, error) {
	r := &Rede{eventos: map[string]*EventoRede{}, margem: margemPadrao}
	if f, err := os.Open(caminho); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var l linhaDiario
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			switch l.Tipo {
			case "evento":
				var m map[string]any
				dec := json.NewDecoder(strings.NewReader(string(l.Dados)))
				dec.UseNumber()
				if dec.Decode(&m) == nil {
					r.aplicar(m, l.Em)
				}
			case "acao":
				r.agir(l.ID, l.Acao, l.Detalhe, l.Em)
			case "config":
				r.margem = l.Margem
			case "os":
				var o OSTecnica
				if json.Unmarshal(l.Dados, &o) == nil {
					r.aplicarOS(&o)
				}
			case "os-status":
				r.StatusOS(l.ID, l.Detalhe, l.Em, false, l.Clientes)
			case "os-wpp":
				var env EnvioGrupo
				if json.Unmarshal(l.Dados, &env) == nil {
					r.RegistrarEnvio(l.ID, env, false)
				}
			case "os-fim":
				r.EncerrarOS(l.ID, l.Fim, l.Detalhe, l.Por, l.Em, false)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(caminho, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	r.arquivo = f
	return r, nil
}

func (r *Rede) gravar(l linhaDiario) {
	b, _ := json.Marshal(l)
	if _, err := r.arquivo.Write(append(b, '\n')); err != nil {
		log.Printf("erro ao gravar evento de rede: %v", err)
	}
}

// Receber valida, aplica e grava um item do webhook. Devolve uma cópia do evento atualizado.
func (r *Rede) Receber(m map[string]any) (EventoRede, bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agora := time.Now().UnixMilli()
	e, novo, motivo := r.aplicar(m, agora)
	if motivo != "" {
		return EventoRede{}, false, motivo
	}
	dados, _ := json.Marshal(m)
	r.gravar(linhaDiario{Tipo: "evento", Dados: dados, Em: agora})
	return copiar(e), novo, ""
}

// Agir aplica e grava uma ação de operador.
func (r *Rede) Agir(id, acao, detalhe string) (EventoRede, int, string) {
	if acao == "abrir_os" { // a O.S. técnica agora sai do formulário com os campos do comunicado
		return EventoRede{}, http.StatusBadRequest, "abra a O.S. pelo formulário (POST /api/rede/os)"
	}
	if acao == "assumir" && (detalhe == "" || len([]rune(detalhe)) > 60) {
		return EventoRede{}, http.StatusBadRequest, "informe quem vai tratar (até 60 caracteres)"
	}
	// dar baixa e mandar para acompanhamento exigem mensagem (só para ações novas; o diário antigo é relido sem essa checagem)
	if nome, exige := map[string]string{"encerrar": "de baixa", "acompanhar": "para o acompanhamento"}[acao]; exige {
		if detalhe == "" {
			return EventoRede{}, http.StatusBadRequest, "informe a mensagem " + nome
		}
		if len([]rune(detalhe)) > 500 {
			return EventoRede{}, http.StatusBadRequest, "a mensagem " + nome + " pode ter até 500 caracteres"
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	agora := time.Now().UnixMilli()
	e, status, motivo := r.agir(id, acao, detalhe, agora)
	if motivo != "" {
		return EventoRede{}, status, motivo
	}
	r.gravar(linhaDiario{Tipo: "acao", ID: id, Acao: acao, Detalhe: detalhe, Em: agora})
	return copiar(e), http.StatusOK, ""
}

const margemPadrao = 75.0

func (r *Rede) Margem() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.margem
}

// DefinirMargem muda a margem para os chamados abertos daqui em diante.
func (r *Rede) DefinirMargem(m float64) string {
	if m < 1 || m > 99 {
		return "a margem precisa estar entre 1 e 99% ON"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.margem = m
	r.gravar(linhaDiario{Tipo: "config", Margem: m, Em: time.Now().UnixMilli()})
	return ""
}

func (r *Rede) JSON() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lista := make([]*EventoRede, 0, len(r.ordem))
	for _, id := range r.ordem {
		lista = append(lista, r.eventos[id])
	}
	b, _ := json.Marshal(lista)
	return b
}

// Obter devolve uma cópia do evento.
func (r *Rede) Obter(id string) (EventoRede, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.eventos[id]
	if !ok {
		return EventoRede{}, false
	}
	return copiar(e), true
}

// ParaConsultar: eventos ainda ativos ou normalizados há pouco (e não encerrados).
func (r *Rede) ParaConsultar(agora, depois int64, soAcompanhamento bool) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := []string{}
	for _, id := range r.ordem {
		e := r.eventos[id]
		acompanhando := e.Status == "acompanhamento" || e.Status == "recuperado"
		temAlvo := e.PON != "" || e.Categoria == "ivr" || e.Categoria == "core"
		if soAcompanhamento && !acompanhando {
			continue
		}
		if temAlvo && e.Status != "encerrado" && (e.NormalizadoEm == 0 || agora-e.NormalizadoEm < depois || acompanhando) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *Rede) Total() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ordem)
}

func copiar(e *EventoRede) EventoRede {
	c := *e
	// listas nunca nulas: no JSON viravam "null" (a página quebrava com chamado ainda sem série)
	c.CidadesAfetadas = append([]string{}, e.CidadesAfetadas...)
	c.Tags = append([]TagAlarme{}, e.Tags...)
	c.Serie = append([]Ponto{}, e.Serie...)
	c.Historico = append([]AcaoRede{}, e.Historico...)
	return c
}

// normalizarCategoria identifica se o chamado é "gpon", "core" ou "ivr".
func normalizarCategoria(cat, olt string) string {
	s := semAcento.Replace(strings.ToLower(strings.TrimSpace(cat)))
	switch {
	case s == "core" || strings.Contains(s, "backbone") || s == "bkb" || s == "dwdm":
		return "core"
	case s == "ivr" || strings.Contains(s, "radio") || strings.Contains(s, "torre") || strings.Contains(s, "concentrador"):
		return "ivr"
	case s == "gpon" || s == "fibra" || s == "olt":
		return "gpon"
	}
	u := strings.ToUpper(strings.TrimSpace(olt))
	if i := strings.LastIndex(u, "."); i >= 0 { // padrão do Zabbix: EMPRESA.CIDADE.LOCAL.OLT-MARCA-NNN
		u = u[i+1:]
	}
	switch {
	case strings.HasPrefix(u, "CORE-") || strings.HasPrefix(u, "BKB-") || strings.HasPrefix(u, "DWDM-") || strings.HasPrefix(u, "SW-CORE-") || strings.HasPrefix(u, "RTR-"):
		return "core"
	case strings.HasPrefix(u, "IVR-") || strings.HasPrefix(u, "TORRE-") || strings.HasPrefix(u, "CONC-") || strings.HasPrefix(u, "RADIO-") || strings.HasPrefix(u, "POP-RADIO-"):
		return "ivr"
	default:
		return "gpon"
	}
}

// aplicar cria o evento ou atualiza um existente com os campos presentes em m.
func (r *Rede) aplicar(m map[string]any, recebidoEm int64) (*EventoRede, bool, string) {
	olt := strings.ToUpper(texto(primeiro(m, "olt", "host", "equipamento", "torre", "concentrador", "core", "backbone")))
	ocorrido, temData := dataOpcional(primeiro(m, "ocorridoEm", "ocorrido_em", "inicio", "timestamp", "data"))
	if temData < 0 {
		return nil, false, `"ocorridoEm" não é uma data válida`
	}
	id := texto(primeiro(m, "id", "evento_id", "eventid"))
	if id == "" && olt != "" && temData > 0 {
		id = olt + "@" + strconv.FormatInt(ocorrido, 10)
	}
	if id == "" {
		return nil, false, `faltou "id" (ou "olt"/"equipamento" + "ocorridoEm")`
	}

	catInformada := texto(primeiro(m, "categoria", "camada", "tecnologia", "rede"))
	e, existe := r.eventos[id]
	if !existe {
		if m["somenteAtualizar"] == true {
			return nil, false, "alarme resolvido sem chamado aberto"
		}
		if olt == "" {
			return nil, false, `faltou "olt" (ou "host", "equipamento", "torre", "concentrador")`
		}
		if temData == 0 {
			ocorrido = recebidoEm
		}
		e = &EventoRede{
			ID: id, Categoria: normalizarCategoria(catInformada, olt), OLT: olt,
			OcorridoEm: ocorrido, Status: "novo", RecebidoEm: recebidoEm, Serie: []Ponto{}, Margem: r.margem,
		}
		e.Historico = []AcaoRede{{Acao: "recebido", Em: recebidoEm}}
		r.eventos[id] = e
		r.ordem = append(r.ordem, id)
	} else {
		if temData > 0 {
			e.OcorridoEm = ocorrido
		}
		if catInformada != "" || e.Categoria == "" {
			e.Categoria = normalizarCategoria(catInformada, e.OLT)
		}
	}

	for campo, destino := range map[string]*string{"cidade": &e.Cidade, "local": &e.Local, "marca": &e.Marca} {
		if v := texto(m[campo]); v != "" {
			*destino = v
		}
	}
	if v := texto(primeiro(m, "pon", "porta", "enlace", "setor", "interface")); v != "" {
		e.PON = v
	}
	if lista, ok := primeiro(m, "cidadesAfetadas", "cidades_afetadas", "cidades").([]any); ok {
		cids := []string{}
		for _, it := range lista {
			if s := texto(it); s != "" {
				cids = append(cids, s)
			}
		}
		if len(cids) > 0 {
			e.CidadesAfetadas = cids
		}
	}
	for _, c := range []struct {
		chaves  []string
		destino *int
	}{
		{[]string{"los", "offline"}, &e.LOS}, {[]string{"losi", "LOSi"}, &e.LOSi}, {[]string{"lofi", "LOFi"}, &e.LOFi},
		{[]string{"semEnergia", "sem_energia", "dying_gasp", "dyingGasp"}, &e.SemEnergia},
		{[]string{"sinalAlto", "sinal_alto", "ruido", "interferencia"}, &e.SinalAlto},
		{[]string{"tempAlta", "temp_alta", "temperaturaAlta", "temperatura_alta"}, &e.TempAlta},
		{[]string{"totalClientes", "total_clientes", "clientes"}, &e.TotalClientes},
	} {
		if n, ok := inteiro(primeiro(m, c.chaves...)); ok {
			*c.destino = n
		}
	}
	if f, err := strconv.ParseFloat(texto(primeiro(m, "temperatura", "temp", "graus")), 64); err == nil && f > 0 {
		e.Temperatura = f
	}
	if n, ok := inteiro(m["severidade"]); ok && n >= 0 && n < len(nomesSeveridade) {
		e.Severidade = &n
	}
	if v := texto(m["alarme"]); v != "" {
		e.Alarme = v
	}
	if _, ok := m["tags"]; ok {
		e.Tags = lerTags(m["tags"])
	}
	// reconhecimentos no Zabbix entram no histórico uma vez só
	if recs, ok := m["reconhecimentos"].([]any); ok {
		vistos := map[string]bool{}
		for _, h := range e.Historico {
			if h.Acao == "reconhecido" {
				vistos[strconv.FormatInt(h.Em, 10)+"|"+h.Detalhe] = true
			}
		}
		for _, it := range recs {
			a, ok := it.(map[string]any)
			if !ok {
				continue
			}
			em, lido := dataOpcional(a["em"])
			if lido <= 0 {
				continue
			}
			detalhe := texto(a["usuario"])
			if msg := texto(a["mensagem"]); msg != "" {
				detalhe = strings.TrimPrefix(detalhe+": "+msg, ": ")
			}
			if chave := strconv.FormatInt(em, 10) + "|" + detalhe; !vistos[chave] {
				vistos[chave] = true
				e.Historico = append(e.Historico, AcaoRede{Acao: "reconhecido", Em: em, Detalhe: detalhe})
			}
		}
	}
	if n, ok := inteiro(m["ultimaConsulta"]); ok {
		e.UltimaConsulta = int64(n)
	}
	if n, ok := inteiro(m["consultaMs"]); ok {
		e.ConsultaMs = n
	}
	if v, ok := m["erroConsulta"]; ok {
		e.ErroConsulta = texto(v)
	}
	if fim, ok := dataOpcional(primeiro(m, "normalizadoEm", "normalizado_em", "recuperadoEm", "fim")); ok > 0 && e.NormalizadoEm == 0 {
		e.NormalizadoEm = fim
		e.Historico = append(e.Historico, AcaoRede{Acao: "normalizado", Em: fim})
	}
	if lista, ok := primeiro(m, "serie", "series", "pontos").([]any); ok {
		vistos := map[int64]int{}
		for i, p := range e.Serie {
			vistos[p.T] = i
		}
		for _, item := range lista {
			pm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t, lido := dataOpcional(primeiro(pm, "t", "timestamp", "em"))
			if lido <= 0 {
				continue
			}
			p := Ponto{T: t}
			p.Online, _ = inteiro(pm["online"])
			p.LOS, _ = inteiro(primeiro(pm, "los", "offline"))
			p.LOSi, _ = inteiro(primeiro(pm, "losi", "LOSi"))
			p.LOFi, _ = inteiro(primeiro(pm, "lofi", "LOFi"))
			p.SemEnergia, _ = inteiro(primeiro(pm, "semEnergia", "sem_energia", "dying_gasp"))
			p.SinalAlto, _ = inteiro(primeiro(pm, "sinalAlto", "sinal_alto"))
			p.TempAlta, _ = inteiro(primeiro(pm, "tempAlta", "temp_alta"))
			if f, err := strconv.ParseFloat(texto(primeiro(pm, "temperatura", "temp")), 64); err == nil {
				p.Temperatura = f
			}
			if f, err := strconv.ParseFloat(texto(pm["rx"]), 64); err == nil {
				p.Rx = f
			}
			if _, repetido := vistos[t]; !repetido { // minuto já recebido mantém o primeiro valor
				vistos[t] = len(e.Serie)
				e.Serie = append(e.Serie, p)
			}
		}
		sort.Slice(e.Serie, func(i, j int) bool { return e.Serie[i].T < e.Serie[j].T })
	}
	avaliarRecuperacao(e)
	return e, !existe, ""
}

// onlineAgora: % de clientes ON no último minuto depois da queda (ok = false sem dados).
func onlineAgora(e *EventoRede) (pct float64, p Ponto, ok bool) {
	if e.TotalClientes <= 0 || len(e.Serie) == 0 {
		return 0, Ponto{}, false
	}
	p = e.Serie[len(e.Serie)-1]
	if p.T < e.OcorridoEm {
		return 0, p, false
	}
	return float64(p.Online) * 100 / float64(e.TotalClientes), p, true
}

// avaliarRecuperacao: em acompanhamento, passa para "recuperado" quando a PON tem mais clientes ON
// que a margem; se cair de novo, volta para acompanhamento. Não mexe se a equipe 2 ajustou na mão.
func avaliarRecuperacao(e *EventoRede) {
	pct, p, ok := onlineAgora(e)
	if !ok || e.AutoPausado {
		return
	}
	acima := pct > e.Margem
	detalhe := fmt.Sprintf("%.0f%% ON, margem %.0f%%", pct, e.Margem)
	switch {
	case e.Status == "acompanhamento" && acima:
		e.Status, e.RecuperadoEm = "recuperado", p.T
		e.Historico = append(e.Historico, AcaoRede{Acao: "recuperado_auto", Em: p.T, Detalhe: detalhe})
	case e.Status == "recuperado" && !acima:
		e.Status, e.RecuperadoEm = "acompanhamento", 0
		e.Historico = append(e.Historico, AcaoRede{Acao: "abaixo_da_margem", Em: p.T, Detalhe: detalhe})
	}
}

func (r *Rede) agir(id, acao, detalhe string, em int64) (*EventoRede, int, string) {
	e, ok := r.eventos[id]
	if !ok {
		return nil, http.StatusNotFound, "evento não encontrado"
	}
	registrar := func(d string) { e.Historico = append(e.Historico, AcaoRede{Acao: acao, Em: em, Detalhe: d}) }
	switch acao {
	case "acompanhar": // equipe 1: problema de energia vai para a equipe 2 acompanhar a volta
		if e.Status == "encerrado" {
			return nil, http.StatusConflict, "evento encerrado"
		}
		e.AcompanhandoDesde, e.Status, e.RecuperadoEm = em, "acompanhamento", 0
		registrar(detalhe)
		avaliarRecuperacao(e)
	case "parar_acompanhamento": // volta para a triagem
		e.AcompanhandoDesde, e.RecuperadoEm, e.AutoPausado = 0, 0, false
		if e.OS != "" {
			e.Status = "os_aberta"
		} else {
			e.Status = "novo"
		}
		registrar(detalhe)
	case "recuperado": // equipe 2 marca na mão
		e.Status, e.RecuperadoEm, e.AutoPausado = "recuperado", em, true
		registrar(detalhe)
	case "voltar_acompanhamento": // equipe 2 corrige um "recuperado" errado
		e.Status, e.RecuperadoEm, e.AutoPausado = "acompanhamento", 0, true
		registrar(detalhe)
	case "retomar_auto":
		e.AutoPausado = false
		registrar(detalhe)
		avaliarRecuperacao(e)
	case "abrir_os":
		if e.OS != "" {
			return nil, http.StatusConflict, "este evento já tem a O.S. " + e.OS
		}
		r.osSeq++
		e.OS, e.OSAbertaEm, e.Status = fmt.Sprintf("OS-T%06d", 100000+r.osSeq), em, "os_aberta"
		registrar(e.OS)
	case "encerrar":
		e.Status, e.EncerradoEm = "encerrado", em
		e.Tratando, e.TratandoDesde = "", 0
		registrar(detalhe)
	case "assumir": // em tratativa: um operador pega o chamado (outro pode assumir no lugar)
		if e.Status == "encerrado" {
			return nil, http.StatusConflict, "evento encerrado"
		}
		e.Tratando, e.TratandoDesde = detalhe, em
		registrar(detalhe)
	case "liberar":
		e.Tratando, e.TratandoDesde = "", 0
		registrar(detalhe)
	case "reabrir":
		e.EncerradoEm = 0
		switch {
		case e.OS != "":
			e.Status = "os_aberta"
		case e.AcompanhandoDesde > 0:
			e.Status = "acompanhamento"
			defer avaliarRecuperacao(e)
		default:
			e.Status = "novo"
		}
		registrar(detalhe)
	default:
		return nil, http.StatusBadRequest, `ação inválida: use "acompanhar", "parar_acompanhamento", "recuperado", "voltar_acompanhamento", "retomar_auto", "abrir_os", "encerrar", "reabrir", "assumir" ou "liberar"`
	}
	return e, http.StatusOK, ""
}

func inteiro(v any) (int, bool) {
	s := texto(v)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int(math.Round(f)), true
}

// dataOpcional: 1 = data lida, 0 = ausente, -1 = presente mas inválida.
func dataOpcional(v any) (int64, int) {
	if texto(v) == "" {
		return 0, 0
	}
	t, ok := normalizarData(v)
	if !ok {
		return 0, -1
	}
	return t, 1
}

// ---------- rotas ----------

func (s *Servidor) webhookRede(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && !tokenValido(r, s.token) {
		responder(w, http.StatusUnauthorized, map[string]any{"erro": "token inválido ou ausente"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var corpo any
	if err := dec.Decode(&corpo); err != nil {
		var grande *http.MaxBytesError
		if errors.As(err, &grande) {
			responder(w, http.StatusRequestEntityTooLarge, map[string]any{"erro": "corpo maior que 4 MB"})
		} else {
			responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido: " + err.Error()})
		}
		return
	}
	var itens []any
	switch v := corpo.(type) {
	case []any:
		itens = v
	case map[string]any:
		if servidores, ok := v["servers"].([]any); ok { // resposta de um painel que agrega vários Zabbix
			for _, sv := range servidores {
				svm, _ := sv.(map[string]any)
				lista, _ := svm["triggers"].([]any)
				for _, t := range lista {
					if tm, ok := t.(map[string]any); ok && tm["server"] == nil {
						tm["server"] = svm["server"]
					}
					itens = append(itens, t)
				}
			}
		} else if lista, ok := v["eventos"].([]any); ok {
			itens = lista
		} else {
			itens = []any{v}
		}
	}
	// ?severidadeMinima=4 descarta triggers abaixo de Alta (útil ao mandar o painel inteiro)
	sevMin, _ := strconv.Atoi(r.URL.Query().Get("severidadeMinima"))
	novos, atualizados, ignorados, rejeitados := 0, 0, 0, []rejeicao{}
	for i, item := range itens {
		m, ok := item.(map[string]any)
		if !ok {
			rejeitados = append(rejeitados, rejeicao{i, "item não é um objeto"})
			continue
		}
		if ehTrigger(m) {
			if n, ok := inteiro(m["priority"]); ok && n < sevMin {
				ignorados++
				continue
			}
			m = deTrigger(m, "")
		}
		e, novo, motivo := s.rede.Receber(m)
		if motivo != "" {
			rejeitados = append(rejeitados, rejeicao{i, motivo})
			continue
		}
		if novo {
			novos++
			log.Printf("rede [%s]: incidente em %s %s", strings.ToUpper(e.Categoria), e.OLT, e.PON)
			if s.fonte != nil && (e.PON != "" || e.Categoria == "ivr" || e.Categoria == "core") && !(s.ponte != nil && e.Categoria == "gpon") {
				go s.consultarEvento(e.ID) // busca contadores e gráficos na hora (pela ponte: só ao abrir o chamado)
			}
		} else {
			atualizados++
		}
		s.enviarEvento(e)
	}
	status := http.StatusOK
	if novos+atualizados == 0 && len(rejeitados) > 0 {
		status = http.StatusUnprocessableEntity
	}
	responder(w, status, map[string]any{"novos": novos, "atualizados": atualizados, "ignorados": ignorados, "rejeitados": rejeitados})
}

func (s *Servidor) enviarEvento(e EventoRede) {
	dados, _ := json.Marshal(e)
	s.hub.Enviar([]byte("event: rede\ndata: " + string(dados) + "\n\n"))
}

func (s *Servidor) listarRede(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.rede.JSON())
}

// POST /api/rede/config {"margem": 75}: margem de incidente para os próximos chamados.
func (s *Servidor) configRede(w http.ResponseWriter, r *http.Request) {
	var corpo struct {
		Margem float64 `json:"margem"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&corpo); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	if motivo := s.rede.DefinirMargem(corpo.Margem); motivo != "" {
		responder(w, http.StatusBadRequest, map[string]any{"erro": motivo})
		return
	}
	log.Printf("rede: margem de incidente agora é %.0f%% ON", corpo.Margem)
	dados, _ := json.Marshal(map[string]any{"margem": corpo.Margem})
	s.hub.Enviar([]byte("event: rede-config\ndata: " + string(dados) + "\n\n"))
	responder(w, http.StatusOK, map[string]any{"margem": corpo.Margem})
}

// Ações vindas da página (triagem, acompanhamento, O.S. técnica, baixa).
func (s *Servidor) acaoRede(w http.ResponseWriter, r *http.Request) {
	var corpo struct {
		ID      string `json:"id"`
		Acao    string `json:"acao"`
		Detalhe string `json:"detalhe"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&corpo); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	e, status, motivo := s.rede.Agir(corpo.ID, corpo.Acao, strings.TrimSpace(corpo.Detalhe))
	if motivo != "" {
		responder(w, status, map[string]any{"erro": motivo})
		return
	}
	log.Printf("rede: %s em %s", corpo.Acao, e.OLT)
	s.enviarEvento(e)
	responder(w, http.StatusOK, e)
}

// ---------- simulador: o sistema de monitoramento ----------
//
// Quando sorteia uma queda massiva, o simulador avisa o Termógrafo (GPON, Core ou IVR)
// e responde as consultas numa API própria (GET /api/pon?olt=...&pon=...&de=...&ate=...).

var codigoCidadeSim = map[string]string{
	"Campinas": "CMP", "Sumaré": "SUM", "Hortolândia": "HTL", "Americana": "AMR", "Indaiatuba": "IDT",
	"Santa Bárbara d'Oeste": "SBO", "Valinhos": "VLH", "Paulínia": "PLN", "Nova Odessa": "NOD", "Vinhedo": "VNH",
	"Monte Mor": "MMR", "Cosmópolis": "CSM", "Jundiaí": "JDI", "Piracicaba": "PCB", "Limeira": "LIM",
	"Rio Claro": "RCL", "Itu": "ITU", "Mogi Guaçu": "MGG", "Araras": "ARR", "Salto": "SLT",
	"Mogi Mirim": "MMI", "Itatiba": "ITB", "Jaguariúna": "JGR", "Amparo": "AMP", "Itupeva": "IPV",
	"Artur Nogueira": "ANG", "Louveira": "LVR", "Capivari": "CPV", "Pedreira": "PDR",
	"Santo Antônio de Posse": "SAP", "Holambra": "HLB", "Engenheiro Coelho": "ECO", "Iracemápolis": "IRC",
	"Elias Fausto": "EFS", "Rafard": "RFD",
}

// redeSim é o comportamento de uma PON, enlace Core ou torre/concentrador IVR ao longo do tempo.
type redeSim struct {
	id, categoria, olt, pon string
	cidades                 []string
	inicio                  time.Time // início da queda (no futuro distante quando não há queda)
	fimQueda                time.Time
	total                   int
	afetados                int
	tipo                    string     // fibra | energia | sinal | temperatura | "" (sem queda)
	partes                  [4]float64 // GPON/Core: LOS, LOSi, LOFi, semEnergia | IVR: LOS/offline, sinalAlto, tempAlta, semEnergia
	rx                      float64    // GPON/Core: potência óptica dBm | IVR: 0
	tempPico                float64    // IVR: temperatura no pico em °C
	semente                 uint64     // o mesmo minuto sempre devolve os mesmos valores, consulta após consulta
}

// categoria: "gpon", "core", "ivr" ou "" para sortear
// tipo: "fibra", "energia", "sinal", "temperatura" ou "" para sortear
func novaRedeSim(cidade string, peso float64, inicio time.Time, duracaoH int, tipo string, categoria string) *redeSim {
	if categoria == "" {
		u := rand.Float64()
		switch {
		case u < 0.55:
			categoria = "gpon"
		case u < 0.77:
			categoria = "core"
		default:
			categoria = "ivr"
		}
	} else {
		categoria = normalizarCategoria(categoria, "")
	}

	cod := codigoCidadeSim[cidade]
	r := &redeSim{
		id:        fmt.Sprintf("EVT-%d", inicio.UnixMilli()),
		categoria: categoria,
		inicio:    inicio.Truncate(time.Minute),
		semente:   rand.Uint64(),
	}
	r.fimQueda = r.inicio.Add(time.Duration(duracaoH)*time.Hour - time.Duration(rand.IntN(40))*time.Minute)

	switch categoria {
	case "core":
		// Backbone em maior escala: enlace entre cidades ou roteador/switch core de POP/DC
		outra := cidadesSim[rand.IntN(len(cidadesSim))].nome
		if outra == cidade {
			outra = "Campinas"
			if cidade == "Campinas" {
				outra = "Jundiaí"
			}
		}
		cod2 := codigoCidadeSim[outra]
		marcasCore := []string{"HUA", "NOK", "DAT", "ZTE"}
		marca := marcasCore[rand.IntN(len(marcasCore))]
		if rand.Float64() < 0.5 {
			r.olt = fmt.Sprintf("CORE-%s-DC-%s-%03d", cod, marca, 1+rand.IntN(2))
		} else {
			r.olt = fmt.Sprintf("BKB-%s-%s-%s-%02d", cod, cod2, marca, 1+rand.IntN(2))
		}
		portas := []string{"100GE-1/0/1", "100GE-1/0/2", "40GE-0/1/1", "10GE-2/0/4"}
		r.pon = fmt.Sprintf("%s · %s ↔ %s", portas[rand.IntN(len(portas))], cidade, outra)
		r.cidades = []string{cidade, outra}
		r.total = int(math.Min(4800, math.Max(1200, peso*140*(0.7+rand.Float64()*0.6))))
		r.rx = -14.5 - rand.Float64()*2.0
		x := rand.Float64()
		switch tipo {
		case "fibra":
			x = 0.1
		case "energia":
			x = 0.65
		case "sinal":
			x = 0.9
		}
		switch {
		case x < 0.55:
			r.tipo, r.partes, r.afetados = "fibra", [4]float64{.88, .04, .04, .04}, int(float64(r.total)*(0.65+rand.Float64()*0.30))
		case x < 0.80:
			r.tipo, r.partes, r.afetados = "energia", [4]float64{.12, .03, .03, .82}, int(float64(r.total)*(0.50+rand.Float64()*0.35))
		default:
			r.tipo, r.partes, r.afetados = "sinal", [4]float64{.15, .48, .32, .05}, int(float64(r.total)*(0.25+rand.Float64()*0.35))
		}

	case "ivr":
		// IVR: concentrador ou torre de rádio, sem OLT e sem sinal de fibra; monitoramento menos preciso
		zonas := []string{"TORRE", "CONC", "POP"}
		marcasIVR := []string{"MK", "UBN", "CMB", "INT"}
		prefixos := []string{"TORRE", "CONC"}
		pref := prefixos[rand.IntN(len(prefixos))]
		r.olt = fmt.Sprintf("%s-%s-%s-%s-%03d", pref, cod, zonas[rand.IntN(len(zonas))], marcasIVR[rand.IntN(len(marcasIVR))], 1+rand.IntN(4))
		setores := []string{"SETOR-NORTE 5.8GHz", "SETOR-SUL 5.8GHz", "PAINEL-01", "CONCENTRADOR-PPPoE"}
		r.pon = setores[rand.IntN(len(setores))]
		r.cidades = []string{cidade}
		r.total = 45 + rand.IntN(135)
		r.rx = 0 // sem fibra óptica
		x := rand.Float64()
		switch tipo {
		case "sinal":
			x = 0.1
		case "temperatura", "fibra":
			x = 0.5
		case "energia":
			x = 0.85
		}
		// Em IVR, partes = [0: offline/sem resposta, 1: sinalAlto, 2: tempAlta, 3: semEnergia]
		switch {
		case x < 0.40:
			r.tipo, r.partes, r.afetados = "sinal", [4]float64{.14, .74, .06, .06}, int(float64(r.total)*(0.35+rand.Float64()*0.40))
			r.tempPico = 48 + rand.Float64()*5
		case x < 0.72:
			r.tipo, r.partes, r.afetados = "temperatura", [4]float64{.15, .15, .64, .06}, int(float64(r.total)*(0.45+rand.Float64()*0.40))
			r.tempPico = 76 + rand.Float64()*9
		default:
			r.tipo, r.partes, r.afetados = "energia", [4]float64{.12, .05, .03, .80}, int(float64(r.total)*(0.60+rand.Float64()*0.35))
			r.tempPico = 41 + rand.Float64()*4
		}

	default:
		// GPON: OLT + PON
		// mesmo padrão do Zabbix real: EMP.CID.POP2.OLT-FH-001, PON 2/16
		locais, marcas := []string{"DC1", "POP1", "POP2", "POP3", "ARM1"}, []string{"FH", "FH", "ZTE", "HW", "NK"}
		r.olt = fmt.Sprintf("EMP.%s.%s.OLT-%s-%03d", cod, locais[rand.IntN(len(locais))], marcas[rand.IntN(len(marcas))], 1+rand.IntN(3))
		r.pon = fmt.Sprintf("%d/%d", 1+rand.IntN(8), 1+rand.IntN(16))
		r.cidades = []string{cidade}
		r.total = int(math.Min(1024, math.Max(96, peso*14*(0.6+rand.Float64()*0.8))))
		r.rx = -20.5 - rand.Float64()*2
		x := rand.Float64()
		switch tipo {
		case "fibra":
			x = 0
		case "energia":
			x = 0.6
		case "sinal":
			x = 0.9
		}
		switch {
		case x < 0.5:
			r.tipo, r.partes, r.afetados = "fibra", [4]float64{.84, .05, .05, .06}, int(float64(r.total)*(0.55+rand.Float64()*0.4))
		case x < 0.8:
			r.tipo, r.partes, r.afetados = "energia", [4]float64{.14, .05, .04, .77}, int(float64(r.total)*(0.30+rand.Float64()*0.5))
		default:
			r.tipo, r.partes, r.afetados = "sinal", [4]float64{.15, .46, .34, .05}, int(float64(r.total)*(0.15+rand.Float64()*0.3))
		}
	}
	return r
}

// cenarioNormal: equipamento consultado sem queda registrada no simulador (só o dia a dia).
func cenarioNormal(olt, pon string) *redeSim {
	h := fnv.New64a()
	h.Write([]byte(olt + "|" + pon))
	s := h.Sum64()
	longe := time.Now().Add(100 * 365 * 24 * time.Hour)
	cat := normalizarCategoria("", olt)
	total := 96 + int(s%600)
	rx := -20 - float64(s%30)/10
	if cat == "core" {
		total = 1500 + int(s%2500)
		rx = -15 - float64(s%20)/10
	} else if cat == "ivr" {
		total = 50 + int(s%110)
		rx = 0
	}
	return &redeSim{categoria: cat, olt: olt, pon: pon, inicio: longe, fimQueda: longe, total: total, rx: rx, tempPico: 45, semente: s}
}

// ponto devolve a amostra no minuto t (antes, durante ou depois da queda).
func (r *redeSim) ponto(t time.Time) Ponto {
	cat := r.categoria
	if cat == "" {
		cat = normalizarCategoria("", r.olt)
	}
	if cat == "ivr" {
		// IVR tem leitura menos precisa (janelas de 3 min) e sem sinal óptico de fibra
		bloco := t.Unix() / 180
		rng := rand.New(rand.NewPCG(r.semente, uint64(bloco)))
		base := max(1, int(float64(r.total)*0.04))
		ruido := func(n int) int { return max(0, n+rng.IntN(5)-2) }
		p := Ponto{T: t.UnixMilli()}
		antes := r.inicio.Sub(t).Minutes()
		tempBase := 44.0 + float64(r.semente%6)
		switch {
		case antes > 0:
			p.Online = r.total - ruido(base)
			p.LOS, p.SinalAlto, p.TempAlta, p.SemEnergia = ruido(1), ruido(base/2), 0, 0
			p.Temperatura = math.Round((tempBase+rng.NormFloat64()*0.8)*10) / 10
			if r.tipo == "temperatura" && antes <= 25 {
				k := (25 - antes) / 25
				p.Temperatura = math.Round((tempBase+(r.tempPico-tempBase)*k)*10) / 10
				p.TempAlta = int(k * float64(r.afetados) * 0.3)
				p.Online -= p.TempAlta
			}
		case t.Before(r.fimQueda):
			af := float64(r.afetados) * math.Pow(0.78, t.Sub(r.inicio).Hours())
			janela := math.Min(15, r.fimQueda.Sub(r.inicio).Minutes()/2)
			if resta := r.fimQueda.Sub(t).Minutes(); resta < janela {
				af *= resta / janela
			}
			p.Online = max(0, r.total-base-int(af))
			p.LOS = ruido(int(af * r.partes[0]))
			p.SinalAlto = ruido(int(af * r.partes[1]))
			p.TempAlta = ruido(int(af * r.partes[2]))
			p.SemEnergia = ruido(int(af * r.partes[3]))
			if r.tipo == "temperatura" {
				p.Temperatura = math.Round((r.tempPico+rng.NormFloat64()*1.2)*10) / 10
			} else {
				p.Temperatura = math.Round((tempBase+rng.NormFloat64()*1.0)*10) / 10
			}
		default:
			p.Online = r.total - ruido(base)
			p.LOS, p.SinalAlto, p.TempAlta, p.SemEnergia = ruido(1), ruido(base/2), 0, 0
			p.Temperatura = math.Round((tempBase+rng.NormFloat64()*0.8)*10) / 10
		}
		return p
	}

	rng := rand.New(rand.NewPCG(r.semente, uint64(t.Unix()/60)))
	base := int(float64(r.total) * 0.025) // ONUs desligadas no dia a dia
	ruido := func(n int) int { return max(0, n+rng.IntN(3)-1) }
	p := Ponto{T: t.UnixMilli(), Rx: math.Round((r.rx+rng.NormFloat64()*0.15)*10) / 10}
	antes := r.inicio.Sub(t).Minutes() // > 0 antes da queda
	switch {
	case antes > 0: // normal; no problema de sinal a potência já vem caindo e LOSi/LOFi crescem nos últimos minutos
		p.Online = r.total - ruido(base)
		p.LOS, p.LOSi, p.LOFi, p.SemEnergia = ruido(base/2), ruido(1), ruido(1), ruido(0)
		if r.tipo == "sinal" && antes <= 20 {
			k := (20 - antes) / 20
			p.Rx = math.Round((r.rx-5*k+rng.NormFloat64()*0.2)*10) / 10
			p.LOSi += int(k * float64(r.afetados) * 0.25)
			p.LOFi += int(k * float64(r.afetados) * 0.18)
			p.Online -= int(k * float64(r.afetados) * 0.3)
		}
	case t.Before(r.fimQueda): // durante: afetados caem aos poucos; no fim, o reparo traz todos de volta
		af := float64(r.afetados) * math.Pow(0.72, t.Sub(r.inicio).Hours())
		janela := math.Min(15, r.fimQueda.Sub(r.inicio).Minutes()/2) // reparo: os clientes voltam nos últimos minutos
		if resta := r.fimQueda.Sub(t).Minutes(); resta < janela {
			af *= resta / janela
		}
		p.Online = r.total - base - int(af)
		p.LOS, p.LOSi, p.LOFi, p.SemEnergia = base/2+int(af*r.partes[0]), int(af*r.partes[1]), int(af*r.partes[2]), int(af*r.partes[3])
		if r.tipo == "sinal" {
			p.Rx = math.Round((r.rx-5+rng.NormFloat64()*0.3)*10) / 10
		}
	default: // depois: normal de novo
		p.Online = r.total - ruido(base)
		p.LOS, p.LOSi, p.LOFi, p.SemEnergia = ruido(base/2), ruido(1), ruido(1), 0
	}
	return p
}

// incidente: o aviso que o monitoramento manda
func (r *redeSim) incidente() map[string]any { return r.trigger(1, nil) }

// trigger: o alarme como o Zabbix manda (mesmo formato dos painéis que agregam vários Zabbix).
// valor 1 = problema, 0 = resolvido; acks = reconhecimentos com mensagem.
func (r *redeSim) trigger(valor int, acks []map[string]any) map[string]any {
	servidor := map[string]string{"gpon": "Zabbix GPON", "core": "Zabbix CORE", "ivr": "Zabbix Rádio"}[r.categoria]
	rotulo := map[string]string{"gpon": "PON", "core": "Enlace", "ivr": "Setor"}[r.categoria]
	grupo := map[string]string{"gpon": "OLTs", "core": "Backbone", "ivr": "Torres"}[r.categoria]
	categoria := map[string]string{"fibra": "Fibra", "energia": "Energia", "sinal": "Sinal", "temperatura": "Temperatura"}[r.tipo]
	descricao := map[string]map[string]string{
		"gpon": {"fibra": "Queda de ONUs na PON %s (LOS)", "energia": "ONUs sem energia na PON %s (Dying Gasp)", "sinal": "Sinal degradado na PON %s (LOSi/LOFi)"},
		"core": {"fibra": "Link down no enlace %s", "energia": "Falta de energia no POP do enlace %s", "sinal": "Atenuação alta no enlace %s"},
		"ivr":  {"fibra": "Setor %s sem resposta", "energia": "Torre sem energia (setor %s)", "sinal": "Interferência no setor %s", "temperatura": "Temperatura alta no concentrador (setor %s)"},
	}[r.categoria][r.tipo]
	if descricao == "" {
		descricao = "Falha no equipamento (%s)"
	}
	prioridade := 3
	switch {
	case r.categoria == "core" || (r.tipo == "fibra" && r.afetados >= 100):
		prioridade = 5
	case r.tipo == "fibra" || r.tipo == "energia":
		prioridade = 4
	}
	tags := []map[string]any{{"name": rotulo, "value": r.pon}, {"name": "Categoria", "value": categoria}}
	for _, c := range r.cidades {
		tags = append(tags, map[string]any{"name": "Cidade", "value": c})
	}
	t := map[string]any{
		"server": servidor, "id": fmt.Sprint(20000 + r.semente%70000), "eventId": fmt.Sprint(10_000_000 + r.semente%89_999_999),
		"description": fmt.Sprintf(descricao, r.pon), "priority": prioridade,
		"lastchange": r.inicio.Unix(), "status": "Incidente", "value": valor, "host": r.olt,
		"groups":       []map[string]any{{"groupid": "31", "name": grupo}},
		"problem_tags": tags, "trigger_tags": []map[string]any{{"name": "service", "value": ""}},
		"acknowledged": 0, "acknowledges_history": []map[string]any{},
	}
	if valor == 0 {
		t["lastchange"], t["status"] = r.fimQueda.Unix(), "OK"
	}
	if len(acks) > 0 {
		t["acknowledged"], t["acknowledges_history"] = 1, acks
	}
	return t
}

// ativos: cópia das quedas registradas no monitoramento simulado
func (mm *monitorMock) ativos() []*redeSim {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	lista := make([]*redeSim, 0, len(mm.cenarios))
	for _, r := range mm.cenarios {
		lista = append(lista, r)
	}
	return lista
}

type monitorMock struct {
	mu       sync.Mutex
	cenarios map[string]*redeSim // "olt|pon"
	arquivo  string
}

// cenarioArquivo: forma gravada em disco de um redeSim
type cenarioArquivo struct {
	ID, Categoria, OLT, PON, Tipo string
	Cidades                       []string
	Inicio, FimQueda              int64
	Total, Afetados               int
	Partes                        [4]float64
	Rx, TempPico                  float64
	Semente                       uint64
}

// carregarMonitor relê as quedas das últimas 24 h, para o simulador poder ser reiniciado sem perdê-las.
func carregarMonitor(arquivo string) *monitorMock {
	mm := &monitorMock{cenarios: map[string]*redeSim{}, arquivo: arquivo}
	b, err := os.ReadFile(arquivo)
	if err != nil {
		return mm
	}
	var lista []cenarioArquivo
	if json.Unmarshal(b, &lista) != nil {
		return mm
	}
	limite := time.Now().Add(-24 * time.Hour)
	for _, c := range lista {
		cat := c.Categoria
		if cat == "" {
			cat = normalizarCategoria("", c.OLT)
		}
		r := &redeSim{id: c.ID, categoria: cat, olt: c.OLT, pon: c.PON, cidades: c.Cidades, tipo: c.Tipo,
			inicio: time.UnixMilli(c.Inicio), fimQueda: time.UnixMilli(c.FimQueda),
			total: c.Total, afetados: c.Afetados, partes: c.Partes, rx: c.Rx, tempPico: c.TempPico, semente: c.Semente}
		if r.fimQueda.After(limite) {
			mm.cenarios[r.olt+"|"+r.pon] = r
		}
	}
	if len(mm.cenarios) > 0 {
		log.Printf("monitoramento simulado: %d queda(s) recarregada(s) de %s", len(mm.cenarios), arquivo)
	}
	return mm
}

func (mm *monitorMock) registrar(r *redeSim) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.cenarios[r.olt+"|"+r.pon] = r
	if mm.arquivo == "" {
		return
	}
	lista := []cenarioArquivo{}
	for _, c := range mm.cenarios {
		lista = append(lista, cenarioArquivo{c.id, c.categoria, c.olt, c.pon, c.tipo, c.cidades, c.inicio.UnixMilli(), c.fimQueda.UnixMilli(), c.total, c.afetados, c.partes, c.rx, c.tempPico, c.semente})
	}
	b, _ := json.MarshalIndent(lista, "", "  ")
	if err := os.WriteFile(mm.arquivo, b, 0o644); err != nil {
		log.Printf("não foi possível gravar %s: %v", mm.arquivo, err)
	}
}

// normalizar antecipa o fim de uma queda: os clientes voltam aos poucos até `fim`.
func (mm *monitorMock) normalizar(olt, pon string, fim time.Time) (*redeSim, bool) {
	mm.mu.Lock()
	r := mm.cenarios[olt+"|"+pon]
	mm.mu.Unlock()
	if r == nil || r.tipo == "" || !time.Now().Before(r.fimQueda) {
		return nil, false
	}
	c := *r
	c.fimQueda = fim
	mm.registrar(&c)
	return &c, true
}

func (mm *monitorMock) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/api/pon" {
		http.NotFound(w, req)
		return
	}
	q := req.URL.Query()
	olt, pon := strings.ToUpper(q.Get("olt")), q.Get("pon")
	if olt == "" {
		responder(w, http.StatusBadRequest, map[string]any{"erro": `informe "olt"`})
		return
	}
	agora := time.Now()
	ate, err1 := strconv.ParseInt(q.Get("ate"), 10, 64)
	de, err2 := strconv.ParseInt(q.Get("de"), 10, 64)
	if err1 != nil || ate > agora.UnixMilli() {
		ate = agora.UnixMilli()
	}
	if err2 != nil || de < ate-6*36e5 { // no máximo 6 h por consulta
		de = ate - 6*36e5
	}
	mm.mu.Lock()
	r := mm.cenarios[olt+"|"+pon]
	mm.mu.Unlock()
	if r == nil {
		r = cenarioNormal(olt, pon)
	}
	serie := []Ponto{}
	passo := time.Minute
	if r.categoria == "ivr" {
		passo = 3 * time.Minute // monitoramento IVR tem amostragem mais espaçada
	}
	for t := time.UnixMilli(de).Truncate(passo); !t.After(time.UnixMilli(ate)); t = t.Add(passo) {
		if !t.Before(time.UnixMilli(de)) {
			serie = append(serie, r.ponto(t))
		}
	}
	resp := map[string]any{"olt": olt, "pon": pon, "categoria": r.categoria, "totalClientes": r.total, "serie": serie}
	if len(r.cidades) > 1 {
		resp["cidadesAfetadas"] = r.cidades
	}
	if r.tipo != "" && !agora.Before(r.fimQueda) {
		resp["normalizadoEm"] = r.fimQueda.UnixMilli()
	}
	responder(w, http.StatusOK, resp)
}
