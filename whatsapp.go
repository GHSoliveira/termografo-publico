// Grupo de incidentes do CGR no WhatsApp.
//
// Termógrafo: ao abrir, atualizar e encerrar uma O.S. técnica, o comunicado é enviado
// para o grupo (POST <-whatsapp>/api/enviar). O resultado fica na O.S. e aparece na página.
//
// "termografo.exe whatsapp": um WhatsApp simulado, sem conexão real, com uma conversa só
// (o grupo). Recebe os comunicados do Termógrafo e mostra como chegariam no celular.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- lado do Termógrafo ----------

type EnvioGrupo struct {
	Status string `json:"status"` // "enviado" | "falhou"
	Em     int64  `json:"em"`
	Motivo string `json:"motivo"` // abertura | atualização | encerramento | reenvio
	Erro   string `json:"erro,omitempty"`
}

var nomesTipoIncidente = map[string]string{
	"acesso_baixa": "REDE DE ACESSO - BAIXA", "acesso_media": "REDE DE ACESSO - MÉDIA", "acesso_alta": "REDE DE ACESSO - ALTA",
	"pop_radio": "POP RÁDIO", "backbone_rompimento": "BACKBONE - ROMPIMENTO ÓPTICO", "backbone_degradacao": "BACKBONE - DEGRADAÇÃO / ATENUAÇÃO",
	"backbone_energia": "BACKBONE - ENERGIA POP", "core_equipamento": "CORE - ATIVO DE REDE", "ivr_sinal": "IVR - SINAL ALTO / INTERFERÊNCIA",
	"ivr_temperatura": "IVR - TEMPERATURA ALTA", "ivr_energia": "IVR - SEM ENERGIA NA TORRE",
}

// textoComunicado: o mesmo texto que a página mostra (Termógrafo.html, textoComunicado).
func textoComunicado(o OSTecnica) string {
	data := func(ms int64) string { return time.UnixMilli(ms).Format("15:04 02-01-2006") }
	ou := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "—"
		}
		return strings.TrimSpace(s)
	}
	cab, rotEq, rotPortas := "🚨 COMUNICADO DE INCIDENTE - REDE GPON 🚨", "OLT", "PORTAS AFETADAS"
	switch normalizarCategoria(o.Categoria, o.OLT) {
	case "core":
		cab, rotEq, rotPortas = "🚨 COMUNICADO DE INCIDENTE - REDE CORE / BACKBONE 🚨", "EQUIPAMENTO CORE", "ENLACES AFETADOS"
	case "ivr":
		cab, rotEq, rotPortas = "🚨 COMUNICADO DE INCIDENTE - REDE IVR (TORRE / CONCENTRADOR) 🚨", "CONCENTRADOR / TORRE", "SETORES AFETADOS"
	}
	tipo := nomesTipoIncidente[o.Tipo]
	if tipo == "" {
		tipo = o.Tipo
	}
	previsao := "—"
	if o.Previsao > 0 {
		previsao = data(o.Previsao)
	}
	linhas := []string{
		cab,
		fmt.Sprintf("ID: %d", o.ID),
		"CIDADES AFETADAS: " + ou(strings.Join(o.Cidades, " / ")),
		rotEq + ": " + o.OLT,
		rotPortas + ": " + ou(strings.Join(o.Portas, ", ")),
		"INCIDENTE: " + tipo,
		"DESCRIÇÃO: " + ou(o.Descricao),
		"INÍCIO: " + data(o.Inicio),
		"PREVISÃO: " + previsao,
	}
	if o.Fim > 0 {
		linhas = append(linhas, "FIM: "+data(o.Fim))
	}
	linhas = append(linhas,
		fmt.Sprintf("CLIENTES AFETADOS: %d", o.Clientes),
		"RESPONSÁVEL: "+ou(strings.ToUpper(o.Responsavel)),
		"ORDEM DE SERVIÇO: "+o.Numero,
		"STATUS ATUAL: "+ou(o.Status),
	)
	if o.MensagemFim != "" {
		linhas = append(linhas, "ENCERRAMENTO: "+o.MensagemFim)
	}
	return strings.Join(linhas, "\n")
}

var clienteGrupo = &http.Client{Timeout: 4 * time.Second}

// enviarAoGrupo manda o comunicado e registra o resultado na O.S. (devolve a O.S. atualizada).
func (s *Servidor) enviarAoGrupo(o OSTecnica, motivo string) OSTecnica {
	if s.whatsapp == "" {
		return o
	}
	envio := EnvioGrupo{Motivo: motivo, Em: time.Now().UnixMilli()}
	corpo, _ := json.Marshal(map[string]any{"grupo": s.grupo, "de": "Termógrafo CGR", "texto": textoComunicado(o), "ref": o.Numero})
	req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(s.whatsapp, "/")+"/api/enviar", bytes.NewReader(corpo))
	req.Header.Set("Content-Type", "application/json")
	resp, err := clienteGrupo.Do(req)
	switch {
	case err != nil:
		envio.Status, envio.Erro = "falhou", "sem resposta do WhatsApp"
	case resp.StatusCode/100 != 2:
		envio.Status, envio.Erro = "falhou", fmt.Sprintf("o WhatsApp respondeu %d", resp.StatusCode)
	default:
		envio.Status = "enviado"
	}
	if resp != nil {
		resp.Body.Close()
	}
	if envio.Status == "enviado" {
		log.Printf("whatsapp: comunicado %d (%s) enviado ao grupo %s", o.ID, motivo, s.grupo)
	} else {
		log.Printf("whatsapp: falha ao enviar o comunicado %d (%s): %s", o.ID, motivo, envio.Erro)
	}
	if atual, ok := s.rede.RegistrarEnvio(o.Numero, envio, true); ok {
		return atual
	}
	return o
}

// RegistrarEnvio guarda o último envio ao grupo na O.S. (também usada ao reler o diário).
func (r *Rede) RegistrarEnvio(numero string, envio EnvioGrupo, gravar bool) (OSTecnica, bool) {
	if gravar {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	o, ok := r.oss[numero]
	if !ok {
		return OSTecnica{}, false
	}
	e := envio
	o.Whatsapp = &e
	if gravar {
		dados, _ := json.Marshal(envio)
		r.gravar(linhaDiario{Tipo: "os-wpp", ID: numero, Dados: dados, Em: envio.Em})
	}
	return *o, true
}

// POST /api/rede/os/whatsapp {"numero": "..."}: reenvia o comunicado atual ao grupo.
func (s *Servidor) reenviarGrupo(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Numero string `json:"numero"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		responder(w, http.StatusBadRequest, map[string]any{"erro": "JSON inválido"})
		return
	}
	if s.whatsapp == "" {
		responder(w, http.StatusConflict, map[string]any{"erro": "envio ao grupo não configurado"})
		return
	}
	o, ok := s.rede.ObterOS(p.Numero)
	if !ok {
		responder(w, http.StatusNotFound, map[string]any{"erro": "O.S. não encontrada"})
		return
	}
	o = s.enviarAoGrupo(o, "reenvio")
	s.enviarOS(o)
	responder(w, http.StatusOK, map[string]any{"os": o})
}

// ---------- WhatsApp simulado ----------

type MensagemWpp struct {
	ID     int    `json:"id"`
	De     string `json:"de"` // "Você" = quem está com o celular
	Texto  string `json:"texto"`
	Em     int64  `json:"em"`
	Status string `json:"status"` // entregue | lida (só nas mensagens de "Você")
	Ref    string `json:"ref,omitempty"`
}

type grupoSim struct {
	mu       sync.Mutex
	nome     string
	msgs     []MensagemWpp
	arquivo  *os.File
	hub      *Hub
	pagina   string
	membros  []string
	ultimoID int
}

func (g *grupoSim) adicionar(m MensagemWpp, gravar bool) MensagemWpp {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m.ID == 0 {
		g.ultimoID++
		m.ID = g.ultimoID
	} else if m.ID > g.ultimoID {
		g.ultimoID = m.ID
	}
	g.msgs = append(g.msgs, m)
	if gravar && g.arquivo != nil {
		b, _ := json.Marshal(m)
		g.arquivo.Write(append(b, '\n'))
	}
	return m
}

func (g *grupoSim) avisar(evento string, v any) {
	b, _ := json.Marshal(v)
	g.hub.Enviar([]byte("event: " + evento + "\ndata: " + string(b) + "\n\n"))
}

func whatsappSimulado(args []string) {
	fs := flag.NewFlagSet("whatsapp", flag.ExitOnError)
	porta := fs.Int("porta", 8082, "porta HTTP do WhatsApp simulado")
	nome := fs.String("grupo", "Incidentes CGR", "nome do grupo")
	dados := fs.String("dados", "whatsapp-simulado.jsonl", "arquivo das mensagens")
	pagina := fs.String("pagina", "WhatsApp simulado.html", "arquivo da página")
	fs.Parse(args)

	g := &grupoSim{nome: *nome, pagina: *pagina, hub: &Hub{clientes: map[chan []byte]struct{}{}}, msgs: []MensagemWpp{},
		membros: []string{"Termógrafo CGR", "Supervisor técnico", "Supervisor de redes", "Equipe 1", "Equipe 2"}}
	if f, err := os.Open(*dados); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m MensagemWpp
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				g.adicionar(m, false)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(*dados, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	g.arquivo = f

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		html, err := os.ReadFile(g.pagina)
		if err != nil {
			http.Error(w, "página não encontrada: "+g.pagina, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(html)
	})
	mux.HandleFunc("GET /api/grupo", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		responder(w, http.StatusOK, map[string]any{"nome": g.nome, "membros": g.membros, "mensagens": g.msgs})
	})
	// quem manda: o Termógrafo (de = "Termógrafo CGR") ou a própria página (de = "Você")
	mux.HandleFunc("POST /api/enviar", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Grupo, De, Texto, Ref string
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || strings.TrimSpace(p.Texto) == "" {
			responder(w, http.StatusBadRequest, map[string]any{"erro": `envie {"texto": "..."}`})
			return
		}
		if p.Grupo != "" && p.Grupo != g.nome {
			responder(w, http.StatusNotFound, map[string]any{"erro": "grupo não encontrado: " + p.Grupo})
			return
		}
		if p.De == "" {
			p.De = "Você"
		}
		m := MensagemWpp{De: p.De, Texto: strings.TrimSpace(p.Texto), Em: time.Now().UnixMilli(), Ref: p.Ref}
		if m.De == "Você" {
			m.Status = "entregue"
		}
		m = g.adicionar(m, true)
		g.avisar("mensagem", m)
		log.Printf("grupo %s: mensagem de %s (%d caracteres)", g.nome, m.De, len([]rune(m.Texto)))
		responder(w, http.StatusOK, map[string]any{"id": m.ID, "em": m.Em})
	})
	mux.HandleFunc("GET /eventos", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming não suportado", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		c := g.hub.Entrar()
		defer g.hub.Sair(c)
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
	})

	end := net.JoinHostPort("127.0.0.1", strconv.Itoa(*porta))
	log.Printf("WhatsApp simulado (sem conexão real): http://%s · grupo %q · %d mensagem(ns)", end, g.nome, len(g.msgs))
	log.Printf("o Termógrafo envia para ele com: termografo.exe -whatsapp http://%s", end)
	log.Fatal(http.ListenAndServe(end, mux))
}
