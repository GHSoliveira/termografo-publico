// Termógrafo: servidor do mapa de calor de O.S.
//
// Serve a página, recebe O.S. por webhook (POST /webhook/os), descarta IDs
// repetidos, grava em os-recebidas.jsonl e empurra cada O.S. nova para as
// páginas abertas em tempo real (Server-Sent Events em /eventos). Também recebe
// eventos de queda de OLT em POST /webhook/rede (veja rede.go).
//
//	termografo                      inicia o servidor (porta 8080)
//	termografo simular              envia O.S. de teste para o webhook
package main

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ordem é uma O.S. de problema de conexão.
type Ordem struct {
	ID       string `json:"id"`
	Cidade   string `json:"cidade"`
	Problema string `json:"problema"` // "sem_conexao" | "lentidao"
	Cliente  string `json:"cliente,omitempty"`
	CriadaEm int64  `json:"criadaEm"` // milissegundos desde 1970
}

// ---------- armazenamento: memória + arquivo JSON Lines ----------

type Armazem struct {
	mu      sync.RWMutex
	lista   []Ordem
	ids     map[string]struct{}
	arquivo *os.File
}

func abrirArmazem(caminho string) (*Armazem, error) {
	a := &Armazem{ids: map[string]struct{}{}}
	if f, err := os.Open(caminho); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var o Ordem
			if json.Unmarshal(sc.Bytes(), &o) != nil || o.ID == "" {
				continue
			}
			if _, existe := a.ids[o.ID]; !existe {
				a.ids[o.ID] = struct{}{}
				a.lista = append(a.lista, o)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(caminho, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	a.arquivo = f
	return a, nil
}

// Adicionar grava só as O.S. com ID ainda não visto.
func (a *Armazem) Adicionar(itens []Ordem) (novas []Ordem, duplicadas int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var buf bytes.Buffer
	for _, o := range itens {
		if _, existe := a.ids[o.ID]; existe {
			duplicadas++
			continue
		}
		a.ids[o.ID] = struct{}{}
		a.lista = append(a.lista, o)
		novas = append(novas, o)
		linha, _ := json.Marshal(o)
		buf.Write(linha)
		buf.WriteByte('\n')
	}
	if buf.Len() > 0 {
		_, err = a.arquivo.Write(buf.Bytes())
	}
	return
}

func (a *Armazem) JSON() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b, _ := json.Marshal(a.lista)
	return b
}

// MaisAntiga devolve o horário da primeira O.S. recebida (0 se não houver nenhuma).
func (a *Armazem) MaisAntiga() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var menor int64
	for _, o := range a.lista {
		if menor == 0 || o.CriadaEm < menor {
			menor = o.CriadaEm
		}
	}
	return menor
}

// Depois devolve o total e as O.S. recebidas depois das n primeiras.
func (a *Armazem) Depois(n int) (int, []Ordem) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if n > len(a.lista) {
		n = len(a.lista)
	}
	return len(a.lista), append([]Ordem{}, a.lista[n:]...)
}

func (a *Armazem) Total() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.lista)
}

// ---------- tempo real: Server-Sent Events ----------

type Hub struct {
	mu       sync.Mutex
	clientes map[chan []byte]struct{}
}

func (h *Hub) Entrar() chan []byte {
	c := make(chan []byte, 64)
	h.mu.Lock()
	h.clientes[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) Sair(c chan []byte) {
	h.mu.Lock()
	delete(h.clientes, c)
	h.mu.Unlock()
}

// Enviar nunca bloqueia: um cliente lento demais é desconectado; o navegador
// reconecta sozinho e busca de novo tudo o que perdeu.
func (h *Hub) Enviar(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clientes {
		select {
		case c <- msg:
		default:
			delete(h.clientes, c)
			close(c)
		}
	}
}

func (h *Hub) Conectados() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clientes)
}

// ---------- servidor HTTP ----------

type Servidor struct {
	whatsapp string // endereço do WhatsApp (simulado) para o comunicado; "" = desligado
	grupo    string
	armazem  *Armazem
	rede     *Rede
	fonte    Fonte         // consulta por PON (nil = desligada)
	ponte    *Ponte        // ponte com o Zabbix pela sessão do navegador (nil = desligada)
	alarmes  *buscaAlarmes // alarmes abertos buscados pela ponte
	hub      *Hub
	pagina   string
	token    string
	exemplo  bool
	// os dados de exemplo da página vão só até aqui; dali em diante valem as O.S. recebidas,
	// para as duas fontes não se somarem no mesmo horário
	exemploAte int64
}

func (s *Servidor) rotas() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.servirPagina)
	mux.HandleFunc("GET /eventos", s.eventos)
	mux.HandleFunc("GET /api/os", s.listar)
	mux.HandleFunc("POST /webhook/os", s.webhook)
	mux.HandleFunc("POST /webhook/rede", s.webhookRede)
	mux.HandleFunc("GET /api/rede", s.listarRede)
	mux.HandleFunc("POST /api/rede/acao", s.acaoRede)
	mux.HandleFunc("POST /api/rede/consultar", s.consultarAgora)
	mux.HandleFunc("POST /api/rede/config", s.configRede)
	mux.HandleFunc("GET /api/rede/os", s.listarOS)
	mux.HandleFunc("POST /api/rede/os", s.abrirOS)
	mux.HandleFunc("POST /api/rede/os/status", s.statusOS)
	mux.HandleFunc("POST /api/rede/os/encerrar", s.encerrarOS)
	mux.HandleFunc("POST /api/rede/os/whatsapp", s.reenviarGrupo)
	mux.HandleFunc("POST /webhook/rede/encerrar", s.webhookEncerrar)
	if s.ponte != nil {
		mux.HandleFunc("GET /ponte-zabbix.js", s.ponteScript)
		mux.HandleFunc("OPTIONS /ponte-zabbix.js", s.ponteScript)
		mux.HandleFunc("GET /api/ponte/pedidos", s.pontePedidos)
		mux.HandleFunc("OPTIONS /api/ponte/pedidos", s.pontePedidos)
		mux.HandleFunc("POST /api/ponte/resposta", s.ponteResposta)
		mux.HandleFunc("OPTIONS /api/ponte/resposta", s.ponteResposta)
		mux.HandleFunc("POST /api/rede/buscar-alarmes", s.buscarAlarmesAgora)
	}
	return mux
}

// A página recebe a configuração e as O.S. já gravadas antes do script principal.
func (s *Servidor) servirPagina(w http.ResponseWriter, r *http.Request) {
	html, err := os.ReadFile(s.pagina)
	if err != nil {
		http.Error(w, "página não encontrada: "+s.pagina, http.StatusInternalServerError)
		return
	}
	var cfg bytes.Buffer
	cfg.WriteString(`<script>window.TERMOGRAFO_SERVIDOR = {"exemplo":`)
	cfg.WriteString(strconv.FormatBool(s.exemplo))
	cfg.WriteString(`,"exemploAte":`)
	cfg.WriteString(strconv.FormatInt(s.exemploAte, 10))
	cfg.WriteString(`,"os":`)
	cfg.Write(s.armazem.JSON()) // json.Marshal já escapa "<", seguro dentro de <script>
	cfg.WriteString(`,"consulta":`)
	cfg.WriteString(strconv.FormatBool(s.fonte != nil))
	cfg.WriteString(`,"margem":`)
	cfg.WriteString(strconv.FormatFloat(s.rede.Margem(), 'f', -1, 64))
	cfg.WriteString(`,"whatsapp":`)
	cfg.WriteString(strconv.FormatBool(s.whatsapp != ""))
	nomeGrupo, _ := json.Marshal(s.grupo)
	cfg.WriteString(`,"grupo":`)
	cfg.Write(nomeGrupo)
	cfg.WriteString(`,"alarmes":`)
	if s.alarmes != nil {
		cfg.Write(s.estadoAlarmesJSON())
	} else {
		cfg.WriteString("null")
	}
	cfg.WriteString(`,"oss":`)
	cfg.Write(s.rede.OSJSON())
	cfg.WriteString(`,"rede":`)
	cfg.Write(s.rede.JSON())
	cfg.WriteString("};</script>\n<script>")
	html = bytes.Replace(html, []byte("<script>"), cfg.Bytes(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(html)
}

// Sem parâmetro: lista todas as O.S. Com ?depois=N: {"total": T, "os": [...]} só com as que
// chegaram depois das N primeiras (a lista só cresce). É o que a página usa quando o tempo
// real não passa, como nos túneis temporários da Cloudflare, que não entregam SSE.
func (s *Servidor) listar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if v := r.URL.Query().Get("depois"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			responder(w, http.StatusBadRequest, map[string]any{"erro": `"depois" precisa ser um número >= 0`})
			return
		}
		total, novas := s.armazem.Depois(n)
		json.NewEncoder(w).Encode(map[string]any{"total": total, "os": novas})
		return
	}
	w.Write(s.armazem.JSON())
}

func (s *Servidor) eventos(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming não suportado", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // proxies como o nginx não seguram os eventos

	c := s.hub.Entrar()
	defer s.hub.Sair(c)
	// o "ola" confirma para a página que os eventos estão chegando; se não chegar, ela passa a consultar
	fmt.Fprint(w, "retry: 3000\n\nevent: ola\ndata: {}\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, aberto := <-c:
			if !aberto {
				return
			}
			w.Write(msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

type rejeicao struct {
	Indice int    `json:"indice"`
	Motivo string `json:"motivo"`
}

func (s *Servidor) webhook(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && !tokenValido(r, s.token) {
		responder(w, http.StatusUnauthorized, map[string]any{"erro": "token inválido ou ausente"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var corpo any
	if err := dec.Decode(&corpo); err != nil {
		var grande *http.MaxBytesError
		if errors.As(err, &grande) {
			responder(w, http.StatusRequestEntityTooLarge, map[string]any{"erro": "corpo maior que 1 MB"})
		} else {
			responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido: " + err.Error()})
		}
		return
	}

	itens := extrairItens(corpo)
	validas := make([]Ordem, 0, len(itens))
	rejeitadas := []rejeicao{}
	for i, item := range itens {
		o, motivo := validar(item)
		if motivo != "" {
			rejeitadas = append(rejeitadas, rejeicao{i, motivo})
			continue
		}
		validas = append(validas, o)
	}

	novas, duplicadas, err := s.armazem.Adicionar(validas)
	if err != nil {
		log.Printf("erro ao gravar O.S.: %v", err)
		responder(w, http.StatusInternalServerError, map[string]any{"erro": "não foi possível gravar as O.S."})
		return
	}
	if len(novas) > 0 {
		dados, _ := json.Marshal(novas)
		s.hub.Enviar([]byte("event: os\ndata: " + string(dados) + "\n\n"))
	}
	log.Printf("webhook: %d nova(s), %d duplicada(s), %d rejeitada(s) | %d página(s) conectada(s)",
		len(novas), duplicadas, len(rejeitadas), s.hub.Conectados())

	status := http.StatusOK
	if len(validas) == 0 && len(rejeitadas) > 0 {
		status = http.StatusUnprocessableEntity
	}
	responder(w, status, map[string]any{"recebidas": len(novas), "duplicadas": duplicadas, "rejeitadas": rejeitadas})
}

func responder(w http.ResponseWriter, status int, corpo any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(corpo)
}

func tokenValido(r *http.Request, esperado string) bool {
	t := r.Header.Get("X-Webhook-Token")
	if t == "" {
		t = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(esperado)) == 1
}

// ---------- validação: aceita alguns nomes de campo e formatos comuns ----------

// Aceita uma O.S., uma lista de O.S. ou {"os": [...]}.
func extrairItens(corpo any) []any {
	switch v := corpo.(type) {
	case []any:
		return v
	case map[string]any:
		if lista, ok := v["os"].([]any); ok {
			return lista
		}
		return []any{v}
	}
	return []any{corpo}
}

func validar(item any) (Ordem, string) {
	m, ok := item.(map[string]any)
	if !ok {
		return Ordem{}, "item não é um objeto"
	}
	id := texto(primeiro(m, "id", "numero", "protocolo"))
	if id == "" {
		return Ordem{}, `faltou "id"`
	}
	cidade := texto(primeiro(m, "cidade", "city"))
	if cidade == "" {
		return Ordem{}, `faltou "cidade"`
	}
	problema := normalizarProblema(texto(primeiro(m, "problema", "tipo", "motivo")))
	if problema == "" {
		return Ordem{}, `"problema" inválido: use "sem_conexao" ou "lentidao"`
	}
	criadaEm, ok := normalizarData(primeiro(m, "criadaEm", "criada_em", "data", "timestamp"))
	if !ok {
		return Ordem{}, `"criadaEm" não é uma data válida (use ISO 8601, "2006-01-02 15:04:05" ou epoch)`
	}
	cliente := texto(primeiro(m, "cliente", "nome_cliente", "cliente_nome", "nomeCliente")) // opcional
	return Ordem{ID: id, Cidade: cidade, Problema: problema, CriadaEm: criadaEm, Cliente: cliente}, ""
}

func primeiro(m map[string]any, chaves ...string) any {
	for _, k := range chaves {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func texto(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

var semAcento = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a", "é", "e", "ê", "e", "è", "e",
	"í", "i", "î", "i", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ú", "u", "ü", "u", "ç", "c",
)

func normalizarProblema(p string) string {
	s := semAcento.Replace(strings.ToLower(p))
	s = strings.NewReplacer(" ", "_", "-", "_").Replace(s)
	switch {
	case strings.Contains(s, "lent") || strings.Contains(s, "slow"):
		return "lentidao"
	case strings.Contains(s, "sem_con") || strings.Contains(s, "conex") || strings.Contains(s, "offline") ||
		strings.Contains(s, "sem_internet") || strings.Contains(s, "queda"):
		return "sem_conexao"
	}
	return ""
}

var formatosData = []string{
	"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04",
	"02/01/2006 15:04:05", "02/01/2006 15:04",
}

// Sem data, vale o momento do recebimento. Datas sem fuso usam o horário local do servidor.
func normalizarData(v any) (int64, bool) {
	s := texto(v)
	if s == "" {
		return time.Now().UnixMilli(), true
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		if n < 1e12 { // epoch em segundos
			n *= 1000
		}
		return int64(n), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli(), true
	}
	for _, f := range formatosData {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// ---------- simulador: envia O.S. de teste para o webhook ----------
//
// Gera O.S. no ritmo esperado de cada cidade, hora a hora, com o mesmo modelo
// dos dados de exemplo da página: base de clientes, perfil do dia (lentidão
// no pico da noite), fim de semana e meses de chuva. De vez em quando solta
// uma queda massiva numa cidade, que dura de 1 a 4 horas e vai diminuindo.

type cidadeSim struct {
	nome string
	peso float64 // mesmo peso da página (≈ clientes / 500)
}

var cidadesSim = []cidadeSim{
	{"Campinas", 30}, {"Sumaré", 12}, {"Hortolândia", 10}, {"Americana", 10}, {"Indaiatuba", 9},
	{"Santa Bárbara d'Oeste", 8}, {"Valinhos", 6}, {"Paulínia", 5}, {"Nova Odessa", 4}, {"Vinhedo", 4},
	{"Monte Mor", 3}, {"Cosmópolis", 3}, {"Jundiaí", 7.6}, {"Piracicaba", 7.2}, {"Limeira", 6.6},
	{"Rio Claro", 5.2}, {"Itu", 4.8}, {"Mogi Guaçu", 4.4}, {"Araras", 3.8}, {"Salto", 3.6},
	{"Mogi Mirim", 3.4}, {"Itatiba", 3.2}, {"Jaguariúna", 2.6}, {"Amparo", 2.2}, {"Itupeva", 2},
	{"Artur Nogueira", 2}, {"Louveira", 1.8}, {"Capivari", 1.8}, {"Pedreira", 1.6},
	{"Santo Antônio de Posse", 1.2}, {"Holambra", 1}, {"Engenheiro Coelho", 1}, {"Iracemápolis", 0.9},
	{"Elias Fausto", 0.8}, {"Rafard", 0.6},
}

const pesoReferencia = 104 // volume de referência do modelo: 40 O.S. sem conexão e 55 de lentidão por dia a cada 104 de peso

var chuva = [12]float64{1.5, 1.5, 1.4, 1.2, 1, 1, 1, 1, 1, 1.1, 1.2, 1.45}

func sino(x, m, s float64) float64 { return math.Exp(-(x - m) * (x - m) / (2 * s * s)) }
func perfilSC(h float64) float64 {
	return 0.15 + 1.0*sino(h, 8.5, 1.5) + 0.8*sino(h, 13, 3) + 0.9*sino(h, 19.5, 2)
}
func perfilLen(h float64) float64 {
	return 0.1 + 0.4*sino(h, 10, 2.5) + 0.5*sino(h, 14, 2.5) + 1.6*sino(h, 21, 1.8)
}

var somaSC, somaLen = func() (a, b float64) {
	for h := 0.0; h < 24; h++ {
		a += perfilSC(h)
		b += perfilLen(h)
	}
	return
}()

// esperadoPorHora devolve quantas O.S. a cidade costuma ter nessa hora.
func esperadoPorHora(c cidadeSim, t time.Time) (sc, lent float64) {
	h, m := float64(t.Hour()), int(t.Month())-1
	fds := t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
	parte := c.peso / pesoReferencia
	sc = 40 * chuva[m] * perfilSC(h) / somaSC * parte
	lent = 55 * perfilLen(h) / somaLen * parte
	if fds {
		sc *= 1.05
		lent *= 1.25
	}
	if m == 0 || m == 6 {
		lent *= 1.1
	}
	return
}

func poisson(l float64) int {
	if l <= 0 {
		return 0
	}
	if l > 30 {
		return max(0, int(math.Round(l+math.Sqrt(l)*rand.NormFloat64())))
	}
	limite, k, p := math.Exp(-l), 0, 1.0
	for {
		k++
		p *= rand.Float64()
		if p <= limite {
			return k - 1
		}
	}
}

// nomes fictícios para o simulador
var (
	nomesSim = []string{"Ana", "Bruno", "Carla", "Diego", "Eduarda", "Fábio", "Gabriela", "Henrique", "Isabela", "João",
		"Karina", "Lucas", "Mariana", "Nicolas", "Olívia", "Pedro", "Rafaela", "Samuel", "Tatiane", "Vinícius",
		"Beatriz", "Caio", "Débora", "Felipe", "Helena", "Igor", "Juliana", "Leonardo", "Marcos", "Patrícia"}
	sobrenomesSim = []string{"Silva", "Santos", "Oliveira", "Souza", "Rodrigues", "Ferreira", "Alves", "Pereira", "Lima", "Gomes",
		"Costa", "Ribeiro", "Martins", "Carvalho", "Almeida", "Lopes", "Soares", "Fernandes", "Vieira", "Barbosa",
		"Rocha", "Dias", "Nascimento", "Andrade", "Moreira", "Nunes", "Marques", "Machado", "Mendes", "Freitas"}
)

func nomeSim() string {
	return nomesSim[rand.IntN(len(nomesSim))] + " " + sobrenomesSim[rand.IntN(len(sobrenomesSim))] + " " + sobrenomesSim[rand.IntN(len(sobrenomesSim))]
}

type quedaSim struct {
	cidade  int
	inicio  time.Time
	forca   float64 // O.S. por hora na primeira hora
	duracao int     // horas
}

func simular(args []string) {
	fs := flag.NewFlagSet("simular", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:8080/webhook/os", "endereço do webhook")
	urlRede := fs.String("url-rede", "", "endereço do webhook de rede (padrão: o mesmo servidor, /webhook/rede)")
	semRede := fs.Bool("sem-rede", false, "não enviar incidentes de OLT junto com as quedas")
	portaMonitor := fs.Int("porta-monitoramento", 8081, "porta da API de consulta por PON simulada (0 desliga)")
	token := fs.String("token", os.Getenv("WEBHOOK_TOKEN"), "token do webhook, se o servidor exigir")
	intervalo := fs.Duration("intervalo", 10*time.Second, "tempo entre os envios")
	quedasDia := fs.Float64("quedas-por-dia", 8, "média de quedas massivas por dia")
	quedaAgora := fs.Bool("queda-agora", false, "começa com uma queda massiva em andamento")
	tipoQueda := fs.String("tipo-queda", "", `tipo das quedas: "fibra", "energia" ou "sinal" (padrão: sorteado)`)
	alarmesVia := fs.String("alarmes", "webhook", `como os alarmes GPON chegam ao Termógrafo: "webhook" (o simulador manda, como uma ação do Zabbix) ou "ponte" (ficam só no Zabbix simulado; a ponte busca)`)
	cenarios := fs.String("cenarios", "simulador-cenarios.json", "arquivo onde o monitoramento simulado guarda as quedas")
	fs.Parse(args)

	pesoTotal := 0.0
	for _, c := range cidadesSim {
		pesoTotal += c.peso
	}
	sortearCidade := func() int {
		x := rand.Float64() * pesoTotal
		for i, c := range cidadesSim {
			if x -= c.peso; x < 0 {
				return i
			}
		}
		return 0
	}
	if *urlRede == "" {
		*urlRede = strings.Replace(*url, "/webhook/os", "/webhook/rede", 1)
	}
	cliente := &http.Client{Timeout: 10 * time.Second}
	postar := func(destino string, corpo any) (int, error) {
		b, _ := json.Marshal(corpo)
		req, _ := http.NewRequest(http.MethodPost, destino, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-Webhook-Token", *token)
		}
		resp, err := cliente.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	// o simulador também faz o papel do monitoramento: responde as consultas por PON
	monitor := carregarMonitor(*cenarios)
	var muQuedas sync.Mutex // as quedas podem ser criadas pelo laço principal ou pelo comando /simular/queda
	quedas := []quedaSim{}
	// cidade < 0 sorteia; tipo "" sorteia; categoria "" sorteia ("gpon" | "core" | "ivr")
	// duracaoRede > 0 fixa quanto tempo fica fora (testes rápidos de recuperação)
	// olt != "" repete um equipamento (outra porta/setor), para testar o agrupamento na O.S.
	novaQueda := func(t time.Time, cidade int, tipo string, duracaoRede time.Duration, olt string, categoria string) (quedaSim, *redeSim) {
		i := cidade
		if i < 0 {
			i = sortearCidade()
		}
		q := quedaSim{cidade: i, inicio: t, forca: (6 + rand.Float64()*20) * (0.6 + cidadesSim[i].peso/25), duracao: 1 + rand.IntN(4)}
		log.Printf("queda massiva em %s: ~%.0f O.S./hora por %dh", cidadesSim[i].nome, q.forca, q.duracao)
		var r *redeSim
		if !*semRede {
			if olt != "" && categoria == "" {
				categoria = normalizarCategoria("", olt)
			}
			r = novaRedeSim(cidadesSim[i].nome, cidadesSim[i].peso, t, q.duracao, tipo, categoria)
			if duracaoRede > 0 {
				r.fimQueda = r.inicio.Add(duracaoRede)
			}
			if olt != "" {
				r.olt = strings.ToUpper(olt)
			}
			monitor.registrar(r)
			if *alarmesVia == "ponte" && r.categoria == "gpon" {
				log.Printf("rede [GPON]: queda em %s %s (%s, %d de %d clientes): alarme só no Zabbix simulado, a ponte busca", r.olt, r.pon, r.tipo, r.afetados, r.total)
			} else if st, err := postar(*urlRede, r.incidente()); err != nil {
				log.Printf("falha ao enviar incidente de rede: %v", err)
			} else {
				log.Printf("rede [%s]: incidente %s %s (%s, %d de %d clientes) | HTTP %d", strings.ToUpper(r.categoria), r.olt, r.pon, r.tipo, r.afetados, r.total, st)
			}
		}
		return q, r
	}
	if !*semRede && *portaMonitor > 0 {
		mux := http.NewServeMux()
		mux.Handle("GET /api/pon", monitor)
		// Zabbix simulado para a ponte (página, jsrpc.php, history.php, api_jsonrpc.php)
		monitor.registrarZabbix(mux, strings.TrimSuffix(*url, "/webhook/os"))
		// POST /simular/queda?categoria=gpon|core|ivr&tipo=fibra&cidade=Campinas&minutos=10&olt=...: dispara uma queda na hora, para testes
		mux.HandleFunc("POST /simular/queda", func(w http.ResponseWriter, r *http.Request) {
			tipo, nome, categoria := r.URL.Query().Get("tipo"), r.URL.Query().Get("cidade"), r.URL.Query().Get("categoria")
			if tipo != "" && tipo != "fibra" && tipo != "energia" && tipo != "sinal" && tipo != "temperatura" {
				responder(w, http.StatusBadRequest, map[string]any{"erro": `tipo: "fibra", "energia", "sinal" ou "temperatura"`})
				return
			}
			if categoria != "" && categoria != "gpon" && categoria != "core" && categoria != "ivr" {
				responder(w, http.StatusBadRequest, map[string]any{"erro": `categoria: "gpon", "core" ou "ivr"`})
				return
			}
			cidade := -1
			for i, c := range cidadesSim {
				if nome != "" && strings.EqualFold(semAcento.Replace(strings.ToLower(c.nome)), semAcento.Replace(strings.ToLower(nome))) {
					cidade = i
				}
			}
			olt := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("olt")))
			if partes := strings.Split(olt, "-"); olt != "" && nome == "" && len(partes) > 1 { // cidade pelo código do equipamento
				for i, c := range cidadesSim {
					if codigoCidadeSim[c.nome] == partes[1] {
						cidade = i
					}
				}
			}
			if nome != "" && cidade < 0 {
				responder(w, http.StatusBadRequest, map[string]any{"erro": "cidade desconhecida: " + nome})
				return
			}
			minutos, _ := strconv.Atoi(r.URL.Query().Get("minutos"))
			q, rs := novaQueda(time.Now(), cidade, tipo, time.Duration(max(0, minutos))*time.Minute, olt, categoria)
			muQuedas.Lock()
			quedas = append(quedas, q)
			muQuedas.Unlock()
			resp := map[string]any{"cidade": cidadesSim[q.cidade].nome, "duracaoHoras": q.duracao}
			if rs != nil {
				resp["categoria"], resp["olt"], resp["pon"], resp["tipo"], resp["afetados"], resp["clientesNaPon"] = rs.categoria, rs.olt, rs.pon, rs.tipo, rs.afetados, rs.total
			}
			responder(w, http.StatusOK, resp)
		})
		// POST /simular/normalizar?olt=OLT-CMP-DC-ZTE-001&pon=1/1/1&minutos=3: os clientes da PON voltam em alguns minutos
		mux.HandleFunc("POST /simular/normalizar", func(w http.ResponseWriter, r *http.Request) {
			olt, pon := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("olt"))), strings.TrimSpace(r.URL.Query().Get("pon"))
			minutos, err := strconv.Atoi(r.URL.Query().Get("minutos"))
			if err != nil || minutos < 1 {
				minutos = 3
			}
			rs, ok := monitor.normalizar(olt, pon, time.Now().Add(time.Duration(minutos)*time.Minute))
			if !ok {
				responder(w, http.StatusNotFound, map[string]any{"erro": "nenhuma queda ativa nessa OLT/PON"})
				return
			}
			log.Printf("rede: %s %s normalizando até %s", rs.olt, rs.pon, rs.fimQueda.Format("15:04"))
			responder(w, http.StatusOK, map[string]any{"olt": rs.olt, "pon": rs.pon, "normalizadoEm": rs.fimQueda.UnixMilli()})
		})
		end := net.JoinHostPort("127.0.0.1", strconv.Itoa(*portaMonitor))
		go func() { log.Fatal(http.ListenAndServe(end, mux)) }()
		log.Printf("monitoramento simulado: GET http://%s/api/pon?olt=...&pon=...&de=...&ate=... | POST http://%s/simular/queda?categoria=gpon|core|ivr", end, end)
	}

	// como o Zabbix: 3 min depois da queda chega um reconhecimento do plantão e, quando a
	// queda termina, a resolução do problema (value 0)
	if !*semRede {
		go func() {
			reconhecidas, resolvidas := map[string]bool{}, map[string]bool{}
			for range time.Tick(20 * time.Second) {
				agora := time.Now()
				for _, r := range monitor.ativos() {
					if *alarmesVia == "ponte" && r.categoria == "gpon" {
						continue // abertura, reconhecimento e resolução chegam pela ponte
					}
					ack := r.inicio.Add(3 * time.Minute)
					if !reconhecidas[r.id] && agora.After(ack) && agora.Before(r.fimQueda) {
						reconhecidas[r.id] = true
						postar(*urlRede, r.trigger(1, []map[string]any{{
							"acknowledgeid": fmt.Sprint(r.semente % 1_000_000), "message": "Ciente, acionando a equipe de campo",
							"user_full_name": "NOC Plantão", "clock": ack.Unix(), "action_name": "Mensagem",
						}}))
					}
					if !resolvidas[r.id] && !agora.Before(r.fimQueda) && agora.Sub(r.fimQueda) < 30*time.Minute {
						resolvidas[r.id] = true
						if st, err := postar(*urlRede, r.trigger(0, nil)); err == nil {
							log.Printf("rede [%s]: problema resolvido %s %s | HTTP %d", strings.ToUpper(r.categoria), r.olt, r.pon, st)
						}
					}
				}
			}
		}()
	}

	fracao := intervalo.Hours() // pedaço da hora coberto por cada envio
	chanceQueda := *quedasDia / 24 * fracao
	if *quedaAgora {
		q, _ := novaQueda(time.Now(), -1, *tipoQueda, 0, "", "")
		quedas = append(quedas, q)
	}

	seq := time.Now().UnixMilli()
	ticksPorMinuto, tick, noMinuto := max(1, int(time.Minute / *intervalo)), 0, 0
	log.Printf("simulador enviando para %s a cada %s, ~%.1f quedas por dia (Ctrl+C para parar)", *url, *intervalo, *quedasDia)

	for t := range time.Tick(*intervalo) {
		lote := []Ordem{}
		nova := func(cidade int, problema string) {
			seq++
			quando := t.Add(-time.Duration(rand.Int64N(int64(*intervalo)))) // espalha dentro do intervalo
			lote = append(lote, Ordem{ID: "SIM-" + strconv.FormatInt(seq, 36), Cidade: cidadesSim[cidade].nome, Problema: problema, CriadaEm: quando.UnixMilli(), Cliente: nomeSim()})
		}

		esperado := 0.0
		for i, c := range cidadesSim {
			sc, lent := esperadoPorHora(c, t)
			esperado += sc + lent
			for n := poisson(sc * fracao); n > 0; n-- {
				nova(i, "sem_conexao")
			}
			for n := poisson(lent * fracao); n > 0; n-- {
				nova(i, "lentidao")
			}
		}

		var sorteada *quedaSim
		if rand.Float64() < chanceQueda {
			q, _ := novaQueda(t, -1, *tipoQueda, 0, "", "")
			sorteada = &q
		}
		muQuedas.Lock()
		if sorteada != nil {
			quedas = append(quedas, *sorteada)
		}
		ativas := quedas[:0]
		for _, q := range quedas {
			hora := int(t.Sub(q.inicio).Hours())
			if hora >= q.duracao {
				log.Printf("queda em %s terminou", cidadesSim[q.cidade].nome)
				continue
			}
			l := q.forca * math.Pow(0.72, float64(hora)) * fracao
			for n := poisson(l); n > 0; n-- {
				nova(q.cidade, "sem_conexao")
			}
			for n := poisson(l * 0.12); n > 0; n-- {
				nova(q.cidade, "lentidao")
			}
			ativas = append(ativas, q)
		}
		quedas = ativas
		nQuedas := len(quedas)
		muQuedas.Unlock()

		// resumo a cada minuto, para o log não parecer parado no ritmo normal
		tick++
		noMinuto += len(lote)
		if tick%ticksPorMinuto == 0 {
			log.Printf("último minuto: %d O.S. | esperado agora: %.1f O.S./hora somando as cidades | %d queda(s) ativa(s)", noMinuto, esperado, nQuedas)
			noMinuto = 0
		}

		if len(lote) == 0 {
			continue
		}
		corpo, _ := json.Marshal(lote)
		req, _ := http.NewRequest(http.MethodPost, *url, bytes.NewReader(corpo))
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-Webhook-Token", *token)
		}
		resp, err := cliente.Do(req)
		if err != nil {
			log.Printf("falha ao enviar: %v", err)
			continue
		}
		var r struct {
			Recebidas int `json:"recebidas"`
		}
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		log.Printf("HTTP %d | %d O.S. enviada(s), %d nova(s)", resp.StatusCode, len(lote), r.Recebidas)
	}
}

// ---------- início ----------

func main() {
	if len(os.Args) > 1 && os.Args[1] == "whatsapp" {
		whatsappSimulado(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "simular" {
		simular(os.Args[2:])
		return
	}

	porta := flag.Int("porta", 8080, "porta HTTP")
	host := flag.String("host", "127.0.0.1", `endereço de escuta; use 0.0.0.0 para receber webhooks de outras máquinas`)
	token := flag.String("token", os.Getenv("WEBHOOK_TOKEN"), "token exigido no webhook (header X-Webhook-Token ou Authorization: Bearer)")
	semExemplo := flag.Bool("sem-exemplo", false, "mostrar só as O.S. recebidas, sem os dados de exemplo")
	dados := flag.String("dados", "os-recebidas.jsonl", "arquivo onde as O.S. recebidas são gravadas")
	monitoramento := flag.String("monitoramento", "", "endereço da API do monitoramento para consultar a PON dos incidentes (ex.: http://127.0.0.1:8081)")
	dadosRede := flag.String("dados-rede", "eventos-rede.jsonl", "arquivo onde os eventos de rede e as ações sobre eles são gravados")
	pagina := flag.String("pagina", "Termógrafo.html", "arquivo da página")
	whatsapp := flag.String("whatsapp", "", "endereço do WhatsApp que recebe o comunicado (ex.: http://127.0.0.1:8082, o simulado)")
	grupo := flag.String("grupo", "Incidentes CGR", "grupo do WhatsApp que recebe o comunicado")
	ponteZabbix := flag.String("ponte-zabbix", "", "origem(ns) do Zabbix autorizada(s) a rodar a ponte, separadas por vírgula (ex.: https://zabbix.exemplo.com.br); a consulta por PON passa a ser feita pela ponte")
	ponteScript := flag.String("ponte-script", "ponte-zabbix.js", "arquivo do script da ponte")
	intervaloConsultaPonte := flag.Duration("intervalo-consulta", 5*time.Minute, "pela ponte: de quanto em quanto tempo reconsulta as PONs dos chamados em acompanhamento (o resto é ao abrir o chamado)")
	intervaloAlarmes := flag.Duration("intervalo-alarmes", 5*time.Minute, "de quanto em quanto tempo a ponte busca os alarmes abertos do Zabbix (0 desliga; dá para pedir na página)")
	alarmesReconhecidos := flag.Bool("alarmes-reconhecidos", false, "também abrir chamado para alarme já reconhecido (ack) no Zabbix; por padrão só os não reconhecidos")
	sevAlarmes := flag.Int("alarmes-severidade", 5, "severidade mínima dos alarmes buscados pela ponte (5 = Desastre, 4 = Alta, 3 = Média)")
	flag.Parse()

	armazem, err := abrirArmazem(*dados)
	if err != nil {
		log.Fatalf("não foi possível abrir %s: %v", *dados, err)
	}
	rede, err := abrirRede(*dadosRede)
	if err != nil {
		log.Fatalf("não foi possível abrir %s: %v", *dadosRede, err)
	}
	var fonte Fonte
	var ponte *Ponte
	switch {
	case *ponteZabbix != "":
		ponte = novaPonte(*ponteZabbix, *ponteScript)
		fonte = ponte
		if *monitoramento != "" { // Core e IVR continuam pela API de monitoramento
			fonte = fonteMista{gpon: ponte, outras: novaFonte(*monitoramento)}
		}
	case *monitoramento != "":
		fonte = novaFonte(*monitoramento)
	}
	s := &Servidor{armazem: armazem, rede: rede, fonte: fonte, ponte: ponte, hub: &Hub{clientes: map[chan []byte]struct{}{}}, pagina: *pagina, token: *token, exemplo: !*semExemplo, whatsapp: *whatsapp, grupo: *grupo}
	s.exemploAte = time.Now().UnixMilli()
	if primeira := armazem.MaisAntiga(); primeira > 0 && primeira < s.exemploAte {
		s.exemploAte = primeira
	}

	endereco := net.JoinHostPort(*host, strconv.Itoa(*porta))
	log.Printf("Termógrafo em http://%s", endereco)
	log.Printf("Webhook: POST http://%s/webhook/os", endereco)
	log.Printf("Webhook de rede: POST http://%s/webhook/rede", endereco)
	log.Printf("%d O.S. carregadas de %s | dados de exemplo: %v", armazem.Total(), *dados, s.exemplo)
	log.Printf("%d evento(s) de rede carregado(s) de %s", rede.Total(), *dadosRede)
	if s.ponte != nil {
		s.alarmes = &buscaAlarmes{sevMin: *sevAlarmes, comReconhecidos: *alarmesReconhecidos}
		if *intervaloAlarmes > 0 {
			go s.vigiarAlarmes(*intervaloAlarmes)
			log.Printf("alarmes: a ponte busca os problemas abertos a cada %s (severidade >= %d, só não reconhecidos: %v) ou quando pedido na página", *intervaloAlarmes, *sevAlarmes, !*alarmesReconhecidos)
		}
		log.Printf("consulta por PON pela ponte com o Zabbix (origens: %s): ao abrir o chamado, e a cada %s só os em acompanhamento", *ponteZabbix, *intervaloConsultaPonte)
		go s.acompanharConsultas(*intervaloConsultaPonte, true)
	} else if s.fonte != nil {
		log.Printf("consulta por PON em %s (a cada %s enquanto o evento estiver ativo)", *monitoramento, intervaloConsulta)
		go s.acompanharConsultas(intervaloConsulta, false)
	} else {
		log.Printf("consulta por PON desligada: use -monitoramento <url> para buscar contadores e gráficos")
	}
	if *token == "" {
		if *host != "127.0.0.1" && *host != "localhost" {
			log.Printf("ATENÇÃO: webhook aberto na rede sem token. Use -token ou a variável WEBHOOK_TOKEN.")
		}
	} else {
		log.Printf("webhook protegido por token")
	}

	srv := &http.Server{Addr: endereco, Handler: s.rotas(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Fatal(srv.ListenAndServe())
}
