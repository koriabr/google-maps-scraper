# Enriquecimento por CNPJ

Na lista de resultados de uma pesquisa, cada empresa pode ter um **CNPJ** ligado. Com ele o
scraper mostra os dados da Receita: razão social, situação cadastral (ativa, baixada, inapta…),
porte, natureza jurídica, abertura (e há quantos anos), CNAE e município. Empresa **baixada ou
inapta** aparece em vermelho: não vale a pena ligar para ela.

O Google Maps **não informa o CNPJ**. Hoje a pessoa **informa** o CNPJ de cada empresa (botão
*Informar CNPJ* no modal do mapa) e o scraper consulta a Receita.

## Como funciona

- O CNPJ é validado (14 dígitos, dígitos verificadores, números de exemplo como
  `11.111.111/1111-11` são recusados) antes de qualquer consulta.
- A consulta usa a **BrasilAPI** (`https://brasilapi.com.br/api/cnpj/v1/<cnpj>`), gratuita e sem
  chave. Para usar outro serviço com o mesmo formato de resposta (por exemplo uma instância própria
  do Minha Receita), defina `CNPJ_API_URL` (o CNPJ é acrescentado ao final).
- As chamadas são espaçadas (meio segundo) e há uma nova tentativa se a API pedir para esperar.
- **Cache em tabela** (`cnpj_lookups`, no mesmo SQLite): um CNPJ já consultado não é consultado
  de novo. Encontrado vale por **30 dias**; "não existe" vale por **7 dias**. Falha de rede ou
  limite de uso **não** é guardada. Uma pesquisa nova, ou outra pessoa, aproveita o que já foi
  consultado. Consultas vencidas são apagadas ao gravar a próxima.
- O vínculo empresa → CNPJ fica em `place_cnpj` e acompanha a pesquisa: **excluir a pesquisa apaga
  os vínculos** (o cache fica, porque descreve dado público da Receita, não a pesquisa).
- Como o CNPJ é digitado por uma pessoa, o scraper **confere com o Maps**: se o município do CNPJ
  não aparece no endereço (ou o telefone também difere), mostra um aviso para checar se é a
  empresa certa.

## Privacidade (LGPD)

- Só dados da **empresa** são guardados. A lista de sócios (`qsa`) e o e-mail cadastrado na
  Receita são **descartados** na leitura, porque podem identificar pessoas.
- Empresas **MEI** são marcadas: o CNPJ de um MEI se confunde com uma pessoa física.
- A base legal para coletar contatos de terceiros do Maps continua sendo uma decisão da TI e do
  jurídico (ver `docs/koria-hub.md` e o PR do Hub).

## O que ainda não existe

- **Descobrir o CNPJ automaticamente.** Testamos achar o CNPJ no site da empresa: em 17 sites de
  transportadoras, só 1 (6%) mostrava o CNPJ, porque a maioria não o publica. A decisão tomada
  foi carregar a **base de dados abertos da Receita** no DW e casar por **telefone** e por **nome
  fantasia + cidade**. Quando existir, ela entra como outra fonte do vínculo (`place_cnpj.source`),
  sem mudar a tabela nem a tela.
- Enviar os dados da Receita para o Hub (o contrato do envio de contatos não tem esses campos;
  seria uma mudança nos dois lados).
- Incluir CNPJ e situação no CSV baixado.
