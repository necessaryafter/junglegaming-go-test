# Jungle Gaming

Processamento distribuído de apostas em **Go + Uber Fx**, com PostgreSQL,
Keycloak e SQS. HTTP e consumidor compartilham as garantias financeiras:
Money exato, ledger imutável, idempotência persistente e transactional outbox.

## Funcionando

![Execução real: 1.000 envios, um débito e disputa de saldo](docs/assets/demo.gif)

**1.000 envios da mesma aposta · 16 clientes concorrentes · 3 processos.**
Resultado: um processamento, 999 replays e um único débito. Depois, duas apostas
de `80.00` disputam saldo `100.00`: uma passa, outra é rejeitada, saldo `20.00`.
O GIF mostra a saída de uma execução real; [registro e comando](docs/assets/demo.json).
São 1.000 requisições no total, com até 16 em andamento.

## Inicialização

**Suporte a Docker e Podman.** Use Docker + Compose v2 ou Podman +
`podman-compose`, além de Python 3.10+. Go 1.27.1 para rodar testes Go.

```sh
cp .env.example .env
docker compose --profile replicas up --build -d
python3 scripts/concurrency.py
```

Com Podman, siga a [inicialização sequencial](docs/guias/operacao.md#inicialização)
e inicie as réplicas antes de executar a demonstração:

```sh
podman-compose --profile replicas up --no-deps -d app app-2 app-3
python3 scripts/concurrency.py
```

O último comando aguarda o Keycloak e as três APIs por até três minutos,
autentica e comprova **50 duplicatas com um débito**, disputa de saldo e
reconciliação. Ao concluir, a saída inclui:

```text
PASS: 1 processed | 49 replays | 1 debit
PASS: 1 processed | 1 INSUFFICIENT_BALANCE | balance 20.00 BRL
PASS: both wallets reconcile; no duplicate financial movement
```

Se a inicialização precisar de mais tempo, use
`python3 scripts/concurrency.py --startup-timeout 300`.
Para reproduzir o GIF:

```sh
python3 scripts/concurrency.py --duplicates 1000 --concurrency 16
```

O Compose provisiona IdP e clientes de teste, papéis do banco, migrations,
credenciais SQS, filas FIFO/DLQ, Prometheus e Grafana. Configuração e valores
locais estão em [.env.example](.env.example). Portas publicadas ficam em loopback.

- **APIs:** `localhost:8081`, `8082`, `8083`; `/health/ready` e `/metrics`.
- **Grafana:** [dashboard](http://localhost:3000/d/jungle-overview), `admin` / `grafana-local`.
- **Keycloak:** `localhost:8080`; PostgreSQL: `5432`; SQS/MiniStack: `4566`.

[Podman, execução no host e variáveis](docs/guias/operacao.md#inicialização).
Os exemplos de credenciais são exclusivos do ambiente local.

## Métricas reais

Nas três execuções mais recentes, com **oito carteiras, três aplicações e
16 clientes concorrentes**, foram medidos **148,81–182,09 req/s**, com p95 de
**163,03–234,44 ms**, zero erros HTTP inesperados e reconciliação financeira correta.

São amostras de 15 segundos em ambiente local compartilhado. A comparação foi
interrompida antes de completar as repetições previstas e não comprova ganho
consistente com os índices nem capacidade máxima. HTTP aguarda o commit
financeiro; a publicação SQS é assíncrona, e sua drenagem é medida separadamente.

[Comparação e método](docs/testes/otimizacao-validacoes.md) ·
[Relatórios e planos SQL](docs/testes/otimizacao-resultados.json) ·
[Matriz inicial: histórico, replay e conflitos](docs/testes/carga.md).

```sh
python3 scripts/load-matrix.py
```

Para logs no Grafana: [habilitar Loki + Alloy](docs/guias/observabilidade.md#logs-no-grafana).

## Autenticação e chamadas

HTTP usa OAuth 2.0/OIDC do Keycloak. `jungle-internal` administra carteiras;
`provider-a` e `provider-b` acessam somente suas transações. Exemplo autenticado:

```sh
TOKEN=$(curl -fsS http://localhost:8080/realms/jungle/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=jungle-internal \
  -d client_secret=jungle-internal-local-secret \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
PLAYER_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -fsS http://localhost:8081/wallets \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"playerId":"'"$PLAYER_ID"'","initialBalance":{"amount":"100.00","currency":"BRL"}}'
```

[Envio de apostas, consultas, reconciliação e códigos HTTP](docs/guias/operacao.md#autenticação-e-chamadas).

## SQS

O bootstrap cria `wager-transactions.fifo`, `wager-events.fifo` e suas DLQs;
redrive após cinco recebimentos. Produtores têm credenciais próprias e assinam
os bytes do envelope em `MessageSignature` — sem JWT sujeito a expirar na fila.
Para enviar o [envelope do desafio](DESAFIO.md#10-consumidor-sqs):

```sh
docker compose --profile tools run --build --rm --no-deps -T producer-a < request.json
```

[Filas, permissões, assinatura, deduplicação e rotação](docs/guias/operacao.md#sqs).

## Migrations

Executadas automaticamente na inicialização, com usuário separado do runtime.
Para aplicar e conceder os acessos de runtime:

```sh
docker compose run --rm migrate up
docker compose run --rm --no-deps db-grants
```

Para reverter todas as versões, **apagando os dados financeiros**:

```sh
docker compose stop app app-2 app-3
docker compose run --rm --no-deps migrate down
```

[Histórico, permissões e atualização de volumes existentes](docs/guias/operacao.md#migrations).

## Testes

```sh
go test ./...
go test -race ./...
go vet ./...
```

Com as dependências do Compose saudáveis, execute a integração no host:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./... -count=1
```

A integração leva vários minutos e cria schemas, filas e identidades temporários.
Use administrador de testes em `TEST_DATABASE_URL`; o broker precisa avaliar IAM.
Sem `TEST_*`, os testes de integração são pulados. A
[reprodução documentada do núcleo](docs/testes/evidencias-entrega.md) em checkout
limpo passou com **480 resultados, contando subtestes**, usando Podman Compose.

[Recuperação após falhas e matrizes de teste](docs/README.md#testes) ·
[Arquitetura e decisões](ARCHITECTURE.md) ·
[Escopo e limites operacionais](ARCHITECTURE.md#escopo-e-limites-operacionais) ·
[Guia de operação](docs/guias/operacao.md)
