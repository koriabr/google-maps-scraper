# Integração com o Koria Hub

O modo web (`-web`) pode entrar **pelo Koria Hub**: o Hub decide quem tem acesso e o scraper
envia os contatos coletados para **Minhas tarefas** do Hub. Sem as variáveis abaixo, o app
funciona como sempre (sem login).

O contrato completo (ticket, endpoint de tarefas, limites) fica no repositório do Hub, no doc
`docs/INT-xx-gmaps-scraper-sso-contatos.md`.

## Variáveis de ambiente

| Variável | Obrigatória | Para quê |
|---|---|---|
| `HUB_SSO_SHARED_KEY` | para ligar o login | Chave HS256 (≥ 32 bytes) igual à `GMAPS_SSO_SHARED_KEY` do Hub. Vazia = sem login. |
| `HUB_URL` | recomendada | URL pública do Hub (botão "Abrir o Hub", "Voltar ao Hub", link para Minhas tarefas). |
| `HUB_API_URL` | para enviar e revalidar acesso | Base da API do Hub para chamadas servidor→servidor (rede interna). |
| `HUB_APP_TOKEN` | para enviar e revalidar acesso | `gmaps:<segredo>` do app registrado no Hub (cabeçalho `X-App-Token`). |

Nunca coloque os valores no repositório, em PR ou em andamento de tarefa.

## Como funciona

É o mesmo molde dos satélites do Hub (Engenharia, Expedição, Dash TV): o Hub decide quem entra,
o app decide o que a pessoa faz dentro.

1. No Hub, a pessoa abre o app; o Hub emite um ticket JWT de 60 s e redireciona para
   `<URL do scraper>/sso?ticket=...`. Contrato do ticket: HS256, `kind = gmaps_sso`,
   `iss = koria-hub`, `aud = gmaps`, `jti`, `iat`, `exp`, `username`, `email`, nomes.
2. O scraper valida assinatura, algoritmo (só HS256; `alg=none` é recusado), `kind`, `iss`, `aud`
   e prazo (**sem tolerância** no `exp`; `exp - iat` até 120 s; `iat` até 30 s no futuro) e aceita
   cada `jti` **uma única vez**. O `jti` fica registrado no SQLite, então um reinício não reabre
   a janela de reapresentação. Qualquer recusa mostra a **mesma** mensagem; o motivo real só vai
   para o log.
3. Abre uma **sessão própria no banco**: o navegador guarda só `<id>.<segredo>` (cookie HttpOnly,
   SameSite=Lax) e o banco guarda o SHA-256 do segredo. Encerra após **8 h sem uso** ou **7 dias**
   desde a entrada. *Sair* e a revogação de acesso encerram a sessão no servidor, então uma cópia
   do cookie deixa de funcionar. A senha do Hub nunca passa pelo scraper.
4. O scraper pergunta ao Hub se a pessoa ainda tem acesso
   (`GET /api/apps/module-access/<usuario>/`, `X-App-Token`), no máximo a cada 5 minutos por
   pessoa, aproveitando as requisições da própria tela (a lista atualiza sozinha a cada poucos
   segundos). **Só `200` com `granted: false` encerra** — e encerra todas as sessões da pessoa.
   Erro de rede, `403`, `409` ou resposta malformada significam "não sei" e mantêm a sessão
   (falha aberta, só para sessão já aberta: entrada nova continua exigindo ticket válido).
5. Em pesquisas concluídas aparece **Enviar ao Hub**: cada empresa com telefone, site ou e-mail
   vira uma tarefa em Minhas tarefas da pessoa logada. Reenviar a mesma pesquisa não duplica.

Com o login ligado, **todas** as rotas exigem sessão, inclusive `/api/v1/*` (scripts com curl
deixam de funcionar sem o cookie).

## Diferenças em relação à Engenharia (conhecidas)

- A verificação de acesso é por requisição (cache de 5 min), não por agendador em segundo plano.
  Como a tela aberta faz requisições a cada poucos segundos, o efeito prático é o mesmo.
- Sem trilha de auditoria própria nem tela de administração: entradas, recusas e revogações vão
  só para o log (sem dados de contato).
- Sem perfis locais: quem tem o módulo `gmaps` no Hub usa o app inteiro.
