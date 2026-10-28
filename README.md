# Termógrafo

Ferramenta do CGR para acompanhar, por cidade, as O.S. de clientes com problema de conexão num mapa de calor e tratar os incidentes de rede (GPON, Core e IVR) da triagem ao encerramento — com consulta das PONs no Zabbix (pela ponte, sem token), comunicado para o grupo do WhatsApp e simuladores para testar tudo local.

## Rodar no Windows (Zabbix real)

1. Copie o **`instalar.bat`** para o PC e dê dois cliques: ele clona o repositório em `%USERPROFILE%\termografo`, baixa o `termografo.exe` da [release](https://github.com/GHSoliveira/termografo-publico/releases/latest) e chama o `iniciar.bat`. Rodar de novo atualiza.
2. No navegador, instale o **Tampermonkey** e, nele, o script **`ponte-zabbix.user.js`** (Criar novo script → colar → salvar). É ele que liga a ponte sozinho ao abrir o Zabbix.
3. Configure o endereço do seu Zabbix: crie `zabbix-url.txt` na pasta com uma linha (ex.: `https://zabbix.suaempresa.com.br`) e troque o `@match` do `ponte-zabbix.user.js` pelo mesmo endereço. Faça login no Zabbix e deixe a aba aberta. No canto aparece "Ponte com o Termógrafo · conectada". Se o Chrome pedir acesso à rede local, permita.
4. Termógrafo: http://localhost:8080 (qualquer código de entrada). Para parar: **`parar.bat`**.

O `iniciar.bat` sobe só o servidor, sem dados de exemplo, com a ponte autorizada para o Zabbix real. Na primeira vez gera um token aleatório para os webhooks em `token-webhook.txt` (não vai para o Git).

`simulacao.bat` sobe tudo simulado (O.S., quedas, Zabbix e WhatsApp de mentira), só para testar sem o Zabbix.

## Compilar

```
go build -ldflags "-s -w" -o termografo.exe .
```

A documentação completa (funções, opções, webhooks, ponte com o Zabbix, formato dos dados) está em [DOCUMENTACAO.md](DOCUMENTACAO.md).
