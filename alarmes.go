// Alarmes pela ponte: sem ação configurada no Zabbix (webhook), a ponte busca a lista de
// problemas abertos do Zabbix GPON — só a cada -intervalo-alarmes (5 min) ou quando alguém
// pede na página ("Buscar alarmes agora", no máximo uma vez a cada 30 s).
//
// Alarme novo vira chamado (mesma tradução do webhook); chamado GPON vindo do Zabbix que
// não está mais na lista é marcado como normalizado — só quando a ponte confirma que leu a
// lista inteira ("completo"); leitura com problema volta como erro e não normaliza nada.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const intervaloManualAlarmes = 30 * time.Second

type EstadoAlarmes struct {
	Em           int64  `json:"em,omitempty"`      // última busca concluída
	Proximo      int64  `json:"proximo,omitempty"` // próxima busca automática
	Intervalo    int64  `json:"intervalo"`         // ms
	Total        int    `json:"total"`             // problemas na lista
	Novos        int    `json:"novos"`
	Resolvidos   int    `json:"resolvidos"`
	Ignorados    int    `json:"ignorados"`
	Reconhecidos int    `json:"reconhecidos"` // abertos mas já reconhecidos (ack): não viram chamado
	Erro         string `json:"erro,omitempty"`
	Buscando     bool   `json:"buscando,omitempty"`
}

type buscaAlarmes struct {
	mu              sync.Mutex
	estado          EstadoAlarmes
	rodando         bool
	manual          time.Time
	sevMin          int
	comReconhecidos bool // -alarmes-reconhecidos: também cria chamado para alarme já reconhecido
}

// BuscarProblemas pede à ponte a lista de problemas abertos.
func (p *Ponte) BuscarProblemas(sevMin int) ([]any, bool, error) {
	dados, _, err := p.pedir(&pedidoPonte{Tipo: "problemas", SevMin: sevMin})
	if err != nil {
		return nil, false, err
	}
	lista, ok := dados["problemas"].([]any)
	if !ok {
		return nil, false, errors.New("resposta da ponte sem a lista de problemas")
	}
	return lista, dados["completo"] == true, nil
}

// AtivosDoMonitoramento: chamados GPON vindos do Zabbix (id "Z…") ainda não normalizados nem baixados,
// com severidade dentro do mínimo buscado (os abaixo dele não vêm na lista e não podem ser dados como resolvidos).
func (r *Rede) AtivosDoMonitoramento(sevMin int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := []string{}
	for _, id := range r.ordem {
		e := r.eventos[id]
		if e.Severidade != nil && *e.Severidade < sevMin {
			continue
		}
		if strings.HasPrefix(id, "Z") && e.Categoria == "gpon" && e.NormalizadoEm == 0 && e.Status != "encerrado" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Servidor) avisarAlarmes() {
	s.alarmes.mu.Lock()
	b, _ := json.Marshal(s.alarmes.estado)
	s.alarmes.mu.Unlock()
	s.hub.Enviar([]byte("event: alarmes\ndata: " + string(b) + "\n\n"))
}

func (s *Servidor) estadoAlarmesJSON() []byte {
	s.alarmes.mu.Lock()
	defer s.alarmes.mu.Unlock()
	b, _ := json.Marshal(s.alarmes.estado)
	return b
}

// buscarAlarmes busca os problemas abertos e aplica: novos viram chamados, sumidos normalizam.
func (s *Servidor) buscarAlarmes(motivo string, manual bool) error {
	a := s.alarmes
	a.mu.Lock()
	if a.rodando {
		a.mu.Unlock()
		return errors.New("já existe uma busca em andamento")
	}
	if manual {
		if falta := intervaloManualAlarmes - time.Since(a.manual); falta > 0 {
			a.mu.Unlock()
			return errors.New("aguarde " + falta.Round(time.Second).String() + " para buscar de novo")
		}
		a.manual = time.Now()
	}
	a.rodando, a.estado.Buscando = true, true
	a.mu.Unlock()
	s.avisarAlarmes()

	lista, completo, err := s.ponte.BuscarProblemas(a.sevMin)
	agora := time.Now().UnixMilli()
	novos, resolvidos, ignorados, reconhecidos := 0, 0, 0, 0
	if err == nil {
		presentes := map[string]bool{}
		for _, it := range lista {
			m, ok := it.(map[string]any)
			if !ok || !ehTrigger(m) {
				ignorados++
				continue
			}
			if n, ok := inteiro(m["priority"]); ok && n < a.sevMin {
				ignorados++
				continue
			}
			t := deTrigger(m, "Zabbix GPON")
			// já reconhecido no Zabbix (alguém está tratando): não abre chamado novo; o que já existe segue
			if t["reconhecido"] == true && !a.comReconhecidos {
				if id := texto(t["id"]); id != "" {
					if _, existe := s.rede.Obter(id); !existe {
						presentes[id] = true
						reconhecidos++
						continue
					}
				}
			}
			e, novo, motivoErro := s.rede.Receber(t)
			if motivoErro != "" {
				ignorados++
				continue
			}
			presentes[e.ID] = true
			if novo {
				novos++
				log.Printf("rede [GPON]: alarme do monitoramento em %s %s (%s)", e.OLT, e.PON, motivo)
				// sem consulta automática da PON: ela é feita quando alguém abre o chamado
			}
			s.enviarEvento(e)
		}
		// sumiu da lista = resolvido (só com a lista confirmada como completa pela ponte)
		if completo {
			for _, id := range s.rede.AtivosDoMonitoramento(a.sevMin) {
				if presentes[id] {
					continue
				}
				if e, _, motivoErro := s.rede.Receber(map[string]any{"id": id, "normalizadoEm": agora}); motivoErro == "" {
					resolvidos++
					s.enviarEvento(e)
				}
			}
		}
	}

	a.mu.Lock()
	a.rodando = false
	a.estado = EstadoAlarmes{Em: agora, Proximo: a.estado.Proximo, Intervalo: a.estado.Intervalo, Total: len(lista),
		Novos: novos, Resolvidos: resolvidos, Ignorados: ignorados, Reconhecidos: reconhecidos}
	if err != nil {
		a.estado.Erro = err.Error()
	}
	a.mu.Unlock()
	s.avisarAlarmes()
	if err != nil {
		log.Printf("alarmes: busca (%s) falhou: %v", motivo, err)
	} else {
		log.Printf("alarmes: busca (%s): %d abertos, %d novos, %d resolvidos, %d já reconhecidos (fora), %d ignorados", motivo, len(lista), novos, resolvidos, reconhecidos, ignorados)
	}
	return err
}

// vigiarAlarmes: uma busca a cada intervalo (a primeira sai quando a ponte conecta).
func (s *Servidor) vigiarAlarmes(intervalo time.Duration) {
	marcar := func() {
		s.alarmes.mu.Lock()
		s.alarmes.estado.Proximo = time.Now().Add(intervalo).UnixMilli()
		s.alarmes.estado.Intervalo = intervalo.Milliseconds()
		s.alarmes.mu.Unlock()
	}
	marcar()
	for range time.Tick(intervalo) {
		marcar()
		if s.ponte.conectada() {
			s.buscarAlarmes("automática", false)
		}
	}
}

// POST /api/rede/buscar-alarmes: "Buscar alarmes agora" (no máximo uma vez a cada 30 s).
func (s *Servidor) buscarAlarmesAgora(w http.ResponseWriter, r *http.Request) {
	if err := s.buscarAlarmes("pedida na página", true); err != nil {
		status := http.StatusBadGateway
		if strings.HasPrefix(err.Error(), "aguarde") || strings.HasPrefix(err.Error(), "já existe") {
			status = http.StatusTooManyRequests
		}
		responder(w, status, map[string]any{"erro": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(s.estadoAlarmesJSON())
}
