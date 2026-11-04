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
| `HUB_SESSION_KEY` | opcional | Chave (≥ 32 bytes) do cookie de sessão; padrão: derivada da chave SSO. |

Nunca coloque os valores no repositório, em PR ou em andamento de tarefa.

## Como funciona

1. No Hub, a pessoa abre o app; o Hub emite um ticket JWT de 60 s e redireciona para
   `<URL do scraper>/sso?ticket=...`.
2. O scraper valida assinatura, algoritmo (só HS256), `kind`, `iss`, `aud` e prazo, aceita cada
   ticket uma única vez e abre uma sessão própria (cookie assinado, HttpOnly, SameSite=Lax,
   8 h). A senha do Hub nunca passa pelo scraper.
3. A cada 5 minutos o scraper pergunta ao Hub se a pessoa ainda tem acesso
   (`/api/apps/module-access/<usuario>/`). Só uma resposta explícita `granted: false` encerra a
   sessão; erro de rede mantém (falha aberta).
4. Em pesquisas concluídas aparece **Enviar ao Hub**: cada empresa com telefone, site ou e-mail
   vira uma tarefa em Minhas tarefas da pessoa logada. Reenviar a mesma pesquisa não duplica.

Com o login ligado, **todas** as rotas exigem sessão, inclusive `/api/v1/*` (scripts com curl
deixam de funcionar sem o cookie).
