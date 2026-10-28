// Alarmes no formato do Zabbix (trigger): o objeto de um painel que agrega vários Zabbix
// (servers[].triggers[]), ou o que uma ação do Zabbix manda por webhook ao abrir,
// atualizar (reconhecimento) ou resolver um problema. Cada trigger é traduzida para os
// campos do chamado antes de aplicar; o diário guarda a forma traduzida.
package main

import (
	"regexp"
	"strings"
)

// prioridade 0–5 do Zabbix
var nomesSeveridade = []string{"Não classificada", "Informação", "Aviso", "Média", "Alta", "Desastre"}

type TagAlarme struct {
	Nome  string `json:"nome"`
	Valor string `json:"valor,omitempty"`
}

// ehTrigger reconhece o formato do Zabbix pelos campos próprios dele.
func ehTrigger(m map[string]any) bool {
	for _, k := range []string{"lastchange", "eventId", "triggerid", "problem_tags", "trigger_tags", "priority", "acknowledges_history"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// categoria pelo nome do servidor Zabbix (CORE, GPON, Rádio)
func categoriaDoServidor(nome string) string {
	s := semAcento.Replace(strings.ToLower(nome))
	switch {
	case strings.Contains(s, "gpon"):
		return "gpon"
	case strings.Contains(s, "core") || strings.Contains(s, "backbone"):
		return "core"
	case strings.Contains(s, "radio") || strings.Contains(s, "ivr"):
		return "ivr"
	}
	return ""
}

// lerTags aceita [{name, value}] (Zabbix), [{tag, value}] ou [{nome, valor}].
func lerTags(v any) []TagAlarme {
	lista, _ := v.([]any)
	tags := []TagAlarme{}
	for _, it := range lista {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		nome := texto(primeiro(m, "name", "tag", "nome"))
		if nome != "" {
			tags = append(tags, TagAlarme{Nome: nome, Valor: texto(primeiro(m, "value", "valor"))})
		}
	}
	return tags
}

// porta no nome do problema: "PON 2/16", "PON 1/1/16", "gpon-olt_1/2/3", "interface 0/1/4"
var rePorta = regexp.MustCompile(`(?i)\b(?:pon|gpon[-_ ]?(?:olt|onu)?_?|porta|interface)\s*[:#]?\s*(\d+/\d+(?:/\d+)?(?::\d+)?)`)

// deTrigger traduz uma trigger do Zabbix para o formato do webhook de rede.
func deTrigger(m map[string]any, servidor string) map[string]any {
	out := map[string]any{}
	if s := texto(m["server"]); s != "" {
		servidor = s
	}
	if c := categoriaDoServidor(servidor); c != "" {
		out["categoria"] = c
	} else if c := texto(m["categoria"]); c != "" {
		out["categoria"] = c
	}

	// o eventId identifica o problema (é o mesmo na abertura, nos reconhecimentos e na resolução)
	id := texto(primeiro(m, "eventId", "eventid", "event_id"))
	if id == "" {
		if t := texto(primeiro(m, "triggerid", "id")); t != "" {
			id = t + "@" + texto(m["lastchange"])
		}
	}
	if id != "" {
		out["id"] = "Z" + id
	}
	switch h := m["host"].(type) {
	case map[string]any:
		out["host"] = texto(primeiro(h, "host", "name"))
	default:
		out["host"] = texto(h)
	}

	descricao := texto(primeiro(m, "description", "name"))
	if descricao != "" {
		out["alarme"] = descricao
	}
	if n, ok := inteiro(m["priority"]); ok && n >= 0 && n < len(nomesSeveridade) {
		out["severidade"] = n
	}

	tags := append(lerTags(m["problem_tags"]), lerTags(m["trigger_tags"])...)
	tags = append(tags, lerTags(m["tags"])...)
	cidades, lista := []any{}, []any{}
	for _, t := range tags {
		chave := semAcento.Replace(strings.ToLower(t.Nome))
		switch {
		case chave == "pon" || chave == "ponlink" || chave == "porta" || chave == "interface" || chave == "enlace" || chave == "setor":
			if _, tem := out["pon"]; !tem && t.Valor != "" {
				out["pon"] = t.Valor
			}
		case chave == "cidade" && t.Valor != "":
			cidades = append(cidades, t.Valor)
		}
		lista = append(lista, map[string]any{"nome": t.Nome, "valor": t.Valor})
	}
	if _, tem := out["pon"]; !tem {
		if p := rePorta.FindStringSubmatch(descricao); p != nil {
			out["pon"] = p[1]
		}
	}
	if len(cidades) > 0 {
		out["cidade"] = cidades[0]
		if len(cidades) > 1 {
			out["cidadesAfetadas"] = cidades
		}
	}
	out["tags"] = lista

	// resolvido: value 0, status OK ou evento de recuperação; o lastchange passa a ser a hora da volta
	status := semAcento.Replace(strings.ToLower(texto(m["status"])))
	valor := texto(m["value"])
	recuperado := valor == "0" || status == "ok" || status == "resolvido" || status == "resolved" || status == "recuperado"
	if r := texto(m["r_eventid"]); r != "" && r != "0" {
		recuperado = true
	}
	if recuperado {
		out["normalizadoEm"] = primeiro(m, "r_clock", "recovery_clock", "lastchange", "clock")
		out["somenteAtualizar"] = true // resolução de um problema que nunca chegou: ignora
	} else if v := primeiro(m, "lastchange", "clock"); v != nil {
		out["ocorridoEm"] = v
	}

	if v, ok := m["acknowledged"]; ok {
		out["reconhecido"] = texto(v) == "1" || strings.EqualFold(texto(v), "true") || strings.EqualFold(texto(v), "yes")
	}

	// reconhecimentos (ack) com mensagem: viram linhas do histórico
	if hist, ok := m["acknowledges_history"].([]any); ok {
		recs := []any{}
		for _, it := range hist {
			a, ok := it.(map[string]any)
			if !ok {
				continue
			}
			msg := texto(primeiro(a, "message", "mensagem"))
			quem := texto(primeiro(a, "user_full_name", "user", "username", "alias"))
			em := primeiro(a, "clock", "timestamp")
			if em == nil || (msg == "" && quem == "") {
				continue
			}
			recs = append(recs, map[string]any{"em": em, "usuario": quem, "mensagem": msg})
		}
		out["reconhecimentos"] = recs
	}
	return out
}
