# Jungle Gaming

Base do desafio de processamento distribuído de apostas em Go, com Uber Fx, PostgreSQL, Keycloak e SQS. Inclui os cinco tipos externos, carteira e ledger, idempotência persistente, inbox/outbox, referências pendentes e API autenticada.

A base está funcional, mas não representa a conclusão de todos os critérios do desafio. As decisões estão no [índice de ADRs](ARCHITECTURE.md); limitações e verificações pendentes estão em [docs/pendencias.md](docs/pendencias.md).

## Organização

```text
cmd/
  jungle/                    # executável do monólito
  migrate/                   # executável das migrations
internal/
  app/                       # composição dos módulos
  wagering/
    module.go                # módulo de negócio financeiro
    application/             # casos de uso e autorização
    domain/                  # money, wallet, wager e event
    postgres/                # persistência financeira e inbox/outbox
    httpapi/                 # rotas e handlers do módulo
    worker/                  # consumidor, publisher e referências
  platform/
    config/                  # configuração de startup
    identity/                # identidade autenticada compartilhada
    auth/                    # verificação OIDC/JWT
    database/                # pool PostgreSQL
    queue/                   # cliente SQS
    telemetry/               # logger e contadores
    httpserver/              # listener, timeouts e shutdown
```

Cada módulo registra seus construtores e ciclo de vida com Fx. `app` monta o processo; o servidor recebe rotas por contribuições dos módulos. A plataforma não depende de `wagering`, e o domínio não depende da infraestrutura. Os testes acompanham os pacotes e verificam também essas fronteiras.

## Inicialização

Pré-requisitos: Go **1.27.1** para desenvolvimento e Docker com Compose v2. Para Podman com `podman-compose`, use a sequência explícita descrita abaixo.

```sh
cp .env.example .env
docker compose up --build -d
docker compose ps
curl --fail http://localhost:8081/health/ready
```

Algumas versões do Podman falham ao iniciar dependências que já terminaram com sucesso. Nesse caso, execute o provisionamento sequencialmente, aguardando PostgreSQL, Keycloak e LocalStack ficarem saudáveis antes de iniciar a API:

```sh
podman-compose up -d postgres keycloak-db keycloak localstack
podman-compose build migrate app
podman-compose run --rm --no-deps -T db-roles
podman-compose run --rm --no-deps -T migrate up
podman-compose run --rm --no-deps -T db-grants
podman-compose up --no-deps -d app
```

O Compose provisiona os papéis do banco (`db-roles`), executa as migrations (`migrate`) e aplica as permissões de runtime (`db-grants`) antes da API. PostgreSQL e Keycloak usam volumes persistentes; o LocalStack recria as filas ao iniciar e não mantém mensagens entre reinícios. As portas publicadas ficam restritas a `127.0.0.1`.

| Serviço | Endereço local |
| --- | --- |
| API | `http://localhost:8081` |
| Keycloak | `http://localhost:8080`, console `admin` / `admin-local` |
| PostgreSQL | `localhost:5432`, banco `jungle`; credenciais separadas abaixo |
| SQS/LocalStack | `http://localhost:4566`, região `us-east-1`, credenciais `test` / `test` |

Esses valores são exemplos de desenvolvimento. Variáveis de infraestrutura e aplicação estão em `.env.example`. O Compose lê `.env`; `go run` exige exportar as variáveis no shell.

| Papel PostgreSQL | Usuário | Senha local | Uso |
| --- | --- | --- | --- |
| Administrador | `jungle` | `jungle-local` | Provisionamento e fixtures de integração |
| Migrations | `jungle_migrator` | `jungle-migration-local` | Proprietário do schema, tabelas e funções |
| Runtime | `jungle_runtime` | `jungle-runtime-local` | API e workers, com DML restrito |

O runtime usa `DATABASE_URL`; o comando de migrations exige `MIGRATION_DATABASE_URL` e não usa a credencial de runtime como fallback. O container da aplicação recebe somente a credencial de runtime. As senhas de provisionamento são configuradas separadamente por `POSTGRES_PASSWORD`, `POSTGRES_MIGRATION_PASSWORD` e `POSTGRES_RUNTIME_PASSWORD`.

Para executar a aplicação no host:

```sh
docker compose up -d postgres keycloak-db keycloak localstack
docker compose run --rm db-roles
set -a
. ./.env
set +a
go run ./cmd/migrate up
docker compose run --rm --no-deps db-grants
go run ./cmd/jungle
```

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

O primeiro uso converte atomicamente o histórico do runner anterior para o formato `(version, dirty)`, preservando a última versão aplicada e os dados financeiros. Execute a atualização com um único tipo de executável de migrations; não rode o runner antigo e o novo simultaneamente. Uma falha deixa a versão marcada como `dirty` e bloqueia novas execuções. Investigue o SQL e o estado do banco antes de corrigir o histórico; o comando não executa `force` automaticamente. Veja a [decisão sobre migrations](docs/adr/0011-migrations.md).

A migration `000002_outbox_attempts` preserva o contador anterior como `processing_attempts` e acrescenta `publication_failures`, iniciado em zero. Execute `up` para atualizar bancos existentes antes de iniciar a nova aplicação; o Compose faz isso pelo serviço `migrate`.

A migration `000003_failed_policy` restringe `FAILED` ao código `INFRASTRUCTURE_DATA_LOSS`. Domínio e banco rejeitam outros motivos nesse estado. Timeout, indisponibilidade, erro de constraint e esgotamento de retries não justificam `FAILED`; a [política completa](docs/adr/0010-falhas-permanentes.md) define os critérios e os limites do fluxo atual. Se houver registros `FAILED` antigos com outros códigos, a migration falha para exigir investigação, sem reclassificá-los automaticamente.

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

`db-roles` transfere somente o schema e os objetos conhecidos do serviço para o migrator, preservando os dados. A execução é idempotente e também funciona em banco vazio. Não remova o volume para atualizar. Se usar o perfil `replicas`, reinicie também `app-2` e `app-3`. O provisionamento pressupõe um banco dedicado ao serviço; revoga acessos de `PUBLIC` ao banco e ao schema. Papéis com memberships inesperadas provocam erro, para exigir revisão administrativa. Veja a [decisão sobre permissões](docs/adr/0012-permissoes-do-banco.md).

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

Os endpoints do desafio estão implementados com `net/http`. `GET /wallets/{id}/ledger` aceita `limit` de 1 a 100 (padrão 50), retorna `entries` e `nextCursor`. Health checks públicos: `/health/live` e `/health/ready`. Métricas iniciais: `/metrics`.

## SQS

Filas provisionadas pelo hook do LocalStack:

- `wager-transactions.fifo` e `wager-transactions-dlq.fifo`: entrada.
- `wager-events.fifo` e `wager-events-dlq.fifo`: saída da outbox.

Redrive após cinco recebimentos; visibility timeout de 60 segundos; retenção de 14 dias. O consumidor processa uma mensagem por vez por instância, com prazo de 10 segundos. Falhas usam visibilidade exponencial até 256 segundos; em shutdown a mensagem em andamento é liberada para reentrega se não tiver sido confirmada.

Use o envelope de `DESAFIO.md` com `data.idempotencyKey`. Nesta base, a mensagem precisa também do atributo SQS `AccessToken`, do tipo `String`, contendo um token válido do provedor. O consumidor valida esse token antes de chamar o mesmo caso de uso do HTTP. Tokens expirados na entrega levam a retry/DLQ; a estratégia final de identidade durável de produtores ainda precisa ser definida.

A [matriz de redrive e autorização](docs/redrive-dlq.md) cobre envelope inválido, credencial ausente, assinatura inválida e token legítimo de outro provedor. Comprova os cinco recebimentos, os delays de retry e a chegada à DLQ sem efeitos financeiros. Cada cenário leva aproximadamente um minuto por usar o backoff real.

O [teste de recuperação do banco após DLQ](docs/recuperacao-banco-dlq.md) injeta uma falha após a escrita do ledger, comprova o rollback em cinco tentativas e reprocessa a mensagem após remover a falha. Uma nova duplicata comprova que a aplicação preserva o resultado sem outro débito.

Para publicar pela AWS CLI:

```sh
QUEUE_URL=$(aws --endpoint-url http://localhost:4566 sqs get-queue-url \
  --queue-name wager-transactions.fifo --query QueueUrl --output text)

aws --endpoint-url http://localhost:4566 sqs send-message \
  --queue-url "$QUEUE_URL" --message-body file://request.json \
  --message-group-id "$WALLET_ID" --message-deduplication-id delivery-1 \
  --message-attributes "{\"AccessToken\":{\"DataType\":\"String\",\"StringValue\":\"$PROVIDER_TOKEN\"}}"
```

Exporte `AWS_REGION`, `AWS_ACCESS_KEY_ID` e `AWS_SECRET_ACCESS_KEY` do `.env`. `request.json` deve conter o envelope completo. Para testar deduplicação da aplicação, reenvie o mesmo envelope com outro `MessageDeduplicationId`. O `messageId` do corpo é a identidade durável da inbox; reentregas com o mesmo ID e corpo diferente são conflitos.

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

Os testes criam schemas PostgreSQL e filas SQS exclusivos e os removem ao terminar. O teste de separação de permissões também cria bancos e papéis exclusivos, conecta como migrator e runtime reais e os remove ao terminar. `TEST_DATABASE_URL` deve usar um administrador do ambiente de testes, com permissão para criar esses recursos; não use a credencial de runtime nessa variável. Sem as variáveis `TEST_*`, os testes de integração são pulados explicitamente.

Para verificar somente as permissões com PostgreSQL real:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
go test -tags=integration -race ./internal/wagering/postgres -run '^TestDatabaseRoleSeparation$' -count=1
```

A suíte cobre 50 recebimentos concorrentes, duas apostas de 80 sobre saldo 100, três pools independentes, replay do resultado original, conflitos de payload e inbox, isolamento de provedores, referência pendente, proteção do ledger, paginação, lease abandonado e migrations up/down. Também inicia e encerra a composição Fx com os três serviços reais e comprova a recepção de uma duplicata por HTTP e SQS.

A [matriz de reversões e referências](docs/matriz-reversoes.md) detalha os cenários adicionais e o comando para executá-los isoladamente.

Para executar três processos independentes:

```sh
docker compose --profile replicas up --build -d
```

APIs em `8081`, `8082` e `8083`, com conexões e memória próprias. Para demonstrar a duplicidade e a disputa de saldo pelos três processos:

```sh
python3 scripts/concurrency.py
```

O script usa apenas a biblioteca padrão do Python, cria duas carteiras locais de teste com IDs aleatórios e preserva seu histórico financeiro. Verifica 50 duplicatas, duas apostas concorrentes de 80, um único débito por carteira, saldo final de 20 e reconciliação. `TEST_API_URLS` permite configurar URLs separadas por vírgula; `TEST_OIDC_ISSUER` configura o IdP. A [matriz automatizada de recuperação com três processos](docs/recuperacao-tres-processos.md) inicia subprocessos Fx independentes, injeta quedas e confere o estado após restart.

Para uma simulação manual, pare uma instância com `docker compose kill app-2`, acompanhe a recuperação da outbox e referências por outra instância, e reinicie com `docker compose up -d app-2`. O [teste de recuperação do consumidor](docs/recuperacao-consumidor.md) cobre a queda entre commit e delete com dois processos independentes. Os [testes de recuperação do publisher](docs/recuperacao-publisher.md) cobrem interrupções antes do envio e após o aceite pelo SQS, antes da confirmação SQL.

## Encerramento

```sh
docker compose logs -f app
docker compose down
```

`docker compose down -v` também remove os bancos e o realm persistidos. Alterações no JSON do realm não sobrescrevem realms existentes; use o console/Admin API ou recrie os dados locais conscientemente.
