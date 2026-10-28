// O.S. técnica de rede: aberta pela equipe a partir de um ou mais chamados da mesma OLT
// (PONs diferentes são agrupadas na mesma O.S.). Guarda os campos do comunicado de
// incidente; o texto do comunicado é montado na página.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// tipos de incidente da abertura (GPON, Core e IVR)
var tiposIncidente = map[string]bool{
	"pop_radio": true, "acesso_baixa": true, "acesso_media": true, "acesso_alta": true,
	"backbone_rompimento": true, "backbone_degradacao": true, "backbone_energia": true, "core_equipamento": true,
	"ivr_sinal": true, "ivr_temperatura": true, "ivr_energia": true,
}

const primeiroIDComunicado = 10614

type OSTecnica struct {
	Numero      string   `json:"numero"`              // ordem de serviço (formato do IXC)
	ID          int      `json:"id"`                  // ID do comunicado
	Categoria   string   `json:"categoria,omitempty"` // "gpon" | "core" | "ivr"
	OLT         string   `json:"olt"`
	Portas      []string `json:"portas"`
	Cidades     []string `json:"cidades"`
	Tipo        string   `json:"tipo"`
	Descricao   string   `json:"descricao"`
	Inicio      int64    `json:"inicio"`   // alarme mais antigo entre os chamados agrupados
	Previsao    int64    `json:"previsao"` // definida pelo operador
	Clientes    int      `json:"clientes"`
	Responsavel string   `json:"responsavel"`
	Status      string   `json:"status"`
	Eventos     []string `json:"eventos"`
	AbertaEm    int64    `json:"abertaEm"`
	// clientes afetados: Clientes é a leitura mais recente (recalculada a cada atualização)
	ClientesAbertura int   `json:"clientesAbertura"`
	ClientesEm       int64 `json:"clientesEm"`
	// encerramento: pelo supervisor de redes (webhook) ou pelo operador
	EncerradaEm  int64  `json:"encerradaEm,omitempty"` // quando foi registrado
	Fim          int64  `json:"fim,omitempty"`         // quando o incidente acabou
	MensagemFim  string `json:"mensagemFim,omitempty"`
	EncerradaPor string `json:"encerradaPor,omitempty"` // "supervisor" | "operador"
	// último envio do comunicado ao grupo de incidentes do CGR
	Whatsapp *EnvioGrupo `json:"whatsapp,omitempty"`
}

type pedidoOS struct {
	Eventos     []string `json:"eventos"`
	Cidades     []string `json:"cidades"`
	Tipo        string   `json:"tipo"`
	Descricao   string   `json:"descricao"`
	Previsao    int64    `json:"previsao"`
	Responsavel string   `json:"responsavel"`
}

// AbrirOS valida o pedido, cria a O.S. e marca os chamados agrupados.
func (r *Rede) AbrirOS(p pedidoOS) (OSTecnica, []EventoRede, int, string) {
	limpar := func(lista []string) []string {
		vistos, out := map[string]bool{}, []string{}
		for _, v := range lista {
			if v = strings.TrimSpace(v); v != "" && !vistos[v] {
				vistos[v] = true
				out = append(out, v)
			}
		}
		return out
	}
	p.Eventos, p.Cidades = limpar(p.Eventos), limpar(p.Cidades)
	p.Descricao, p.Responsavel = strings.TrimSpace(p.Descricao), strings.ToUpper(strings.TrimSpace(p.Responsavel))
	agora := time.Now().UnixMilli()
	switch {
	case len(p.Eventos) == 0:
		return OSTecnica{}, nil, http.StatusBadRequest, "escolha ao menos um chamado"
	case len(p.Cidades) == 0 || len(p.Cidades) > 20:
		return OSTecnica{}, nil, http.StatusBadRequest, "escolha de 1 a 20 cidades afetadas"
	case !tiposIncidente[p.Tipo]:
		return OSTecnica{}, nil, http.StatusBadRequest, "tipo de incidente inválido"
	case p.Descricao == "" || len([]rune(p.Descricao)) > 500:
		return OSTecnica{}, nil, http.StatusBadRequest, "escreva a descrição (até 500 caracteres)"
	case p.Responsavel == "" || len([]rune(p.Responsavel)) > 80:
		return OSTecnica{}, nil, http.StatusBadRequest, "informe o responsável"
	case p.Previsao < agora-60_000:
		return OSTecnica{}, nil, http.StatusBadRequest, "a previsão precisa ser no futuro"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	o := OSTecnica{Cidades: p.Cidades, Tipo: p.Tipo, Descricao: p.Descricao, Previsao: p.Previsao, Responsavel: p.Responsavel,
		Status: "SUPERVISOR INFORMADO", Eventos: p.Eventos, AbertaEm: agora, ID: primeiroIDComunicado + len(r.ossOrdem),
		Numero: time.Now().Format("20060102150405") + fmt.Sprintf("%06d", rand.IntN(1_000_000))}
	for _, id := range p.Eventos {
		e, ok := r.eventos[id]
		switch {
		case !ok:
			return OSTecnica{}, nil, http.StatusNotFound, "chamado não encontrado: " + id
		case e.OS != "":
			return OSTecnica{}, nil, http.StatusConflict, fmt.Sprintf("o chamado %s já tem a O.S. %s", e.PON, e.OS)
		case e.Status == "encerrado":
			return OSTecnica{}, nil, http.StatusConflict, fmt.Sprintf("o chamado %s já foi baixado", e.PON)
		case o.OLT != "" && e.OLT != o.OLT:
			return OSTecnica{}, nil, http.StatusBadRequest, "só dá para agrupar chamados do mesmo equipamento"
		}
		o.OLT = e.OLT
		o.Categoria = normalizarCategoria(e.Categoria, e.OLT)
		if e.PON != "" {
			o.Portas = append(o.Portas, e.PON)
		}
		o.Clientes += e.LOS + e.LOSi + e.LOFi + e.SemEnergia + e.SinalAlto + e.TempAlta
		if o.Inicio == 0 || e.OcorridoEm < o.Inicio {
			o.Inicio = e.OcorridoEm
		}
	}
	sort.Strings(o.Portas)
	o.ClientesAbertura, o.ClientesEm = o.Clientes, agora
	r.aplicarOS(&o)
	dados, _ := json.Marshal(o)
	r.gravar(linhaDiario{Tipo: "os", Dados: dados, Em: agora})
	eventos := []EventoRede{}
	for _, id := range o.Eventos {
		eventos = append(eventos, copiar(r.eventos[id]))
	}
	return o, eventos, http.StatusOK, ""
}

// aplicarOS registra a O.S. e marca os chamados (também usada ao reler o diário).
func (r *Rede) aplicarOS(o *OSTecnica) {
	if r.oss == nil {
		r.oss = map[string]*OSTecnica{}
	}
	if o.Categoria == "" {
		o.Categoria = normalizarCategoria("", o.OLT)
	}
	if o.ClientesEm == 0 { // O.S. gravadas antes do recálculo
		o.ClientesAbertura, o.ClientesEm = o.Clientes, o.AbertaEm
	}
	if _, existe := r.oss[o.Numero]; !existe {
		r.ossOrdem = append(r.ossOrdem, o.Numero)
	}
	r.oss[o.Numero] = o
	for _, id := range o.Eventos {
		if e, ok := r.eventos[id]; ok {
			e.OS, e.OSAbertaEm, e.Status = o.Numero, o.AbertaEm, "os_aberta"
			e.Historico = append(e.Historico, AcaoRede{Acao: "abrir_os", Em: o.AbertaEm, Detalhe: fmt.Sprintf("%s (comunicado %d)", o.Numero, o.ID)})
		}
	}
}

// ObterOS devolve uma cópia da O.S.
func (r *Rede) ObterOS(numero string) (OSTecnica, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.oss[numero]
	if !ok {
		return OSTecnica{}, false
	}
	return *o, true
}

// StatusOS muda o status e, se clientes != nil, registra a nova contagem de clientes afetados.
func (r *Rede) StatusOS(numero, status string, em int64, gravar bool, clientes *int) (OSTecnica, string) {
	status = strings.ToUpper(strings.TrimSpace(status))
	if status == "" || len([]rune(status)) > 60 {
		return OSTecnica{}, "informe o status (até 60 caracteres)"
	}
	if gravar {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	o, ok := r.oss[numero]
	if !ok {
		return OSTecnica{}, "O.S. não encontrada"
	}
	o.Status = status
	detalhe := status
	if clientes != nil {
		o.Clientes, o.ClientesEm = *clientes, em
		detalhe = fmt.Sprintf("%s · %d clientes afetados", status, *clientes)
	}
	for _, id := range o.Eventos {
		if e, ok := r.eventos[id]; ok {
			e.Historico = append(e.Historico, AcaoRede{Acao: "status_os", Em: em, Detalhe: detalhe})
		}
	}
	if gravar {
		r.gravar(linhaDiario{Tipo: "os-status", ID: numero, Detalhe: status, Em: em, Clientes: clientes})
	}
	return *o, ""
}

// EncerrarOS fecha o incidente: pelo supervisor de redes (webhook) ou pelo operador (mensagem obrigatória).
// Os chamados da O.S. que ainda não tiveram baixa são baixados junto.
func (r *Rede) EncerrarOS(numero string, fim int64, mensagem, por string, em int64, gravar bool) (OSTecnica, []EventoRede, int, string) {
	mensagem = strings.TrimSpace(mensagem)
	if gravar {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	o, ok := r.oss[numero]
	if !ok {
		return OSTecnica{}, nil, http.StatusNotFound, "O.S. não encontrada"
	}
	if o.EncerradaEm != 0 {
		return OSTecnica{}, nil, http.StatusConflict, "incidente já encerrado em " + time.UnixMilli(o.EncerradaEm).Format("02/01 15:04")
	}
	if gravar { // só para ações novas; o diário antigo é relido sem checagem
		switch {
		case por == "operador" && mensagem == "":
			return OSTecnica{}, nil, http.StatusBadRequest, "informe a mensagem de encerramento"
		case len([]rune(mensagem)) > 500:
			return OSTecnica{}, nil, http.StatusBadRequest, "a mensagem de encerramento pode ter até 500 caracteres"
		case fim > em+60_000:
			return OSTecnica{}, nil, http.StatusBadRequest, "o fim do incidente não pode ser no futuro"
		case fim < o.Inicio-60_000:
			return OSTecnica{}, nil, http.StatusBadRequest, "o fim do incidente não pode ser antes do início do alarme"
		}
	}
	o.EncerradaEm, o.Fim, o.MensagemFim, o.EncerradaPor, o.Status = em, fim, mensagem, por, "NORMALIZADO"
	quem := map[string]string{"supervisor": "pelo supervisor de redes", "operador": "pelo operador"}[por]
	detalhe := fmt.Sprintf("Incidente encerrado %s (comunicado %d) · fim às %s", quem, o.ID, time.UnixMilli(fim).Format("15:04 de 02/01"))
	if mensagem != "" {
		detalhe += ": " + mensagem
	}
	eventos := []EventoRede{}
	for _, id := range o.Eventos {
		e, ok := r.eventos[id]
		if !ok {
			continue
		}
		e.Historico = append(e.Historico, AcaoRede{Acao: "encerrar_os", Em: em, Detalhe: detalhe})
		if e.Status != "encerrado" {
			e.Status, e.EncerradoEm = "encerrado", em
		}
		e.Tratando, e.TratandoDesde = "", 0
		eventos = append(eventos, copiar(e))
	}
	if gravar {
		r.gravar(linhaDiario{Tipo: "os-fim", ID: numero, Detalhe: mensagem, Em: em, Fim: fim, Por: por})
	}
	return *o, eventos, http.StatusOK, ""
}

// numeroOS acha a O.S. pelo número (IXC) ou pelo ID do comunicado.
func (r *Rede) numeroOS(numero string, comunicado int) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.oss[numero]; ok && numero != "" {
		return numero, true
	}
	for n, o := range r.oss {
		if comunicado != 0 && o.ID == comunicado {
			return n, true
		}
	}
	return "", false
}

func (s *Servidor) avisarEncerramento(o OSTecnica, eventos []EventoRede) OSTecnica {
	o = s.enviarAoGrupo(o, "encerramento")
	log.Printf("rede: incidente %s (comunicado %d) encerrado pelo %s, fim %s", o.Numero, o.ID, o.EncerradaPor, time.UnixMilli(o.Fim).Format("15:04 02/01"))
	s.enviarOS(o)
	for _, e := range eventos {
		s.enviarEvento(e)
	}
	return o
}

// POST /api/rede/os/encerrar {"numero": "...", "fim": ms, "mensagem": "..."}: o operador encerra o incidente.
func (s *Servidor) encerrarOS(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Numero   string `json:"numero"`
		Fim      int64  `json:"fim"`
		Mensagem string `json:"mensagem"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	agora := time.Now().UnixMilli()
	if p.Fim == 0 {
		p.Fim = agora
	}
	o, eventos, status, motivo := s.rede.EncerrarOS(p.Numero, p.Fim, p.Mensagem, "operador", agora, true)
	if motivo != "" {
		responder(w, status, map[string]any{"erro": motivo})
		return
	}
	o = s.avisarEncerramento(o, eventos)
	responder(w, http.StatusOK, map[string]any{"os": o, "eventos": eventos})
}

// POST /webhook/rede/encerrar: o supervisor de redes encerrou o incidente no sistema dele.
// {"os": "2026...", "fim": "2026-10-15T23:10:00-03:00", "mensagem": "..."}; aceita também "comunicado" (ID).
// Sem "fim", vale a hora do recebimento.
func (s *Servidor) webhookEncerrar(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && !tokenValido(r, s.token) {
		responder(w, http.StatusUnauthorized, map[string]any{"erro": "token inválido ou ausente"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido: envie um objeto"})
		return
	}
	comunicado, _ := inteiro(primeiro(m, "comunicado", "idComunicado", "id_comunicado"))
	numero, ok := s.rede.numeroOS(texto(primeiro(m, "os", "numero", "ordemServico", "ordem_servico", "id_os")), comunicado)
	if !ok {
		responder(w, http.StatusNotFound, map[string]any{"erro": `O.S. não encontrada: informe "os" (número da O.S.) ou "comunicado" (ID)`})
		return
	}
	agora := time.Now().UnixMilli()
	fim, lido := dataOpcional(primeiro(m, "fim", "encerradoEm", "encerrado_em", "normalizadoEm"))
	switch lido {
	case -1:
		responder(w, http.StatusBadRequest, map[string]any{"erro": `"fim" inválido: use ISO 8601 ou epoch`})
		return
	case 0:
		fim = agora
	}
	o, eventos, status, motivo := s.rede.EncerrarOS(numero, fim, texto(primeiro(m, "mensagem", "descricao", "observacao")), "supervisor", agora, true)
	if motivo != "" {
		responder(w, status, map[string]any{"erro": motivo})
		return
	}
	o = s.avisarEncerramento(o, eventos)
	responder(w, http.StatusOK, map[string]any{"os": o.Numero, "comunicado": o.ID, "encerrada": true})
}

func (r *Rede) OSJSON() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lista := make([]*OSTecnica, 0, len(r.ossOrdem))
	for _, n := range r.ossOrdem {
		lista = append(lista, r.oss[n])
	}
	b, _ := json.Marshal(lista)
	return b
}

// ---------- rotas ----------

func (s *Servidor) enviarOS(o OSTecnica) {
	dados, _ := json.Marshal(o)
	s.hub.Enviar([]byte("event: os-tecnica\ndata: " + string(dados) + "\n\n"))
}

// POST /api/rede/os: abre a O.S. técnica a partir de um ou mais chamados da mesma OLT.
func (s *Servidor) abrirOS(w http.ResponseWriter, r *http.Request) {
	var p pedidoOS
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	o, eventos, status, motivo := s.rede.AbrirOS(p)
	if motivo != "" {
		responder(w, status, map[string]any{"erro": motivo})
		return
	}
	o = s.enviarAoGrupo(o, "abertura")
	log.Printf("rede: O.S. %s (comunicado %d) aberta para %s portas %s", o.Numero, o.ID, o.OLT, strings.Join(o.Portas, ", "))
	s.enviarOS(o)
	for _, e := range eventos {
		s.enviarEvento(e)
	}
	responder(w, http.StatusOK, map[string]any{"os": o, "eventos": eventos})
}

// afetadosAgora: alarmes no último minuto consultado (ou os do pior minuto, sem série).
func afetadosAgora(e EventoRede) int {
	if n := len(e.Serie); n > 0 && e.Serie[n-1].T >= e.OcorridoEm {
		p := e.Serie[n-1]
		return p.LOS + p.LOSi + p.LOFi + p.SemEnergia + p.SinalAlto + p.TempAlta
	}
	return e.LOS + e.LOSi + e.LOFi + e.SemEnergia + e.SinalAlto + e.TempAlta
}

// POST /api/rede/os/status {"numero": "...", "status": "..."}: antes de atualizar, consulta de novo
// cada PON da O.S. no monitoramento e recalcula quantos clientes estão afetados agora.
func (s *Servidor) statusOS(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Numero string `json:"numero"`
		Status string `json:"status"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	atual, ok := s.rede.ObterOS(p.Numero)
	if !ok {
		responder(w, http.StatusNotFound, map[string]any{"erro": "O.S. não encontrada"})
		return
	}
	aviso := ""
	if s.fonte != nil {
		var wg sync.WaitGroup
		for _, id := range atual.Eventos {
			wg.Add(1)
			go func(id string) { defer wg.Done(); s.consultarEvento(id) }(id)
		}
		wg.Wait()
	} else {
		aviso = "consulta ao monitoramento desligada: usada a última leitura de cada PON"
	}
	clientes, falhas := 0, 0
	for _, id := range atual.Eventos {
		if e, ok := s.rede.Obter(id); ok {
			clientes += afetadosAgora(e)
			if e.ErroConsulta != "" {
				falhas++
			}
		}
	}
	if falhas > 0 {
		aviso = fmt.Sprintf("%d PON(s) sem resposta do monitoramento: usada a última leitura", falhas)
	}
	o, motivo := s.rede.StatusOS(p.Numero, p.Status, time.Now().UnixMilli(), true, &clientes)
	if motivo != "" {
		responder(w, http.StatusBadRequest, map[string]any{"erro": motivo})
		return
	}
	log.Printf("rede: O.S. %s atualizada (%s, %d clientes afetados)", o.Numero, o.Status, o.Clientes)
	o = s.enviarAoGrupo(o, "atualização")
	s.enviarOS(o)
	for _, id := range o.Eventos {
		if e, ok := s.rede.Obter(id); ok {
			s.enviarEvento(e)
		}
	}
	responder(w, http.StatusOK, map[string]any{"os": o, "aviso": aviso})
}

func (s *Servidor) listarOS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.rede.OSJSON())
}
