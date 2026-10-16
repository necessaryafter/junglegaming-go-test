# Métricas e diagnóstico

`GET /metrics` expõe o formato Prometheus usando o cliente oficial de Go. Cada
processo tem seu próprio registry. O scrape lê somente memória, sem consultar
PostgreSQL, SQS ou CloudWatch; continua respondendo quando essas dependências
estão indisponíveis.

## Logs dos workers

Os registros dos workers descritos abaixo usam JSON e campos selecionados
explicitamente; não registram tokens, assinaturas `MessageSignature`, chaves,
receipt handles, chave de idempotência, jogador, valores monetários, payloads
completos ou texto bruto de erros externos/SQL.

| Operação | Contexto disponível | Registros |
| --- | --- | --- |
| `sqs_consume` | `brokerMessageId`; após decoding, `messageId`, `correlationId` e `walletId`; após autenticação, `providerId`; após resultado durável, `transactionId` | Resultado durável com `status`, `failureCode` e `idempotentReplay`, remoção aceita pelo SQS, retenção para retry/redrive e falhas de delete/visibilidade. |
| `outbox_publish` | `eventId`, `walletId`, `processingAttempt`; `correlationId`, `causationId`, `transactionId` e `providerId` quando presentes no evento | Aceite ou falha de publicação e resultado da chamada de conclusão da outbox. |
| `reference_retry` | `transactionId` e `walletId` após selecionar a pendência; `providerId` e `correlationId` após ler a transação | Commit da tentativa com estado/código de rejeição, ou falha. Polls sem trabalho e cancelamento não geram erro. |

No consumidor, `providerId` vem da identidade autenticada, inclusive quando o
provedor informado no corpo é outro. `walletId` identifica a carteira solicitada;
sua presença em um log de falha não comprova autorização. `brokerMessageId` é o
ID do SQS; `messageId` é o ID durável do envelope. Campos de identificação vazios
ou com mais de 200 bytes vindos do transporte/evento são omitidos.

`SQS transaction committed` indica resultado durável disponível. Em replays,
`idempotentReplay=true` distingue o resultado recuperado de uma execução nova.
Esse registro precede o delete: uma falha de remoção pode causar reentrega sem
novo efeito financeiro. `SQS message deleted` indica aceite da chamada de delete.

`outbox publication accepted` registra aceite pelo broker. Uma falha posterior
na conclusão SQL tem outro log com o mesmo `eventId`. A conclusão é condicionada
ao lease: `outbox completion call succeeded` indica sucesso da chamada, sem
comprovar alteração de uma linha quando o lease já foi substituído.

Metadados da outbox são lidos por uma estrutura que seleciona apenas os IDs;
falha de decoding gera aviso sem conteúdo do payload e não impede publicar o
snapshot original. Não há consulta financeira adicional para enriquecer logs.

A tentativa de referência registra seu resultado no adaptador PostgreSQL, onde
os identificadores estão disponíveis, sem repetir o erro no agendador. O
`correlationId` dessa tentativa é o ID externo usado nos eventos de resolução.
O schema atual não preserva o correlation ID original nem o message ID de entrada
para esse worker; esses valores não são reconstruídos artificialmente.

## Latência e resultados

| Métrica | Labels | O que mede |
| --- | --- | --- |
| `jungle_http_request_duration_seconds` | `route`, `method`, `status` | Requisição de negócio completa, incluindo autenticação e escrita da resposta. |
| `jungle_processing_duration_seconds` | `transport`, `status` | Chamada financeira compartilhada ou tentativa de resolução de referência. |
| `jungle_transaction_results_total` | `transport`, `status` | Resultado por tentativa, incluindo replays persistidos. |
| `jungle_conflicts_total` | `transport` | Conflitos devolvidos pela aplicação em HTTP ou SQS. |
| `jungle_database_conflicts_total` | `kind` | Violações de unicidade, serialização, deadlock e indisponibilidade de lock. Inclui disputas recuperadas por replay. |

Histogramas têm buckets em segundos: `0.001`, `0.005`, `0.01`, `0.025`, `0.05`,
`0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, `60` e `+Inf`. Cada um expõe
as séries `_bucket`, `_sum` e `_count`.

`route` é o template registrado, como `/wallets/{walletID}`. URLs concretas,
IDs, tokens, nomes de filas e identificadores de provedores não são labels.
Health checks e o próprio scrape não entram no histograma HTTP. Erros de método
ou rota que o mux rejeita antes do handler também não entram.

`transport` é `http`, `sqs` ou `reference`. Resultados são `PROCESSED`, `REJECTED`,
`PENDING`, `PENDING_REFERENCE`, `FAILED` ou `ERROR`. Uma tentativa pode devolver
um estado persistido de outra tentativa; esses contadores não representam
transações únicas nem movimentações financeiras. Referências ainda pendentes
podem aparecer várias vezes. Polls sem referência elegível são excluídos.
Rejeições de autenticação ou decoding anteriores à chamada financeira aparecem
no HTTP ou no retry de SQS, sem amostra de processamento financeiro.

Os contadores existentes foram preservados: `jungle_requests_total`,
`jungle_processed_total`, `jungle_rejected_total`, `jungle_pending_total`,
`jungle_duplicates_total`, `jungle_retries_total`, `jungle_events_published_total`,
`jungle_reconciliation_divergences_total` e `jungle_processing_nanoseconds_total`.
Foi acrescentado `jungle_failed_total`. Requests, resultados e duração acumulada
agora incluem HTTP, SQS e tentativas efetivas do worker de referências.

## SQS e DLQ

| Métrica | Labels | O que mede |
| --- | --- | --- |
| `jungle_sqs_operation_duration_seconds` | `operation`, `result` | Duração de chamada ao SDK, incluindo retries internos e long polling. |
| `jungle_sqs_operations_total` | `operation`, `result` | Chamadas concluídas com `success`, `error` ou `canceled`. |
| `jungle_sqs_messages_total` | `action` | Recebidas, reentregues, deletadas, publicadas e recebimentos vazios. |
| `jungle_sqs_retries_total` | `reason` | Entregas retidas por `unauthorized`, `invalid_request`, `conflict` ou `unavailable`. |
| `jungle_sqs_queue_messages` | `queue`, `state` | Quantidades aproximadas `visible`, `inflight` e `delayed`. |
| `jungle_sqs_dlq_configured` | `queue` | Fonte com redrive configurado: `1`; sem redrive: `0`; desconhecido: `NaN`. |
| `jungle_sqs_oldest_message_age_seconds` | `queue` | Idade do mais antigo, conforme amostra recente do CloudWatch. |

Operações são `receive`, `delete`, `publish`, `visibility`, `attributes` e
`resolve`. Ações são `received`, `redelivered`, `deleted`, `published` e
`empty_receives`. Reentrega usa `ApproximateReceiveCount > 1`; contadores de
publicação contam aceite pelo SQS, inclusive eventual repetição após perda do
lease. Recebimentos vazios contam chamadas, não mensagens.

`queue` usa quatro papéis fixos: `requests`, `events`, `requests_dlq` e
`events_dlq`. O coletor descobre as DLQs pelo `RedrivePolicy` real das fontes e
resolve seus URLs por ARN. Ele não recebe mensagens da DLQ nem altera sua
visibilidade. Ausência de configuração deixa as métricas da DLQ desconhecidas;
consulte `jungle_sqs_dlq_configured` para distinguir esse caso.

A idade vem de `AWS/SQS/ApproximateAgeOfOldestMessage`, dimensão `QueueName`,
estatística `Maximum`, período de 60 segundos. A consulta escolhe a amostra mais
recente dos últimos cinco minutos. O valor é o da amostra, sem extrapolação até
o instante do scrape. Filas inativas podem não ter amostras. Para FIFO, a idade
na DLQ começa quando a mensagem entra nela, conforme a
[semântica do SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/setting-up-dead-letter-queue-retention.html).

## Coleta e indisponibilidade

Banco e filas são consultados em tarefas de background a cada 15 segundos,
com contextos limitados a três segundos por coleta de banco, por fila
(atributos e idade compartilham esse limite), ou por resolução de DLQ.

Backlog financeiro: `jungle_outbox_pending`, `jungle_outbox_oldest_seconds` e
`jungle_pending_references`. A leitura usa um único snapshot SQL.

Uma falha de coleta publica `NaN` para o valor desconhecido. Zero representa
uma observação válida. Use também:

- `jungle_database_collection_success` e
  `jungle_database_last_success_timestamp_seconds`;
- `jungle_sqs_collection_success{queue}` e
  `jungle_sqs_last_success_timestamp_seconds{queue}`;
- `jungle_sqs_age_collection_success{queue}`, que indica sucesso da chamada ao
  CloudWatch, mesmo se ela não devolver amostras;
- `jungle_sqs_oldest_message_age_available{queue}`, que indica amostra válida;
- `jungle_sqs_oldest_message_age_timestamp_seconds{queue}`, que contém o horário
  da amostra, ou zero se indisponível.

Os timestamps de último sucesso de banco/atributos são mantidos após falhas.
Valores em memória podem ficar antigos se uma tarefa travar ou parar: acompanhe
os timestamps junto dos indicadores de sucesso e a disponibilidade do processo.

## Consultas PromQL

p95 financeiro por transporte, agregando réplicas:

```promql
histogram_quantile(0.95,
  sum by (le, transport) (rate(jungle_processing_duration_seconds_bucket[5m]))
)
```

p99 HTTP por rota:

```promql
histogram_quantile(0.99,
  sum by (le, route) (rate(jungle_http_request_duration_seconds_bucket[5m]))
)
```

Conflitos por segundo:

```promql
sum by (transport) (rate(jungle_conflicts_total[5m]))
```

Mensagens visíveis nas DLQs:

```promql
max by (queue) (jungle_sqs_queue_messages{queue=~".*_dlq",state="visible"})
```

Cada réplica observa as mesmas filas e o mesmo banco. Agregue esses gauges com
`max`, preservando os labels que identificam o ambiente/banco/fila no scrape;
somar duplicaria o backlog. Para contadores por processo, use soma de taxas.

## Permissões e ambiente local

Além das permissões de negócio, a identidade do coletor precisa de
`sqs:GetQueueAttributes` e `sqs:GetQueueUrl` nas fontes e DLQs, e
`cloudwatch:GetMetricStatistics` para ler o namespace `AWS/SQS`.
O coletor não precisa de `sqs:ReceiveMessage` ou `sqs:DeleteMessage` nas DLQs.
As permissões estão em `docker/sqs/runtime-policy.json`. A identidade de runtime
recebe e confirma entradas, publica eventos e lê métricas; cada produtor tem
credenciais próprias e apenas resolve/publica na fila de entrada. As assinaturas
de mensagem vinculam a entrada ao provedor também após espera na fila ou DLQ.

O serviço chamado `localstack` no Compose usa MiniStack 1.5.27, com
`sqs,iam,sts,cloudwatch` e `AUTH=true`, para verificar políticas IAM e credenciais
independentes. O emulador não gera automaticamente a métrica de idade; localmente
ela pode aparecer como `NaN`. O teste de integração publica uma amostra controlada
no CloudWatch para validar sua leitura. O código de produção apenas lê métricas.
O SDK CloudWatch permanece em `v1.52.8`, que usa AWS Query; essa versão foi
selecionada para compatibilidade com o emulador anterior. Qualquer atualização
deve preservar os testes reais de coleta.

As credenciais administrativas locais usadas no provisionamento são exclusivas
de desenvolvimento. O runtime não recebe essas credenciais nem as chaves privadas
dos produtores. O teste `TestRealProducerPermissions` confere aceites e negativas
reais do broker, incluindo secret incorreta e revogação independente por produtor.

A cobertura inclui histogramas, limites dos buckets, scrapes concorrentes,
indisponibilidade, labels sem IDs, conflito real no PostgreSQL, fluxo HTTP/SQS
composto pelo Fx e coleta real de filas/DLQ sem consumir sua mensagem.
A regressão completa com `go test -tags=integration -race ./... -count=1` passou
com PostgreSQL, Keycloak e MiniStack reais, incluindo autorização HTTP, políticas
IAM por produtor, assinaturas de mensagem, redrive e recuperação.

Em execuções anteriores com LocalStack 4.0.3, `TestDatabaseFailureRedriveAndRecovery`
falhou intermitentemente porque o subprocesso não recebeu a mensagem reenviada.
A falha também foi reproduzida sem as mudanças de observabilidade e passou em
repetição isolada; ocorreu tanto na porta padrão quanto na alternativa. Esse
histórico não caracteriza uma falha na última regressão, nem houve alteração do
fluxo de recuperação para contorná-lo.

## Grafana e Prometheus no Compose

`docker compose up --build -d` inicia também Prometheus e Grafana. Para adicionar
somente a observabilidade a uma aplicação que já está rodando no Compose:

```sh
docker compose up -d prometheus grafana
```

Acesse [Jungle · Visão operacional](http://localhost:3000/d/jungle-overview),
na pasta `Jungle`, com `admin` / `grafana-local`. A dashboard também é a página
inicial do Grafana. `GRAFANA_PORT`, `GRAFANA_ADMIN_USER`,
`GRAFANA_ADMIN_PASSWORD` e `PROMETHEUS_PORT` podem ser definidos no `.env`.
As credenciais de administrador são aplicadas na primeira inicialização do volume;
alterar a variável depois disso não troca a senha já persistida.

O provisionamento usa a [configuração oficial do Grafana](https://grafana.com/docs/grafana/latest/administration/provisioning/):

- `docker/grafana/provisioning/datasources/prometheus.yml` registra a fonte
  `jungle-prometheus` em `http://prometheus:9090`.
- `docker/grafana/provisioning/dashboards/jungle.yml` carrega a pasta de dashboards.
- `docker/grafana/dashboards/jungle.json` define os painéis versionados. Edite esse
  arquivo para mudar a dashboard; as alterações são recarregadas automaticamente.
- `docker/prometheus/prometheus.yml` coleta `/metrics` a cada 15 segundos com
  timeout de cinco segundos, usando descoberta DNS de `app`, `app-2` e `app-3`.

As réplicas opcionais aparecem quando o perfil `replicas` está ativo, sem mudar
a configuração. Serviços ausentes do DNS não aparecem como alvos indisponíveis;
`up` indica sucesso do scrape dos alvos descobertos, sem substituir readiness.
O filtro **Instância** permite observar uma ou todas as réplicas. A configuração
padrão atende aplicações no Compose; uma aplicação executada no host exige
adaptar os alvos no arquivo do Prometheus e reiniciar o serviço.

Os painéis somam taxas de contadores por processo e usam `max` nos backlogs
compartilhados. Indicadores de coleta e timestamps são exibidos por instância
para revelar falhas parciais. Valores `NaN` permanecem desconhecidos: os painéis
não os convertem em zero. Histogramas e contadores com labels podem não existir
antes da primeira operação; taxas precisam de pelo menos dois scrapes. A idade
SQS pode ficar sem dados localmente porque o MiniStack não gera amostras sozinho.

O Prometheus fica em `http://localhost:9090`, retém 15 dias e persiste em
`prometheus-data`; o Grafana persiste em `grafana-data`. As duas portas ficam
restritas a `127.0.0.1`, seguindo os demais serviços locais. O monitoramento não
depende da saúde da API para iniciar, permitindo inspecionar sua indisponibilidade.
`docker compose down` preserva os volumes; `down -v` apaga também esses dados.

Alertas provisionados e tracing ainda não foram implementados.

## Logs no Grafana

O perfil `logs` adiciona Loki 3.7.0 e Alloy 1.14.0. Alloy descobre somente os
containers `app`, `app-2` e `app-3` do projeto Compose configurado e envia seus
logs JSON ao Loki. IDs de operação permanecem no JSON, sem labels por carteira,
provedor ou evento. Loki retém sete dias em volume local. A fonte `Loki` e o
painel **Logs das aplicações** da dashboard são provisionados automaticamente.

```sh
docker compose --profile logs up -d loki alloy
```

Para Podman rootless, habilite sua API compatível e exporte o socket:

```sh
systemctl --user start podman.socket
export CONTAINER_SOCKET="$XDG_RUNTIME_DIR/podman/podman.sock"
podman-compose --profile logs up --no-deps -d loki alloy
```

Se usar outro nome de projeto, exporte também `COMPOSE_PROJECT_NAME` com esse
nome para o filtro de descoberta do Alloy. O coletor usa acesso ao socket do
engine de containers, destinado ao ambiente local; o mount `:ro` não restringe
as operações da API. Grafana e Loki ficam publicados somente em loopback.
As aplicações não recebem esse socket.

Se o Grafana já estava iniciado, reinicie somente seu serviço para recarregar o
provisionamento, ou use sua API administrativa de reload. Na interface, abra
**Explore → Loki** e consulte:

```logql
{job="jungle"} | json
{job="jungle",service="app-2"} | json | operation="outbox_publish"
{job="jungle"} | json | level=~"WARN|ERROR"
```

O backend de métricas continua sendo Prometheus. O Loki não recebe payloads
financeiros completos ou credenciais: preserva os campos selecionados pelos
loggers da aplicação. Os comandos de carga estão em [carga](../testes/carga.md).
A configuração segue os componentes oficiais
[discovery.docker](https://grafana.com/docs/alloy/latest/reference/components/discovery/discovery.docker/) e
[loki.source.docker](https://grafana.com/docs/alloy/latest/reference/components/loki/loki.source.docker/).
