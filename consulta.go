// Consulta por PON: quando chega um incidente de rede com host (OLT) e PON, o Termógrafo
// pergunta ao sistema de monitoramento os contadores (LOS, LOSi, LOFi, sem energia) e a série
// por minuto daquela PON, e repete a consulta a cada minuto enquanto o evento estiver ativo.
//
// Contrato esperado da API de monitoramento (configurada com -monitoramento <url>):
//
//	GET <url>/api/pon?olt=OLT-CMP-DC-ZTE-001&pon=1/2/6&de=<ms>&ate=<ms>
//	{"totalClientes": 412, "normalizadoEm": <ms, opcional>,
//	 "serie": [{"t": <ms>, "online": 398, "los": 3, "losi": 0, "lofi": 1, "semEnergia": 0, "rx": -21.4}, ...]}
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	janelaAntes       = 30 * time.Minute // gráfico começa 30 min antes da queda
	janelaMaxima      = 6 * time.Hour    // nunca pede mais que isso de uma vez (chamado antigo não vira consulta pesada)
	intervaloConsulta = time.Minute
	consultarDepois   = 20 * time.Minute // continua consultando um pouco depois de normalizar ("depois" do gráfico)
)

// Fonte: de onde vêm os contadores e a série da PON (API HTTP do -monitoramento ou a ponte com o Zabbix)
type Fonte interface {
	Consultar(olt, pon string, de, ate int64) (map[string]any, time.Duration, error)
}

type FonteMonitoramento struct {
	base    string
	cliente *http.Client
}

func novaFonte(base string) *FonteMonitoramento {
	if base == "" {
		return nil
	}
	return &FonteMonitoramento{base: strings.TrimRight(base, "/"), cliente: &http.Client{Timeout: 8 * time.Second}}
}

func (f *FonteMonitoramento) Consultar(olt, pon string, de, ate int64) (map[string]any, time.Duration, error) {
	u := fmt.Sprintf("%s/api/pon?olt=%s&pon=%s&de=%d&ate=%d", f.base, url.QueryEscape(olt), url.QueryEscape(pon), de, ate)
	ini := time.Now()
	resp, err := f.cliente.Get(u)
	dur := time.Since(ini)
	if err != nil {
		return nil, dur, fmt.Errorf("monitoramento fora do ar: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, dur, fmt.Errorf("monitoramento respondeu %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, dur, fmt.Errorf("resposta inválida do monitoramento: %v", err)
	}
	return m, time.Since(ini), nil
}

var consultando sync.Map // id -> struct{}: evita duas consultas do mesmo evento ao mesmo tempo

// consultarEvento busca a PON no monitoramento e aplica o resultado como uma atualização do evento.
func (s *Servidor) consultarEvento(id string) (EventoRede, string) {
	if s.fonte == nil {
		return EventoRede{}, "consulta ao monitoramento não configurada (-monitoramento)"
	}
	if _, ocupado := consultando.LoadOrStore(id, struct{}{}); ocupado {
		e, _ := s.rede.Obter(id)
		return e, ""
	}
	defer consultando.Delete(id)

	e, ok := s.rede.Obter(id)
	if !ok {
		return EventoRede{}, "evento não encontrado"
	}
	if e.PON == "" && e.Categoria != "ivr" && e.Categoria != "core" {
		return e, "o evento não informa a PON"
	}
	ate := time.Now().UnixMilli()
	// só os minutos novos: os já recebidos não mudam (a primeira consulta pega desde 30 min antes da queda)
	de := e.OcorridoEm - janelaAntes.Milliseconds()
	if n := len(e.Serie); n > 0 && e.Serie[n-1].T-2*60_000 > de {
		de = e.Serie[n-1].T - 2*60_000
	}
	if limite := ate - janelaMaxima.Milliseconds(); de < limite {
		de = limite
	}
	resp, dur, err := s.fonte.Consultar(e.OLT, e.PON, de, ate)
	upd := map[string]any{"id": id, "ultimaConsulta": ate, "consultaMs": dur.Milliseconds(), "erroConsulta": ""}
	if err != nil {
		upd["erroConsulta"] = err.Error()
	} else {
		for _, k := range []string{"serie", "totalClientes", "normalizadoEm", "cidade", "cidadesAfetadas", "local", "marca", "categoria"} {
			if v, ok := resp[k]; ok && v != nil {
				upd[k] = v
			}
		}
		// contadores do incidente: o pior minuto desde a queda (só substitui se for maior que o já registrado)
		if lista, ok := resp["serie"].([]any); ok {
			pico := e.LOS + e.LOSi + e.LOFi + e.SemEnergia + e.SinalAlto + e.TempAlta
			tempMax := e.Temperatura
			for _, item := range lista {
				pm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if t, lido := dataOpcional(primeiro(pm, "t", "timestamp", "em")); lido <= 0 || t < e.OcorridoEm {
					continue
				}
				los, _ := inteiro(primeiro(pm, "los", "offline"))
				losi, _ := inteiro(primeiro(pm, "losi", "LOSi"))
				lofi, _ := inteiro(primeiro(pm, "lofi", "LOFi"))
				energia, _ := inteiro(primeiro(pm, "semEnergia", "sem_energia", "dying_gasp"))
				sinalAlto, _ := inteiro(primeiro(pm, "sinalAlto", "sinal_alto"))
				tempAlta, _ := inteiro(primeiro(pm, "tempAlta", "temp_alta"))
				if temp, err := strconv.ParseFloat(texto(primeiro(pm, "temperatura", "temp")), 64); err == nil && temp > tempMax {
					tempMax = temp
					upd["temperatura"] = tempMax
				}
				if soma := los + losi + lofi + energia + sinalAlto + tempAlta; soma > pico {
					pico = soma
					upd["los"], upd["losi"], upd["lofi"], upd["semEnergia"], upd["sinalAlto"], upd["tempAlta"] = los, losi, lofi, energia, sinalAlto, tempAlta
				}
			}
		}
	}
	novo, _, motivo := s.rede.Receber(upd)
	if motivo != "" {
		return EventoRede{}, motivo
	}
	if err != nil {
		log.Printf("rede: consulta de %s (%s) falhou: %v", e.OLT, e.PON, err)
	}
	s.enviarEvento(novo)
	return novo, ""
}

// acompanharConsultas consulta de novo, a cada intervalo, os eventos ativos (e os recém-normalizados).
// Pela ponte (Zabbix real) é bem mais contido: só os chamados em acompanhamento/recuperado (a equipe 2
// acompanha a volta dos clientes), um de cada vez; o resto é sob demanda (abrir o chamado, "Consultar agora").
func (s *Servidor) acompanharConsultas(intervalo time.Duration, soAcompanhamento bool) {
	for range time.Tick(intervalo) {
		ids := s.rede.ParaConsultar(time.Now().UnixMilli(), consultarDepois.Milliseconds(), soAcompanhamento)
		if soAcompanhamento {
			for _, id := range ids {
				s.consultarEvento(id) // em fila, um por vez
			}
			continue
		}
		for _, id := range ids {
			go s.consultarEvento(id)
		}
	}
}

// POST /api/rede/consultar {"id": "..."}: "Consultar agora" na página.
func (s *Servidor) consultarAgora(w http.ResponseWriter, r *http.Request) {
	var corpo struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&corpo); err != nil || corpo.ID == "" {
		responder(w, http.StatusBadRequest, map[string]any{"erro": `informe o "id" do evento`})
		return
	}
	if s.fonte == nil {
		responder(w, http.StatusConflict, map[string]any{"erro": "consulta ao monitoramento não configurada"})
		return
	}
	e, motivo := s.consultarEvento(corpo.ID)
	if motivo != "" {
		responder(w, http.StatusNotFound, map[string]any{"erro": motivo})
		return
	}
	responder(w, http.StatusOK, e)
}
