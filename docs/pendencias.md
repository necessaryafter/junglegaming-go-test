# Limitações e verificações pendentes

As verificações realizadas e os limites operacionais estão descritos abaixo.
As decisões estão no
[índice de ADRs](../ARCHITECTURE.md); os comandos de execução e teste estão no
[README](../README.md#testes).

## O que já foi verificado

- Logs dos workers: correlação entre consumo SQS e publicação, sucesso/replay/rejeição/pendência, tentativas de referência e falhas, sem credenciais ou payload financeiro. Veja os [campos e limites de correlação](guias/observabilidade.md#logs-dos-workers).

- Testes unitários: dinheiro, carteira/ledger, tipos externos, hash, estados, autorização, JWTs assinados para HTTP e assinaturas EdDSA duráveis para SQS.
- Integração real: PostgreSQL, IdP e SQS; migrations; composição e start/stop do Fx; cruzamento HTTP/SQS.
- Autorização HTTP: token real do Keycloak aceito antes da expiração e rejeitado depois dela, sem persistir uma nova aposta; isolamento entre provedores nas consultas por ID e por provedor/ID externo, sem expor dados nas respostas negadas. Cobertura em `TestRealDependenciesAndLifecycle` e configuração no [README](../README.md#testes).
- Concorrência: 50 duplicatas, disputa de saldo, três pools independentes e outbox concorrente/abandonada.
- Matriz de reversões e referências: tipos permitidos, identidade, valores, duplicidade, concorrência, saldo insuficiente, replay, backoff e expiração. Veja a [cobertura detalhada](testes/matriz-reversoes.md).
- Recuperação do consumidor após commit e antes de delete: processo encerrado à força, expiração real de visibilidade e replay em outro processo, sem novos efeitos financeiros. Veja o [cenário reproduzível](testes/recuperacao-consumidor.md).
- Recuperação do publisher antes do envio e após aceite pelo SQS, antes da confirmação SQL: queda forçada, expiração real do lease e retomada em outro processo, preservando evento e estado financeiro. Veja os [cenários reproduzíveis](testes/recuperacao-publisher.md).
- Mensagem com campo desconhecido chega à DLQ após cinco recebimentos reais, com backoff de produção, sem alteração financeira nem conclusão da inbox. Provisionamento de redrive também conferido; veja o [cenário reproduzível](testes/redrive-dlq.md).
- Mensagens sem `MessageSignature`, com assinatura inválida e com assinatura legítima de outro provedor chegam à DLQ, sem efeitos financeiros ou conclusão da inbox, preservando o atributo de assinatura quando presente. Cobertura na [matriz de redrive](testes/redrive-dlq.md).
- Identidade SQS durável: JWS destacado EdDSA sobre os bytes exatos da mensagem, com chave pública vinculada ao provedor; o consumidor não depende da validade temporal de um JWT.
- Políticas do broker: `TestRealProducerPermissions` verifica credenciais separadas no MiniStack com `AUTH=true`, operações autorizadas e negadas, secret incorreta e revogação de A sem afetar B. Runtime não pode publicar entradas, e produtores não podem consumir entradas nem publicar eventos.
- Falha persistente de escrita no PostgreSQL: cinco rollbacks completos até a DLQ, remoção da falha e reprocessamento em outro processo, seguido de duplicata recebida pela aplicação sem novo débito. Veja o [cenário de recuperação](testes/recuperacao-banco-dlq.md).
- Três aplicações Fx simultâneas: concorrência financeira, isolamento entre carteiras, reentrega HTTP/SQS, disputa da outbox, retomada e expiração de referências, DLQ e reprocessamento após restart. Veja a [matriz de três processos](testes/recuperacao-tres-processos.md).
- O perfil `replicas` e `scripts/concurrency.py` exercitam três processos por HTTP, com reconciliação final.

A matriz automatizada confere três PIDs distintos, prontidão das aplicações e três pools identificados separadamente no PostgreSQL.

Os cenários de recuperação e restart da matriz estão automatizados com
infraestrutura real. A cobertura e os limites estão descritos na
[matriz de três processos](testes/recuperacao-tres-processos.md).

## Validação do ambiente local

A regressão completa com `go test -tags=integration -race ./... -count=1` passou
contra PostgreSQL, Keycloak e MiniStack reais. Após o ajuste do health check da
imagem distroless, também passaram `go test ./...`, `go test -race ./...`,
`go vet ./...` e `go vet -tags=integration ./...`.

Em 15/10/2026, a sequência explícita de Podman Compose do README foi executada
em uma cópia Git limpa dos arquivos previstos para entrega, com volumes novos
e portas distintas. A cópia não recebeu `.env`, `.local/`, credenciais ou
estado do ambiente original; seu `.env` foi criado a partir de `.env.example`.
Foram conferidos:

- Provisionamento de banco, migrations, grants, filas e identidades IAM.
- Aplicação saudável com credencial de runtime e somente chaves públicas dos produtores.
- Prometheus e Grafana saudáveis, dashboard provisionada e coleta das métricas da aplicação.
- Publicação por A e B pelo perfil `tools`, incluindo mensagens datadas de 2000.
- Replay cruzado HTTP/SQS preservando o saldo original de `75.00`; saldo final
  de `70.00`, com três lançamentos e reconciliação consistente.
- Negativa `403` ao produtor tentando acessar a fila de saída e outbox sem eventos pendentes.

Na validação anterior, também foram conferidos a exportação dos arquivos de
runtime de um container de bootstrap encerrado e o restart de broker/aplicação
com reexecução idempotente do bootstrap, sem alterar credenciais, chaves públicas
ou saldo após replay.

Na cópia limpa também passaram `go test ./... -count=1`,
`go test -race ./... -count=1`, `go vet ./...`,
`go vet -tags=integration ./...` e a suíte completa
`go test -tags=integration -race ./... -count=1`, incluindo os quatro grupos
de recuperação documentados em `docs/testes/`. Os testes foram executados sem
reaproveitar resultados em cache. Os links locais da documentação foram conferidos.

Os comandos de publicação, execução no host, migração e rotação estão no
[README](../README.md#sqs) e na [ADR 0006](adr/0006-autenticacao-e-autorizacao.md).
Docker Compose v2 não foi executado diretamente neste ambiente; foi validada a
sequência equivalente documentada para Podman Compose.

## Limitações e melhorias opcionais

- A [política de `FAILED`](adr/0010-falhas-permanentes.md) restringe esse estado a
  perda irrecuperável de dados confirmada operacionalmente. O fluxo atual não tem
  classificador automático nem ferramenta administrativa para registrar essa decisão.
  Falhas recuperáveis fazem rollback/503 ou retry; mensagens inválidas ou sem
  autorização seguem retry/redrive. O classificador e a ferramenta administrativa
  não são requisitos adicionais impostos pelo `DESAFIO.md`.
- Histogramas, métricas de SQS/DLQ, backlog e conflitos estão implementados e
  cobertos por testes. Veja o [catálogo e limites da coleta](guias/observabilidade.md).
  Prometheus e Grafana são provisionados no Compose com dashboard operacional.
  Alertas provisionados e tracing são melhorias ainda não implementadas;
  tracing é um diferencial opcional no `DESAFIO.md`.
- As validações financeiras no banco agregam dados por carteira; seu custo ainda
  não foi medido em carga. Testes de carga são um diferencial opcional.
- O Compose separa administrador, migrator e runtime, com
  [permissões verificadas em PostgreSQL real](adr/0012-permissoes-do-banco.md).
  Ambientes externos devem provisionar os mesmos privilégios e fornecer segredos
  próprios; os valores locais de `.env.example` são apenas de desenvolvimento.
- O bootstrap SQS local não automatiza rotação e restaura seus grants ao executar.
  Rotação e revogação permanentes exigem adaptar o provisionamento, conforme a
  [ADR 0006](adr/0006-autenticacao-e-autorizacao.md#operação-e-limites).
