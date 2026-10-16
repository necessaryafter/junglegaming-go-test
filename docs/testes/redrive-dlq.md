# Redrive de mensagens inválidas e sem autorização

`TestInvalidMessageReachesDLQ` e `TestUnauthorizedMessageReachesDLQ`, em
`internal/wagering/worker/dlq_integration_test.go`, executam o loop de consumo de
produção contra SQS e PostgreSQL reais. A autenticação da mensagem usa
`MessageSignature`: JWS compacto destacado com EdDSA/Ed25519, assinado sobre os
bytes exatos do corpo. As fixtures geram chaves independentes para cada provedor e
configuram o registro de chaves públicas do consumidor; não colocam JWTs na fila.

O teste primeiro confere as filas provisionadas por `docker/localstack/init-sqs.sh`:
`wager-transactions.fifo` deve apontar para `wager-transactions-dlq.fifo`, com
`maxReceiveCount=5`, visibilidade de 60 segundos e ambas FIFO. Essa verificação é
somente leitura. O cenário de entrega usa suas próprias filas e schema exclusivos,
com a mesma política de redrive; não consome mensagens das filas provisionadas.

Em cada cenário, uma única mensagem contém uma BET válida de `25.00 BRL` sobre uma carteira com
`100.00 BRL`. A matriz isola o motivo da rejeição:

| Cenário | Entrada | Verificação |
| --- | --- | --- |
| Campo desconhecido | Corpo com campo adicional, assinado pela chave de provider-a | Decodificação estrita rejeita a entrada com motivo invalid_request. |
| Credencial ausente | Envelope válido, sem atributo MessageSignature | Autenticação rejeita a entrada com motivo unauthorized. |
| Assinatura inválida | Envelope válido e JWS com um byte da assinatura alterado | O verificador rejeita a assinatura mantendo o corpo e o header originais. |
| Outro provedor | Envelope válido de provider-a e assinatura legítima de provider-b | A chave é validada como provider-b antes do envio, mas a autorização do caso de uso impede a operação para provider-a. |

Um proxy observador encaminha todas as chamadas ao broker, sem alterar requisições,
mensagens, respostas ou prazos. O verificador é executado previamente para comprovar
que o cenário de outro provedor usa uma credencial válida, enquanto ausência e
assinatura inválida falham na autenticação. Isso evita confundir as duas causas.

O teste comprova:

- Recebimentos da mesma mensagem do broker com contadores de 1 a 5.
- Atualizações de visibilidade aceitas pelo SQS com delays de 2, 4, 8, 16 e 32 segundos.
- Nenhuma chamada de delete pelo consumidor e nenhum sexto recebimento entregue a ele.
- Movimentação feita pelo broker para a DLQ, preservando corpo e a presença/ausência e valor do atributo MessageSignature.
- Cinco retries e cinco logs com motivo `invalid_request` ou `unauthorized`, conforme o cenário; credenciais não aparecem nos logs.
- Fila de origem sem a mensagem após o redrive.
- Carteira, transações, ledger, inbox e eventos iguais ao snapshot inicial em cada
  tentativa e ao final; nenhuma inbox é registrada como concluída.

Cada cenário usa o backoff de produção: a soma dos delays é 62 segundos, além de startup
e chamadas de rede. Não há sleeps de sincronização, aceleração dos retries,
reenvio manual da mensagem ou transferência manual para a DLQ. A fixture tem prazo
de dois minutos, e o consumidor é cancelado e aguardado antes da limpeza.

Prepare as dependências conforme o [README](../../README.md#testes) e execute:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/wagering/worker \
  -run '^Test(Invalid|Unauthorized)MessageReachesDLQ$' -count=1 -v
```

Os quatro cenários rodam sequencialmente, com filas e schemas exclusivos por caso;
a execução completa leva aproximadamente quatro a cinco minutos. Para rodar somente
a matriz de autorização, use `-run '^TestUnauthorizedMessageReachesDLQ$'`.

A [recuperação de falha de banco após DLQ](recuperacao-banco-dlq.md) cobre uma
falha persistente de escrita e o reprocessamento em outro processo, incluindo uma
duplicata posterior. A assinatura de mensagem não possui expiração temporal;
mensagens antigas continuam verificáveis enquanto sua chave pública estiver
configurada. Os testes de `internal/platform/messageauth` cobrem essa propriedade,
a alteração do corpo e a rejeição de algoritmos ou chaves não autorizados.

A expiração de tokens permanece relevante para HTTP: a integração com Keycloak
em `TestRealDependenciesAndLifecycle` comprova aceite antes e rejeição depois da
expiração. O controle de acesso ao broker é verificado separadamente por
`TestRealProducerPermissions`, contra MiniStack com `AUTH=true`: credenciais
independentes, secret incorreta recusada, operações negadas e revogação de um
produtor sem afetar o outro. Esta matriz de DLQ comprova a autorização da mensagem
no consumidor, não substitui a verificação das políticas IAM.
