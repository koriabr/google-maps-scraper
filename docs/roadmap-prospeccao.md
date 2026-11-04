# Evolução do módulo de prospecção

Ideias levantadas com a equipe, na ordem em que foram decididas. Nada aqui é compromisso de prazo.

## Em andamento / feito
- **CNPJ informado à mão, com cache em tabela** (`docs/cnpj.md`).

## Próximos, já aprovados em conversa
1. **Descoberta automática do CNPJ** pela base aberta da Receita no DW (casando por telefone e por
   nome fantasia + cidade). Decisão de infraestrutura: disco para a base (ordem de dezenas de GB,
   a confirmar) e carga mensal. A cobertura real do casamento ainda não foi medida.
2. **Qualidade do dado:** distinguir celular de fixo (link de WhatsApp), checar se o site responde
   e se o domínio do e-mail recebe e-mail.
3. **Higiene da carteira:** conferir os clientes atuais contra o Maps. Quem aparece como "fechado
   permanentemente" ou com telefone e endereço diferentes do cadastro pode ter o cadastro corrigido.
4. **Repetir pesquisas todo mês** e mostrar só as empresas novas e as que fecharam.
5. **Escolher o que enviar ao Hub:** marcar quais empresas, definir prazo e marcar "já enviada" ou
   "descartada" na lista. Atribuir a outra pessoa exige mudança no Hub (hoje a tarefa vai sempre
   para quem está logado).

## Base comum
Os itens 2 a 5 dependem de uma **tabela persistente de empresas** (com histórico e estado), que
ainda não existe: hoje cada pesquisa é um CSV. O CNPJ por pesquisa já usa o SQLite; a tabela de
empresas seria o passo seguinte.

## Outras ideias
Cruzar com clientes e leads (DW e PipeRun), varrer uma cidade ou estado inteiro com o modo de grade
que o código já tem, nota de encaixe por CNAE e porte, e agendamento de pesquisas.

## Riscos a decidir (não técnicos)
- **LGPD:** não há base legal documentada para coletar esses contatos.
- **Termos do Google Maps:** raspar o Maps vai contra os termos do Google; a alternativa oficial
  (API Places) é paga.
