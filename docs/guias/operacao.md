# Guia de operação

Execute os comandos deste guia na raiz do repositório. O [README](../../README.md) contém o caminho rápido; aqui ficam configuração, execução no host, Podman, contratos, segurança e procedimentos de atualização.

A documentação está organizada em [docs/README.md](../README.md). As decisões estão no [índice de ADRs](../../ARCHITECTURE.md); o [escopo e os limites operacionais](../../ARCHITECTURE.md#escopo-e-limites-operacionais) estão na arquitetura e as [evidências de reprodução](../testes/evidencias-entrega.md) acompanham os cenários de teste.

## Organização

```text
cmd/
  jungle/                    # executável do monólito
  migrate/                   # executável das migrations
  healthcheck/               # readiness da imagem distroless
  publish/                   # assinatura e publicação de pedidos SQS
internal/
  app/                       # composição dos módulos
  wagering/
    module.go                # módulo de negócio financeiro
    application/             # casos de uso e autorização
    domain/                  # money, wallet, wager e event
    postgres/                # persistência financeira e inbox/outbox
    httpapi/                 # rotas e handlers do módulo
    worker/                  # consumidor, publisher, referências e coleta de métricas
  platform/
    config/                  # configuração de startup
    identity/                # identidade autenticada compartilhada
    auth/                    # verificação OIDC/JWT para HTTP
    messageauth/             # verificação de assinaturas dos produtores SQS
    database/                # pool PostgreSQL
    queue/                   # cliente SQS
    telemetry/               # logger e contadores
    httpserver/              # listener, timeouts e shutdown
```

Em `worker`, `Consumer`, `Publisher`, `References` e `StatisticsCollector` têm construtores e dependências próprios; `module.go` faz a composição e `lifecycle.go` coordena cancelamento e encerramento.

Cada módulo registra seus construtores e ciclo de vida com Fx. `app` monta o processo; o servidor recebe rotas por contribuições dos módulos. A plataforma não depende de `wagering`, e o domínio não depende da infraestrutura. Os testes acompanham os pacotes e verificam também essas fronteiras.

## Inicialização

Suporte a **Docker com Compose v2** ou **Podman com `podman-compose`**.
Go **1.27.1** para desenvolvimento. Para Podman, use a sequência explícita
descrita abaixo.

```sh
cp .env.example .env
docker compose up --build -d
docker compose ps
curl --fail http://localhost:8081/health/ready
```

Algumas versões do Podman falham ao iniciar dependências que já terminaram com sucesso. Nesse caso, execute o provisionamento sequencialmente, aguardando PostgreSQL, Keycloak e MiniStack ficarem saudáveis antes de iniciar a API:

```sh
podman-compose up -d postgres keycloak-db keycloak localstack prometheus grafana
podman-compose build migrate app
podman-compose run --rm --no-deps -T db-roles
podman-compose run --rm --no-deps -T migrate up
podman-compose run --rm --no-deps -T db-grants
podman-compose run --rm --no-deps -T queue-bootstrap
podman-compose up --no-deps -d app
```

A imagem distroless usa o executável Go `healthcheck` para consultar `/health/ready`, sem depender de shell ou `wget`.

O Compose provisiona os papéis do banco (`db-roles`), executa as migrations (`migrate`), aplica as permissões de runtime (`db-grants`) e provisiona as identidades SQS (`queue-bootstrap`) antes da API. PostgreSQL, Keycloak, o estado do MiniStack e as credenciais/chaves SQS usam volumes persistentes. As portas publicadas ficam restritas a `127.0.0.1`. O serviço `localstack` mantém esse nome por compatibilidade, mas executa **MiniStack 1.5.27**, com `AUTH=true`.

| Serviço | Endereço local |
| --- | --- |
| API | `http://localhost:8081` |
| Keycloak | `http://localhost:8080`, console `admin` / `admin-local` |
| PostgreSQL | `localhost:5432`, banco `jungle`; credenciais separadas abaixo |
| SQS/MiniStack | `http://localhost:4566`, região `us-east-1`, identidades provisionadas por papel |
| Grafana | `http://localhost:3000`, login `admin` / `grafana-local` |
| Prometheus | `http://localhost:9090` |

Esses valores são exemplos de desenvolvimento. Variáveis de infraestrutura e aplicação estão em `.env.example`. O Compose lê `.env`; `go run` exige exportar as variáveis no shell.

O Compose também inicia Prometheus e Grafana, com fonte de dados e a dashboard
**Jungle · Visão operacional** provisionadas automaticamente na pasta `Jungle`.
Acesse `http://localhost:3000/d/jungle-overview`. Há painéis de HTTP, resultados
e latência financeira, conflitos, SQS/DLQ, outbox, referências pendentes e saúde
da coleta, com filtro de instância e descoberta automática do perfil `replicas`.
Veja [configuração e limites dos painéis](observabilidade.md#grafana-e-prometheus-no-compose).

| Papel PostgreSQL | Usuário | Senha local | Uso |
| --- | --- | --- | --- |
| Administrador | `jungle` | `jungle-local` | Provisionamento e fixtures de integração |
| Migrations | `jungle_migrator` | `jungle-migration-local` | Proprietário do schema, tabelas e funções |
| Runtime | `jungle_runtime` | `jungle-runtime-local` | API e workers, com DML restrito |

O runtime usa `DATABASE_URL`; o comando de migrations exige `MIGRATION_DATABASE_URL` e não usa a credencial de runtime como fallback. O container da aplicação recebe somente a credencial de runtime. As senhas de provisionamento são configuradas separadamente por `POSTGRES_PASSWORD`, `POSTGRES_MIGRATION_PASSWORD` e `POSTGRES_RUNTIME_PASSWORD`.

Para executar a aplicação no host:

```sh
docker compose up -d postgres keycloak-db keycloak localstack queue-bootstrap
docker compose wait queue-bootstrap
docker compose run --rm db-roles
umask 077
mkdir -p .local/sqs
chmod 700 .local/sqs
docker compose cp queue-bootstrap:/runtime/credentials .local/sqs/runtime-credentials
docker compose cp queue-bootstrap:/runtime/producers.json .local/sqs/producers.json
chmod 600 .local/sqs/runtime-credentials .local/sqs/producers.json
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE
set -a
. ./.env
set +a
go run ./cmd/migrate up
docker compose run --rm --no-deps db-grants
go run ./cmd/jungle
```

Aguarde `queue-bootstrap` terminar com sucesso antes de copiar os arquivos. O comando `cp` usa o container desse serviço, inclusive depois de encerrado; por isso esse fluxo usa `up`, sem `run --rm`. Os caminhos de `.env.example` apontam para essas cópias. `.local/` é ignorado pelo Git e pelo contexto de build da imagem. A aplicação no host recebe somente a credencial de runtime e as chaves públicas; credenciais AWS exportadas anteriormente têm prioridade sobre o arquivo compartilhado, daí o `unset`. Refaça a cópia se o provisionamento substituir as credenciais ou o registro.

`OIDC_ISSUER` deve coincidir exatamente com o `iss` do token. `OIDC_JWKS_URL` permite buscar as chaves pelo endereço interno do Keycloak dentro do Compose, mantendo o issuer público. A configuração atual usa o endpoint de chaves do Keycloak; outros IdPs exigem configurar sua URL JWKS. Não há modo de autenticação desabilitada.

## Migrations

Exporte `MIGRATION_DATABASE_URL` antes de executar no host:

```sh
go run ./cmd/migrate up
go run ./cmd/migrate down
```

Ou, com containers:

```sh
docker compose run --rm migrate up
docker compose run --rm --no-deps db-grants
docker compose run --rm migrate down
```

`down` apaga as tabelas financeiras, inbox e outbox; use apenas quando quiser remover esses dados. O comando usa [golang-migrate](https://github.com/golang-migrate/migrate), com SQL embarcado via `iofs` e driver PostgreSQL `pgx/v5`. `up` aplica todas as versões pendentes e `down` reverte todas as versões aplicadas, com advisory lock gerenciado pela biblioteca. Cada arquivo SQL é atômico; o conjunto de arquivos não forma uma única transação. A aplicação não migra o banco implicitamente.

O primeiro uso converte atomicamente o histórico do runner anterior para o formato `(version, dirty)`, preservando a última versão aplicada e os dados financeiros. Execute a atualização com um único tipo de executável de migrations; não rode o runner antigo e o novo simultaneamente. Uma falha deixa a versão marcada como `dirty` e bloqueia novas execuções. Investigue o SQL e o estado do banco antes de corrigir o histórico; o comando não executa `force` automaticamente. Veja a [decisão sobre migrations](../adr/0011-migrations.md).

A migration `000002_outbox_attempts` preserva o contador anterior como `processing_attempts` e acrescenta `publication_failures`, iniciado em zero. Execute `up` para atualizar bancos existentes antes de iniciar a nova aplicação; o Compose faz isso pelo serviço `migrate`.

A migration `000003_failed_policy` restringe `FAILED` ao código `INFRASTRUCTURE_DATA_LOSS`. Domínio e banco rejeitam outros motivos nesse estado. Timeout, indisponibilidade, erro de constraint e esgotamento de retries não justificam `FAILED`; a [política completa](../adr/0010-falhas-permanentes.md) define os critérios e os limites do fluxo atual. Se houver registros `FAILED` antigos com outros códigos, a migration falha para exigir investigação, sem reclassificá-los automaticamente.

A migration `000004_wallet_validation` adiciona índices de transações por
carteira e de ledger por transação. Não altera funções, triggers nem verificações
financeiras. Sua reversão remove apenas esses dois índices, preservando os dados. Veja a
[comparação das validações](../testes/otimizacao-validacoes.md).

### Permissões e atualização de volumes existentes

O runtime recebe `SELECT/INSERT/UPDATE` em carteiras, transações, inbox e outbox; `SELECT/INSERT` no ledger; e apenas `SELECT` no histórico de migrations. Ele não é proprietário, superuser nem membro de outros papéis. Não recebe `CREATE`, `TEMPORARY`, `TRIGGER`, `DELETE`, `TRUNCATE` ou permissão para alterar `session_replication_role`. O migrator é proprietário do schema e dos objetos financeiros, mas também não é superuser. As permissões são explícitas: novas tabelas e funções não ficam acessíveis automaticamente ao runtime.

Para atualizar um volume criado com a credencial única anterior, atualize `.env` conforme `.env.example`, inclusive `DATABASE_URL` para `jungle_runtime` e `MIGRATION_DATABASE_URL` para `jungle_migrator`, e execute:

```sh
docker compose stop app app-2 app-3
docker compose run --rm db-roles
docker compose run --rm migrate up
docker compose run --rm --no-deps db-grants
docker compose up --build -d app
```

`db-roles` transfere somente o schema e os objetos conhecidos do serviço para o migrator, preservando os dados. A execução é idempotente e também funciona em banco vazio. Não remova o volume para atualizar. Se usar o perfil `replicas`, reinicie também `app-2` e `app-3`. O provisionamento pressupõe um banco dedicado ao serviço; revoga acessos de `PUBLIC` ao banco e ao schema. Papéis com memberships inesperadas provocam erro, para exigir revisão administrativa. Veja a [decisão sobre permissões](../adr/0012-permissoes-do-banco.md).

## Autenticação e chamadas

O realm `jungle` é importado automaticamente. Clientes de teste:

| Client ID | Secret local | Permissão |
| --- | --- | --- |
| `jungle-internal` | `jungle-internal-local-secret` | Carteiras, ledger e reconciliação |
| `provider-a` | `provider-a-local-secret` | Transações do provedor A |
| `provider-b` | `provider-b-local-secret` | Transações do provedor B |

Obtenha tokens usando `client_credentials`; os exemplos abaixo usam `jq`:

```sh
INTERNAL_TOKEN=$(curl --fail --silent --show-error \
  http://localhost:8080/realms/jungle/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=jungle-internal \
  -d client_secret=jungle-internal-local-secret | jq -r .access_token)

PROVIDER_TOKEN=$(curl --fail --silent --show-error \
  http://localhost:8080/realms/jungle/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a \
  -d client_secret=provider-a-local-secret | jq -r .access_token)

PLAYER_ID=0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1

WALLET_ID=$(curl --fail --silent --show-error http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d '{"playerId":"'"$PLAYER_ID"'","initialBalance":{"amount":"100.00","currency":"BRL"}}' | jq -r .id)

curl --silent --show-error http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:bet-1' \
  -d '{"providerId":"provider-a","externalTransactionId":"bet-1","playerId":"'"$PLAYER_ID"'","walletId":"'"$WALLET_ID"'","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'

curl --silent --show-error -X POST \
  "http://localhost:8081/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
```

Repetir a abertura para o mesmo jogador e moeda retorna conflito. Repetir a aposta com a mesma chave e conteúdo retorna o resultado original. As leituras de transações permitem somente o provedor autenticado, inclusive por ID interno.

| HTTP | Resultado |
| --- | --- |
| `201` | Carteira criada |
| `200` | Operação processada, replay ou consulta |
| `202` | `PENDING_REFERENCE`; consulte novamente |
| `400` | `{"code":"INVALID_REQUEST"}` |
| `401` | `{"code":"UNAUTHORIZED"}` |
| `403` | `{"code":"FORBIDDEN"}` |
| `404` | `{"code":"NOT_FOUND"}` |
| `409` | `{"code":"CONFLICT"}` |
| `422` | Operação rejeitada, com `status`, `failureCode` e saldo observado |
| `503` | `{"code":"UNAVAILABLE"}`; nova tentativa pode ser necessária |

Os endpoints do desafio estão implementados com `net/http`. `GET /wallets/{id}/ledger` aceita `limit` de 1 a 100 (padrão 50), retorna `entries` e `nextCursor`. Health checks públicos: `/health/live` e `/health/ready`. Métricas Prometheus: `/metrics`, com histogramas de latência, SQS/DLQ, backlog e conflitos. Veja o [catálogo e consultas PromQL](observabilidade.md).

## SQS

Filas provisionadas pelo hook do MiniStack:

- `wager-transactions.fifo` e `wager-transactions-dlq.fifo`: entrada.
- `wager-events.fifo` e `wager-events-dlq.fifo`: saída da outbox.

Redrive após cinco recebimentos; visibility timeout de 60 segundos; retenção de 14 dias. O consumidor processa uma mensagem por vez por instância, com prazo de 10 segundos. Falhas usam visibilidade exponencial até 256 segundos; em shutdown a mensagem em andamento é liberada para reentrega se não tiver sido confirmada.

### Identidade e permissões

Use o envelope de `DESAFIO.md` com `data.idempotencyKey`. O atributo SQS `MessageSignature`, do tipo `String`, contém uma **JWS compacta detached** (`cabeçalho..assinatura`), assinada com Ed25519/`EdDSA`. O cabeçalho protegido contém `typ=jungle-sqs-v1` e `kid`. A assinatura cobre os bytes exatos do corpo: até uma mudança de espaços invalida a assinatura. Não há access token ou prazo de expiração da assinatura na fila.

O consumidor carrega `SQS_PRODUCER_KEYS_FILE` no startup. Esse registro contém pares de `providerId` e JWK pública (`kty=OKP`, `crv=Ed25519`, `alg=EdDSA`, `use=sig`, `kid`, `x`). O `kid` seleciona uma chave já confiada localmente; a mensagem não fornece uma nova chave ou URL de descoberta. O provedor associado à chave determina a identidade. O mesmo caso de uso do HTTP exige que ela corresponda ao `data.providerId`, preservando autorização, domínio e inbox. Arquivo ausente, vazio ou inválido impede o startup. HTTP continua usando OIDC/Keycloak com expiração de tokens.

`queue-bootstrap` gera credenciais IAM e chaves de assinatura distintas para A e B, preserva o material existente e reaplica as políticas de [produtor](../../docker/sqs/producer-policy.json) e [runtime](../../docker/sqs/runtime-policy.json):

| Identidade | Permissões |
| --- | --- |
| `provider-a` / `provider-b` | `GetQueueUrl` e `SendMessage` somente na fila de pedidos |
| `jungle-runtime` | Consulta das quatro filas; consumo, delete e visibilidade somente nos pedidos; publicação somente nos eventos; leitura de métricas CloudWatch |
| Provisionamento | Criação de filas, usuários, credenciais e políticas |

A leitura CloudWatch usa `Resource: "*"`; as permissões SQS são limitadas aos ARNs provisionados. Cada produtor monta somente seu próprio volume. A aplicação monta a credencial de runtime e o registro público, sem chaves privadas dos produtores. Nenhum desses papéis pode administrar filas ou enviar pedidos em nome do runtime. O provisionamento recusa grants adicionais inesperados nos usuários que administra.

O MiniStack local usa `AUTH=true` para avaliar IAM e as credenciais assinadas. As credenciais especiais de administrador do emulador, como `test`/`test`, contornam essa avaliação e ficam no broker/provisionamento e nas fixtures administrativas. Esse ambiente é de desenvolvimento; a porta fica em loopback. Em AWS, provisione identidades e políticas equivalentes com segredos próprios e IAM nativo. A assinatura da mensagem continua necessária para associar um provedor ao corpo após entrega, retenção e redrive; ela não substitui o controle do broker. Veja a [decisão de autenticação](../adr/0006-autenticacao-e-autorizacao.md).

### Publicação

`request.json` deve conter o envelope completo e `data.providerId=provider-a`. Com a infraestrutura provisionada, publique pela ferramenta que assina e envia o mesmo corpo:

```sh
docker compose --profile tools run --build --rm --no-deps -T producer-a < request.json
```

Use `producer-b` para um envelope de `provider-b`. `--no-deps` pressupõe que `queue-bootstrap` já terminou; o comando de inicialização acima faz esse provisionamento. A ferramenta usa `data.walletId` como `MessageGroupId` e, por padrão, `messageId` como `MessageDeduplicationId`. Para exercitar a inbox sem a deduplicação FIFO do broker:

```sh
docker compose --profile tools run --build --rm --no-deps -T producer-a \
  -dedup-id delivery-2 < request.json
```

Para executar `go run ./cmd/publish` no host, exporte um arquivo de credenciais e a chave privada **do produtor**, distintos dos arquivos do runtime. Configure `AWS_SHARED_CREDENTIALS_FILE`, `SQS_SIGNING_KEY_FILE`, `SQS_SIGNING_KEY_ID`, `AWS_REGION` e `SQS_ENDPOINT`; remova credenciais AWS antigas do ambiente como no exemplo de startup. A chave é um PEM PKCS#8 Ed25519, deve permanecer privada e nunca deve ser copiada para o runtime. Os containers do perfil `tools` já configuram esses caminhos.

O `messageId` do corpo é a identidade durável da inbox; o mesmo ID com corpo diferente é conflito. A assinatura fica fora do corpo e não muda esse hash. A [matriz de redrive e autorização](../testes/redrive-dlq.md) cobre envelope inválido, assinatura ausente/adulterada e assinatura legítima de outro provedor, com cinco recebimentos, backoff real e DLQ sem efeitos financeiros.

O [teste de recuperação do banco após DLQ](../testes/recuperacao-banco-dlq.md) injeta uma falha após a escrita do ledger, comprova o rollback em cinco tentativas e reprocessa a mensagem após remover a falha. Uma nova duplicata comprova que a aplicação preserva o resultado sem outro débito.

### Atualização e rotação

Mensagens antigas com apenas `AccessToken` não são aceitas pelo novo consumidor. Antes do rollout, interrompa novas publicações antigas e drene a fila com a versão anterior, enquanto seus tokens ainda forem válidos. Para pendências/DLQ, investigue a origem e obtenha nova assinatura do produtor autorizado sobre o corpo original; preserve `messageId`, conteúdo e chave de idempotência. Use outro ID de deduplicação FIFO se necessário. Não há fallback automático para JWT expirado nem assinatura feita pelo runtime. A troca de LocalStack por MiniStack não transfere o estado antigo do broker; resolva os pedidos pendentes antes da troca.

O bootstrap local cria as chaves `provider-a-v1` e `provider-b-v1` e as preserva em volumes; ele não automatiza rotação. Para rotacionar em um ambiente administrado:

1. Gere uma nova chave privada Ed25519 e um `kid` único; acrescente sua JWK pública ao registro com o mesmo provedor, mantendo a chave antiga.
2. Distribua o registro e reinicie **todas** as instâncias consumidoras; não há recarga automática. Confirme que todas aceitam o novo `kid` antes de trocar o produtor.
3. Passe o produtor para a nova chave e seu `kid`, sem alterar o corpo ou a identidade de operações já emitidas.
4. Retenha a chave pública antiga por toda a janela de mensagens na origem, DLQ, arquivos e redrives. A retenção local de cada fila é de 14 dias; considere também atrasos e a política operacional de reprocessamento, sem assumir que 14 dias desde a rotação bastam.
5. Remova a chave pública antiga somente quando não houver mais mensagens legítimas que dependam dela e reinicie os consumidores novamente.

Revogar a credencial IAM bloqueia novos envios daquela identidade, mas não invalida assinaturas já aceitas pelo broker. Remover uma chave do registro revoga também suas mensagens ainda enfileiradas: elas seguem retry/DLQ. Em comprometimento, essa rejeição pode ser necessária; registre e investigue as mensagens afetadas. O bootstrap local reescreve o registro para as duas chaves `-v1` na próxima execução; uma rotação manual exige adaptar o provisionamento e a configuração dos produtores antes de reexecutá-lo. Ele também reaplica as políticas `Allow` e substitui credenciais IAM inativas: uma revogação permanente exige adaptar ou desabilitar esse reconciliador, para que uma nova execução não reautorize o produtor. `down -v` remove estado, credenciais e chaves, inclusive as necessárias para verificar mensagens arquivadas.

O lease da outbox é configurado por `OUTBOX_LEASE_DURATION`, com padrão `60s`. Aceita durações em milissegundos inteiros, com mínimo de `20s` para cobrir os prazos de claim e publicação. O repositório recebe essa duração explicitamente; o SQL não fixa o prazo.

`processing_attempts` conta cada reivindicação, incluindo recuperação após queda. `publication_failures` conta falhas de publicação registradas pelo dono atual do lease e determina o backoff. Uma queda sem confirmação aumenta reivindicações futuras, mas não é contabilizada automaticamente como falha de envio. O contador de falhas começa em zero na migration, pois os registros antigos não permitem reconstruí-lo com precisão.

O publisher preserva `eventId` e payload em retries. Consumidores de eventos devem ser idempotentes por `eventId`. Múltiplos publishers podem alterar a ordem de chegada; `WalletBalanceChanged` inclui `walletVersion`.

## Testes

```sh
go test ./...
go test -race ./...
go vet ./...
```

Prepare as dependências reais e execute integração:

```sh
docker compose up -d postgres keycloak-db keycloak localstack

TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./... -count=1
```

Os testes criam schemas PostgreSQL e filas SQS exclusivos e os removem ao terminar. `TEST_SQS_ENDPOINT` deve apontar para MiniStack com `AUTH=true`; um broker que ignore IAM não atende ao teste de permissões. `TestRealProducerPermissions` cria identidades IAM temporárias, aplica as políticas versionadas e comprova envios permitidos, operações negadas, secret incorreta e revogação de A sem bloquear B; remove essas identidades ao terminar. O teste de separação de permissões também cria bancos e papéis exclusivos, conecta como migrator e runtime reais e os remove ao terminar. `TEST_DATABASE_URL` deve usar um administrador do ambiente de testes, com permissão para criar esses recursos; não use a credencial de runtime nessa variável. Sem as variáveis `TEST_*`, os testes de integração são pulados explicitamente.

O teste `TestRealDependenciesAndLifecycle` também verifica isolamento entre
provedores nas duas rotas de consulta de transações e expiração real de um token
Keycloak. O cliente local `provider-a-expiring` representa `provider-a` e emite
tokens de 10 segundos pelo atributo
[`access.token.lifespan`](https://www.keycloak.org/docs-api/26.8.0/javadocs/constant-values.html).
O teste confirma acesso antes da expiração, aguarda o `exp` assinado e exige 401
nas consultas e no envio de uma nova aposta, sem persistir a operação nem mudar
a carteira. Respostas negadas devem conter apenas o código de erro.

Se o realm já existia antes da inclusão desse cliente, o import de startup não
o atualiza. Para testar com uma importação nova sem substituir o realm persistido,
inicie um Keycloak temporário em outra porta:

```sh
docker run --rm --name jungle-authorization-keycloak \
  -p 127.0.0.1:18180:8080 \
  -v "$PWD/docker/keycloak/jungle-realm.json:/opt/keycloak/data/import/jungle-realm.json:ro" \
  -e KC_HOSTNAME=http://localhost:18180 \
  quay.io/keycloak/keycloak:26.8.0 start-dev --import-realm
```

Após a mensagem de startup do Keycloak, execute em outro terminal:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:18180/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/app \
  -run '^TestRealDependenciesAndLifecycle$' -count=1 -v
```

Para verificar somente as permissões com PostgreSQL real:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
go test -tags=integration -race ./internal/wagering/postgres -run '^TestDatabaseRoleSeparation$' -count=1
```

A suíte cobre 50 recebimentos concorrentes, duas apostas de 80 sobre saldo 100, três pools independentes, replay do resultado original, conflitos de payload e inbox, isolamento de provedores, referência pendente, proteção do ledger, paginação, lease abandonado e migrations up/down. Também inicia e encerra a composição Fx com os três serviços reais e comprova a recepção de uma duplicata por HTTP e SQS.

A [matriz de reversões e referências](../testes/matriz-reversoes.md) detalha os cenários adicionais e o comando para executá-los isoladamente.

Para executar três processos independentes:

```sh
docker compose --profile replicas up --build -d
```

APIs em `8081`, `8082` e `8083`, com conexões e memória próprias. Para demonstrar a duplicidade e a disputa de saldo pelos três processos:

```sh
python3 scripts/concurrency.py
```

O script usa apenas a biblioteca padrão do Python, cria duas carteiras locais de teste com IDs aleatórios e preserva seu histórico financeiro. Verifica 50 duplicatas, duas apostas concorrentes de 80, um único débito por carteira, saldo final de 20 e reconciliação. `TEST_API_URLS` permite configurar URLs separadas por vírgula; `TEST_OIDC_ISSUER` configura o IdP. A [matriz automatizada de recuperação com três processos](../testes/recuperacao-tres-processos.md) inicia subprocessos Fx independentes, injeta quedas e confere o estado após restart.

Para repetir a demonstração do README, use
`python3 scripts/concurrency.py --duplicates 1000 --concurrency 16`.
São 1.000 requisições totais, com até 16 em andamento. Para regenerar o GIF,
instale Pillow no ambiente Python e execute, apontando uma fonte monoespaçada:

```sh
python3 scripts/record-demo.py --font /caminho/para/fonte.ttf
```

O gravador renderiza a saída real do comando, preserva os tempos das mensagens
e mantém o último quadro por cinco segundos. Só gera o GIF após sucesso;
`docs/assets/demo.json` registra comando, horário e saída para conferência.

Para uma simulação manual, pare uma instância com `docker compose kill app-2`, acompanhe a recuperação da outbox e referências por outra instância, e reinicie com `docker compose up -d app-2`. O [teste de recuperação do consumidor](../testes/recuperacao-consumidor.md) cobre a queda entre commit e delete com dois processos independentes. Os [testes de recuperação do publisher](../testes/recuperacao-publisher.md) cobrem interrupções antes do envio e após o aceite pelo SQS, antes da confirmação SQL.

## Encerramento

```sh
docker compose logs -f app
docker compose down
```

`docker compose down -v` também remove os bancos, o realm, as filas, o material criptográfico, o histórico do Prometheus e os dados do Grafana persistidos. Alterações no JSON do realm não sobrescrevem realms existentes; use o console/Admin API ou recrie os dados locais conscientemente.

## Testes de carga

O gerador usa Python 3.10+ e a biblioteca padrão. Execute com três instâncias:

```sh
docker compose --profile replicas up --build -d
python3 scripts/load-matrix.py
```

A [metodologia e os resultados](../testes/carga.md) incluem throughput,
p50/p95/p99, erros, conflitos, custo SQL das validações por carteira, atraso da
outbox e reconciliação. Relatórios brutos ficam em `.local/load/`.
Para ver os logs na dashboard existente, habilite o
[perfil Loki + Alloy](observabilidade.md#logs-no-grafana).
