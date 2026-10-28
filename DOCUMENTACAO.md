# Termógrafo — documentação

Ferramenta do CGR para acompanhar, por cidade, as O.S. de clientes com problema de conexão (sem conexão e lentidão) num mapa de calor, e para tratar os incidentes de rede (quedas de OLT/PON) que chegam do monitoramento, da triagem até o encerramento.

Estado em 15/10/2026. Tudo roda num único executável (`termografo.exe`, Go) que serve a página (`Termógrafo.html`) e recebe os webhooks.

---

## 1. Como rodar

```
# servidor (lê o token do arquivo e consulta as PONs no monitoramento)
$env:WEBHOOK_TOKEN = (Get-Content token-webhook.txt -Raw).Trim()
.\termografo.exe -monitoramento http://127.0.0.1:8081

# simulador (O.S. de clientes + incidentes de rede + monitoramento falso na porta 8081)
.\termografo.exe simular

# WhatsApp simulado (porta 8082): o grupo que recebe os comunicados
.\termografo.exe whatsapp
```

Para o Termógrafo enviar ao grupo, acrescente `-whatsapp http://127.0.0.1:8082` ao comando do servidor.

Página: http://localhost:8080 · WhatsApp simulado: http://localhost:8082

### Opções do servidor

| Opção | Padrão | Para quê |
|---|---|---|
| `-porta` | 8080 | porta HTTP |
| `-host` | 127.0.0.1 | use `0.0.0.0` para receber webhooks de outras máquinas |
| `-token` | variável `WEBHOOK_TOKEN` | token exigido nos webhooks |
| `-monitoramento` | (desligado) | endereço da API de consulta por PON |
| `-dados` | os-recebidas.jsonl | diário das O.S. de clientes |
| `-dados-rede` | eventos-rede.jsonl | diário dos chamados de rede, ações e O.S. técnicas |
| `-pagina` | Termógrafo.html | arquivo da página (é relido a cada acesso: editar o HTML não exige reiniciar) |
| `-sem-exemplo` | — | mostra só O.S. recebidas, sem os dados de exemplo |
| `-ponte-zabbix` | (desligado) | origem(ns) do Zabbix autorizada(s) a rodar a ponte, separadas por vírgula (ex.: `https://zabbix.exemplo.com.br`). Liga a consulta por PON pela ponte (seção 3.5). Com `-monitoramento` junto, GPON vai pela ponte e Core/IVR pela API de monitoramento |
| `-ponte-script` | ponte-zabbix.js | arquivo do script da ponte |
| `-intervalo-alarmes` | 5m | de quanto em quanto tempo a ponte busca os alarmes abertos (0 desliga; dá para pedir na página) |
| `-alarmes-severidade` | 5 | severidade mínima dos alarmes buscados pela ponte (5 = Desastre, 4 = Alta, 3 = Média) |
| `-alarmes-reconhecidos` | desligado | também abrir chamado para alarme já reconhecido (ack) no Zabbix; por padrão só os **não reconhecidos** |
| `-whatsapp` | (desligado) | endereço do WhatsApp que recebe o comunicado (o simulado: http://127.0.0.1:8082) |
| `-grupo` | Incidentes CGR | grupo que recebe o comunicado |

### WhatsApp simulado (`termografo.exe whatsapp`)

Um WhatsApp de mentira, sem conexão real, com uma conversa só: o grupo de incidentes do CGR. Recebe os comunicados do Termógrafo ao vivo e mostra como chegariam no celular. Dá para digitar no grupo como membro ("Você", com formatação do WhatsApp: `*negrito*`, `_itálico_`). As mensagens ficam em `whatsapp-simulado.jsonl`.

| Opção | Padrão | Para quê |
|---|---|---|
| `-porta` | 8082 | porta HTTP |
| `-grupo` | Incidentes CGR | nome do grupo (o Termógrafo precisa mandar para o mesmo nome) |
| `-dados` | whatsapp-simulado.jsonl | arquivo das mensagens |
| `-pagina` | WhatsApp simulado.html | página |

Contrato (o mesmo que um gateway real de WhatsApp precisaria aceitar): `POST /api/enviar {"grupo", "de", "texto", "ref"}` → `{"id", "em"}`.

### Opções do simulador (`termografo.exe simular`)

| Opção | Padrão | Para quê |
|---|---|---|
| `-url` | http://127.0.0.1:8080/webhook/os | para onde mandar as O.S. |
| `-url-rede` | mesmo servidor, /webhook/rede | para onde mandar os incidentes |
| `-sem-rede` | — | não gera incidentes de OLT |
| `-porta-monitoramento` | 8081 | API de consulta por PON simulada (0 desliga) |
| `-intervalo` | 10s | tempo entre envios |
| `-quedas-por-dia` | 8 | média de quedas massivas por dia |
| `-queda-agora` | — | começa com uma queda em andamento |
| `-tipo-queda` | sorteado | `fibra`, `energia` ou `sinal` |
| `-cenarios` | simulador-cenarios.json | onde o monitoramento simulado guarda as quedas (sobrevive a reinício) |

O simulador gera O.S. no ritmo esperado de cada cidade, com quedas massivas de vez em quando, e para cada queda manda o alarme no formato do Zabbix (servidor GPON, CORE ou Rádio): a abertura, um reconhecimento do plantão 3 minutos depois e a resolução quando a queda termina. Também responde às consultas por PON como se fosse o monitoramento.

### Comandos de teste do simulador

```
# dispara uma queda agora (categoria: gpon, core ou ivr; tipo: fibra, energia, sinal ou temperatura)
curl -X POST "http://127.0.0.1:8081/simular/queda?categoria=gpon&tipo=fibra&cidade=Campinas&minutos=10"
curl -X POST "http://127.0.0.1:8081/simular/queda?categoria=core&tipo=fibra&minutos=15"
curl -X POST "http://127.0.0.1:8081/simular/queda?categoria=ivr&tipo=temperatura&minutos=10"

# faz os clientes de uma PON/enlace/setor voltarem em alguns minutos (padrão 3)
curl -X POST "http://127.0.0.1:8081/simular/normalizar?olt=OLT-CMP-POP-ZTE-003&pon=1/1/16&minutos=3"
```

### Acesso externo para teste

Túnel temporário da Cloudflare apontando para a porta 8080, sempre com um arquivo de configuração vazio para não usar o túnel nomeado da máquina:

```
cloudflared tunnel --config <arquivo-vazio.yml> --no-autoupdate --protocol http2 --url http://127.0.0.1:8080
```

O túnel temporário não entrega tempo real (SSE); a página percebe e passa a buscar novidades a cada 5 s.

---

## 2. A página

**Entrada**: ao abrir, a página mostra uma tela preta com um globo girando e linhas de conexão (a mesma animação do login do Onion, em tons de cinza, com o Brasil de frente e as cidades atendidas marcadas) e um cartão fosco pedindo o **código de entrada**. Por enquanto qualquer código serve; vale até fechar a aba (sessionStorage). O globo usa a biblioteca `cobe` (MIT) embutida na página, então funciona sem internet.

**Perfil** (botão redondo no canto superior direito): mostra a hora de entrada e permite mudar o **tema** (Sistema · Claro · Escuro) e **Sair**, que volta para a tela de entrada.

Menu principal: **Mapa · Monitor · Rede · Incidentes**. A aba, os filtros e a visão escolhidos ficam salvos no navegador. Visual monocromático (cinzas e branco); cor só no mapa de calor.

### 2.1 Mapa

- Grade **cidade × tempo** centralizada, com submenu de período: **Mensal, Semanal, Diário, Hora a hora**.
- Filtro de problema na própria aba: **Todas, Sem conexão, Lentidão**.
- Cidades agrupadas por **filial** (101 a 105, por proximidade) e ordenadas pelo volume de O.S.; cidades novas vindas do webhook entram em "Sem filial".
- **Cor de cada célula** = quanto o valor passou do esperado (média da cidade naquela visão), em escala contínua verde → amarelo → vermelho → vermelho escuro. Para subir de degrau a célula precisa passar nas duas regras:
  - % acima do esperado — hora: 50/100/200%, dia: 25/50/100%, semana: 15/30/50%, mês: 30/60/100%;
  - excesso acima do ruído normal: 2, 3 e 4 desvios (√esperado), para 1 ou 2 O.S. a mais numa cidade pequena não virar alarme.
- **Clique na célula** abre uma janela de detalhes arrastável (não modal), sempre perto da célula: nível, O.S. × esperado e desvio, sem conexão × lentidão, distribuição no período e lista das O.S. com o nome do cliente.
- Na janela: **Gerar incidente** (para a cidade ou para a **filial inteira**). O incidente aparece na aba Incidentes.
- Avisos (canto superior direito) de **variação anormal**: cidade com risco ≥ 50% (Alto) ou ≥ 75% (Crítico) e pelo menos 5 O.S. na última hora; com 3+ cidades da mesma filial em alerta, vira um aviso só da filial. Um aviso por cidade por hora (avisa de novo só se piorar).

### 2.2 Monitor

- **Risco por filial**: soma das cidades da filial na última hora contra o esperado, com botão Gerar incidente.
- **Maior risco de problema massivo agora**: cidades ordenadas pelo risco (última hora contra a média por hora das últimas 24 h, com tendência subindo/estável/caindo).
- **Últimas 24 horas**: gráficos das cidades em destaque, barras por hora com linha curva pelo total e o esperado tracejado.
- Filtros de problema (Todas, Sem conexão, Lentidão) e de filial.

### 2.3 Rede (chamados de GPON, Core e IVR)

Cada incidente recebido do monitoramento vira um **chamado** classificado em uma das 3 categorias na fila de **Triagem** (`Todas · GPON · Core · IVR`):
- **GPON** (fibra óptica de acesso): equipamento OLT (`OLT-CMP-DC-ZTE-001`), porta `PON`, contadores `LOS`, `LOSi`, `LOFi`, `Sem energia` e gráfico de sinal óptico (`rx` em dBm).
- **Core** (backbone e transporte): equipamento de transporte (`CORE-...`, `BKB-...`), `Enlace` tronco, múltiplas cidades/POPs afetados em maior escala, contadores `Link Down / LOS`, `Atenuação`, `Erros / Flapping`, `Sem energia POP` e sinal óptico do enlace.
- **IVR** (concentrador e torre de rádio): sem OLT e sem sinal de fibra; identifica `TORRE-...` ou `CONC-...` e o `Setor` / concentrador, com monitoramento voltado a `Sinal alto`, `Temperatura alta` (com leitura e gráfico em `°C`), `Sem energia` e `Sem resposta`.

**Consulta ao monitoramento**: ao chegar um chamado, o servidor consulta o monitoramento na hora e depois a cada minuto enquanto ele estiver ativo, em acompanhamento ou recuperado. Os contadores guardam o pior minuto desde a queda. A tela mostra quando foi a última consulta, quanto demorou e permite **Consultar agora**.

**Alarme do monitoramento**: cada chamado mostra a **severidade** (Não classificada → Desastre, só em intensidade de cinza), o nome do alarme e as etiquetas (PON, categoria, cidade). Os reconhecimentos feitos no monitoramento entram no histórico.

**Detalhe do chamado**: afetados e % da base, contadores por alarme da categoria, provável causa com sugestão, gráficos de clientes online, alarmes por tipo e sinal óptico (GPON/Core) ou temperatura do concentrador (IVR), clientes das cidades afetadas que abriram O.S. desde a queda e histórico de tudo que foi feito.

**Filas (submenu)**: Triagem (dividida em **GPON · Core · IVR**) · Acompanhamento · Baixados · Todos.

| Status | Quem | O que acontece |
|---|---|---|
| **Novo** (Triagem) | equipe 1 | analisa os gráficos por categoria (GPON, Core ou IVR); se é falta de energia, manda para acompanhamento; se exige atuação técnica, abre O.S. técnica |
| **Acompanhamento** | equipe 2 | acompanha a volta dos clientes; mostra ON agora × margem |
| **Recuperado** | equipe 2 | automático quando o % ON passa da margem (75%); a equipe 2 confere e dá baixa |
| **O.S. aberta** | — | chamado ligado a uma O.S. técnica |
| **Encerrado** (Baixados) | — | baixa dada ou incidente encerrado |

Ações:
- **Mandar para acompanhamento** — exige mensagem (até 500 caracteres); a equipe 2 vê a mensagem no painel do chamado.
- **Marcar como recuperado / Voltar para acompanhamento** — correção manual; pausa a mudança automática (**Retomar automático** desfaz).
- **Voltar para triagem**, **Reabrir**.
- **Dar baixa** — exige mensagem (até 500 caracteres).
- **Abrir O.S. técnica** — abre a janela de abertura adaptada para GPON, Core ou IVR.

### 2.4 Abertura de O.S. técnica (janela dedicada)

- Agrupa **vários chamados do mesmo equipamento** (PONs, enlaces ou setores diferentes) numa O.S.
- Adapta rótulos, tipos de incidente, prazos e cabeçalho do comunicado para **GPON**, **Core / Backbone** ou **IVR (Torre / Concentrador)**.
- Gera o número da O.S. (formato IXC, simulado) e o ID do comunicado (a partir de 10614). Status inicial: SUPERVISOR INFORMADO.
- Ao concluir: confirmação com o número da O.S. e o comunicado (copiar é opcional).

### 2.5 Incidentes

Lista única com os incidentes gerados pelo mapa/Monitor e as O.S. técnicas, abertos primeiro. Filtros: Abertos · Resolvidos · Todos.

**Cartão da O.S. técnica**:
- Resumo: OLT, tipo, portas, cidades, clientes afetados agora (e na abertura), previsão e se está atrasada.
- Detalhes: campos do comunicado, chamados vinculados com % ON agora, histórico e o comunicado.
- **Atualizar**: escolhe o novo status e **recalcula os clientes afetados** — o servidor consulta de novo cada PON da O.S. e soma quem está com alarme agora. O número vai para o comunicado e o histórico. Se uma PON não responder, usa a última leitura e avisa.
- **Encerrar**: o operador informa **data/hora do fim** (não pode ser no futuro nem antes do alarme) e **mensagem** (obrigatória). Status vira NORMALIZADO, os chamados da O.S. recebem baixa junto e o comunicado ganha FIM e ENCERRAMENTO. Normalmente quem encerra é o **supervisor de redes**, e o aviso chega pelo webhook (seção 3).
- **Comunicado**: o servidor envia sozinho para o grupo de incidentes do CGR ao **abrir**, **atualizar** e **encerrar** a O.S. (com `-whatsapp`). O cartão mostra discretamente "Enviado ao grupo Incidentes CGR às hh:mm" ou, se falhar, "Falha ao enviar ao grupo" com **Reenviar**. Sem `-whatsapp`, aparece "envio automático não configurado". Copiar serve só para encaminhar ao supervisor técnico.

Modelo do comunicado:

```
🚨 COMUNICADO DE INCIDENTE - REDE GPON 🚨
ID: 10617
CIDADES AFETADAS: Salto
OLT: OLT-SLT-POP-ZTE-003
PORTAS AFETADAS: 1/1/11
INCIDENTE: REDE DE ACESSO - MÉDIA
DESCRIÇÃO: ...
INÍCIO: 21:19 15-10-2026
PREVISÃO: 05:20 16-10-2026
FIM: ...                      (só depois de encerrado)
CLIENTES AFETADOS: 52
RESPONSÁVEL: FULANO DE TAL
ORDEM DE SERVIÇO: 20261015212114632962
STATUS ATUAL: SUPERVISOR INFORMADO
ENCERRAMENTO: ...             (só depois de encerrado)
```

**Cartão de incidente do mapa**: cidade ou filial, período, O.S. × esperado, **Resolvido** e **Ver no mapa**. Atenção: esses incidentes ficam salvos só no navegador de quem gerou (ainda não vão para o servidor).

---

## 3. Integrações (webhooks)

Todos os webhooks exigem o token (header `X-Webhook-Token: <token>` ou `Authorization: Bearer <token>`). O token fica em `token-webhook.txt` — não compartilhar.

### 3.1 O.S. de clientes — `POST /webhook/os`

Uma O.S., uma lista ou `{"os": [...]}`. Duplicadas (mesmo id) são ignoradas, então dá para reenviar a janela dos últimos minutos.

```json
{ "id": "123456", "cidade": "Campinas", "problema": "sem conexão", "cliente": "Fulano de Tal", "criadaEm": "2026-10-15T21:10:00-03:00" }
```

Aceita também `numero`/`protocolo`, `tipo`/`motivo`, `data`/`timestamp`. Problema: textos com "lent" viram lentidão; "sem conexão", "offline", "queda" etc. viram sem conexão.

### 3.2 Incidente de rede — `POST /webhook/rede`

Um evento, uma lista ou `{"eventos": [...]}`. O mínimo é OLT e PON; o resto vem da consulta.

```json
{ "id": "EVT-1", "host": "OLT-CMP-DC-ZTE-001", "pon": "1/1/16", "ocorridoEm": "2026-10-15T22:40:00-03:00" }
```

Opcionais: `cidade`, `local`, `marca`, `los`, `losi`, `lofi`, `semEnergia` (ou `dying_gasp`), `totalClientes`, `normalizadoEm`, `serie`. Reenviar o mesmo `id` atualiza o chamado.

**Formato do Zabbix (trigger).** O mesmo endereço aceita os alarmes como o Zabbix manda, sem conversão:
- uma trigger solta ou uma lista, como uma ação do Zabbix mandaria ao abrir, reconhecer (ack) ou resolver o problema;
- a resposta de um painel que agrega vários Zabbix (`servers[].triggers[]`).

```json
{ "server": "Zabbix GPON", "eventId": "65952897", "host": "OLT-CMP-POP-HUA-002",
  "description": "Queda de ONUs na PON 1/2/15 (LOS)", "priority": 5, "lastchange": 1792709340, "value": 1,
  "problem_tags": [ { "name": "PON", "value": "1/2/15" }, { "name": "Cidade", "value": "Campinas" } ],
  "acknowledges_history": [ { "message": "Ciente, acionando a equipe de campo", "user_full_name": "NOC Plantão", "clock": 1792709520 } ] }
```

Como cada campo é lido:

| Zabbix | Chamado |
|---|---|
| `eventId` | identificador (o mesmo na abertura, nos acks e na resolução) |
| `server` (GPON / CORE / Rádio) | categoria GPON / Core / IVR |
| `host` | OLT / equipamento |
| `description` | nome do alarme |
| `priority` 0–5 | severidade (Não classificada … Desastre) |
| `lastchange` | início da queda; na resolução, hora da normalização |
| tag `PON` / `Enlace` / `Setor` / `Porta` / `Interface` | porta (sem a tag, procura `PON 1/2/3` ou `gpon-olt_1/2/3` no nome do alarme) |
| tag `Cidade` (uma ou várias) | cidade / cidades afetadas |
| `value: 0`, `status: "OK"` ou `r_eventid` | problema resolvido (resolução sem chamado aberto é ignorada) |
| `acknowledges_history` | histórico do chamado ("Reconhecido no monitoramento · quem: mensagem"), sem repetir |

`?severidadeMinima=N` descarta triggers abaixo da severidade N (ex.: `3` = só Média para cima), útil ao mandar o painel inteiro. A resposta informa `novos`, `atualizados`, `ignorados` e `rejeitados`.

Caminho recomendado: uma **ação no Zabbix** com um tipo de mídia webhook apontando para este endereço, com operações de problema, atualização e recuperação, e as condições da ação filtrando só os alarmes que viram chamado.

### 3.3 Encerramento pelo supervisor — `POST /webhook/rede/encerrar`

```json
{ "os": "20261015212114632962", "fim": "2026-10-15T23:10:00-03:00", "mensagem": "Fibra emendada" }
```

Identifica pela O.S. (`os`) ou pelo ID do comunicado (`comunicado`). `fim` e `mensagem` são opcionais (sem `fim`, vale a hora do recebimento). Incidente já encerrado é recusado.

### 3.4 Consulta ao monitoramento (o que o Termógrafo chama)

```
GET <monitoramento>/api/pon?olt=OLT-CMP-DC-ZTE-001&pon=1/1/16&de=<epoch ms>&ate=<epoch ms>
```

Resposta esperada:

```json
{ "totalClientes": 440, "normalizadoEm": 1792115190987,
  "serie": [ { "t": 1792114800000, "online": 108, "los": 51, "losi": 16, "lofi": 13, "semEnergia": 255, "rx": -21.3 } ] }
```

Um ponto por minuto. Minutos já recebidos não são sobrescritos. Depois da primeira consulta (que pega desde 30 min antes da queda), o Termógrafo pede só os minutos novos.

### 3.5 Ponte com o Zabbix (sem token da API)

Sem token, quem conversa com o Zabbix é um script que roda **numa aba do Zabbix já logada**: lá o `fetch(..., { credentials: 'include' })` leva o cookie de sessão. (Da página do Termógrafo isso não funciona: o navegador não manda o cookie do Zabbix para outra origem, e o `api_jsonrpc.php` do 6.0 não aceita o cookie, só `auth`.)

Como usar:
1. Servidor com `-ponte-zabbix https://zabbix.suaempresa.com.br` (a origem da aba do Zabbix). O `iniciar.bat` lê esse endereço do arquivo `zabbix-url.txt`.
2. Na aba do Zabbix, no console (F12):
   ```
   s=document.createElement('script');s.src='http://127.0.0.1:8080/ponte-zabbix.js';document.head.append(s)
   ```
   ou um favorito com `javascript:(s=>{s.src='http://127.0.0.1:8080/ponte-zabbix.js';document.head.append(s)})(document.createElement('script'))`.
3. Aparece um painel pequeno no canto da aba: "Ponte com o Termógrafo · conectada · N consultas". A aba precisa ficar aberta (pode ficar em segundo plano).

O que a ponte faz para cada pedido (host + PON + período):

| Passo | Modo **API** (cookie de sessão legível) | Modo **tela** (cookie HttpOnly, o normal) |
|---|---|---|
| achar o host | `host.get` com a sessão como `auth` | `jsrpc.php` → `multiselect.get` (hosts) |
| achar os itens da PON | `item.get` com busca "PON 2/16" | `jsrpc.php` → `multiselect.get` (items) |
| valores | `history.get` | `history.php?action=showvalues&plaintext=1` |

Itens reconhecidos pelo nome (depois de "PON 2/16"): `LOSi`, `LOFi`, `Los`, `DyingGasp` (sem energia), `Online` e `Total de onu` (total da PON). Se os nomes reais forem outros, ajuste a lista `ITENS` no começo de `ponte-zabbix.js`.

**Alarmes pela ponte (sem ação no Zabbix).** A ponte também busca a lista de **problemas abertos** do Zabbix GPON, só a cada 5 minutos (`-intervalo-alarmes`), quando a ponte conecta, ou quando alguém clica em **Buscar agora** na aba Rede (no máximo uma vez a cada 30 s). Modo API: `trigger.get` com os alarmes ativos; modo tela: o CSV do botão "Exportar para CSV" da página de Problemas (`zabbix.php?action=problem.view.csv&show=3`). Alarme novo vira chamado (mesma tradução do webhook); chamado GPON que sumiu da lista é marcado como normalizado — só quando a ponte confirma que leu a lista inteira (falha de leitura não normaliza nada). Alarmes abaixo de `-alarmes-severidade` (padrão 5 = só Desastre) ficam de fora, e chamados abaixo desse mínimo não são dados como resolvidos pela busca. Alarmes **já reconhecidos** (ack) no Zabbix não viram chamado novo — alguém já está tratando (coluna "Ack" do CSV ou `acknowledged` da API); um chamado que já existe e depois é reconhecido continua aberto, porque o alarme continua aberto. No topo da lista da Rede aparece "Alarmes buscados às hh:mm · N abertos · próxima às hh:mm · Buscar agora". No simulador, `simular -alarmes ponte` deixa os alarmes GPON só no Zabbix simulado (sem webhook), para testar esse caminho.

Se a ponte cair (aba fechada, sessão expirada), os chamados mostram "ponte com o monitoramento desconectada" e voltam a ser consultados assim que ela for ativada de novo.

**Nome dos hosts.** O padrão real `EMPRESA.CIDADE.LOCAL.EQUIPAMENTO-MARCA-NNN` (ex.: `EMP.CID.POP2.OLT-FH-001`) é lido assim: cidade pelo código (`CID`; se o código ainda não estiver cadastrado, aparece "cidade CID"), local `POP 2`, marca `FH` = Fiberhome. A PON aceita `2/16` (slot/porta) e `1/2/16`.

**Zabbix simulado.** O simulador imita o Zabbix em http://127.0.0.1:8081 (página com login, `jsrpc.php`, `history.php`, `api_jsonrpc.php`), com hosts e itens no padrão real. Para testar: servidor com `-ponte-zabbix http://127.0.0.1:8081`, abrir http://127.0.0.1:8081 e clicar em **Ativar ponte** (`/?modo=api` testa o modo API).

---

## 4. API usada pela página

| Rota | Para quê |
|---|---|
| `GET /` | página, com configuração e dados atuais embutidos |
| `GET /eventos` | tempo real (SSE): `os`, `rede`, `rede-config`, `os-tecnica` |
| `GET /api/os?depois=N` | O.S. de clientes (busca periódica quando não há SSE) |
| `GET /api/rede` | chamados de rede |
| `POST /api/rede/acao` | ações nos chamados (`acompanhar`, `recuperado`, `encerrar`…) |
| `POST /api/rede/consultar` | consultar a PON agora |
| `POST /api/rede/config` | margem de recuperação (fora da interface; padrão 75% ON) |
| `GET /api/rede/os` / `POST /api/rede/os` | listar / abrir O.S. técnica |
| `POST /api/rede/os/status` | atualizar status (recalcula os clientes afetados) |
| `POST /api/rede/os/encerrar` | encerramento pelo operador |

---

## 5. Arquivos

| Arquivo | Conteúdo |
|---|---|
| `Termógrafo.html` | a página inteira (HTML, CSS e JS) |
| `main.go`, `rede.go`, `consulta.go`, `os_tecnica.go`, `zabbix.go`, `ponte.go`, `whatsapp.go`, `zabbix_sim.go` | servidor e simuladores (`zabbix.go`: tradução das triggers; `ponte.go`: ponte com o Zabbix; `zabbix_sim.go`: Zabbix simulado) |
| `ponte-zabbix.js` | script da ponte, roda na aba do Zabbix |
| `termografo.exe` | executável (`go build -ldflags "-s -w" -o termografo.exe .`) |
| `os-recebidas.jsonl` | diário das O.S. de clientes |
| `eventos-rede.jsonl` | diário dos chamados, ações e O.S. técnicas (relido ao iniciar: nada se perde ao reiniciar) |
| `simulador-cenarios.json` | quedas do monitoramento simulado |
| `token-webhook.txt` | token dos webhooks (sigiloso) |

---

## 6. Pendências conhecidas

- Envio do comunicado ao WhatsApp: funciona com o WhatsApp simulado; falta um gateway real (API oficial do WhatsApp Business ou similar) que aceite o mesmo `POST /api/enviar`.
- Número da O.S. no formato IXC é simulado; falta a integração real com o IXC.
- Incidentes gerados pelo mapa/Monitor ficam só no navegador de quem gerou.
- O.S. técnica encerrada não pode ser reaberta.
- As ações feitas no Termógrafo (acompanhamento, O.S., baixa) ainda não voltam para o Zabbix como reconhecimento; falta conhecer as rotas de escrita.
- O formato real dos alarmes do servidor GPON não foi confirmado: a PON é lida da tag ou do nome do alarme, conforme a tabela da seção 3.2.
- Ponte com o Zabbix: testada só no Zabbix simulado. No real falta confirmar os nomes dos itens de LOSi/LOFi/DyingGasp/Online, o formato do `history.php` em texto puro e os parâmetros do `multiselect.get` de itens no 6.0.46. Os códigos de cidade reais (ex.: `CID`) ainda não estão cadastrados.
- Lista de responsáveis: um nome inicial mais os responsáveis das O.S. já abertas (não há cadastro).
