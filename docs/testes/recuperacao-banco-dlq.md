# Falha de banco, DLQ e reprocessamento seguro

`TestDatabaseFailureRedriveAndRecovery`, em
`internal/wagering/worker/dlq_integration_test.go`, usa os helpers de
`database_recovery_integration_test.go` para comprovar a recuperação de uma
operação financeira após uma falha persistente de escrita.

O teste usa PostgreSQL e SQS reais no MiniStack, filas e schema exclusivos e o
mesmo consumidor, caso de uso e repositório da aplicação. A fixture compartilhada
exige também `TEST_OIDC_ISSUER`; a autenticação da mensagem usa chaves Ed25519
exclusivas de teste, geradas por `internal/testsupport/sqsidentity`, sem JWT na fila. A política de redrive e os
retries seguem a [matriz de DLQ](redrive-dlq.md): cinco recebimentos e delays de
2, 4, 8, 16 e 32 segundos.

1. Abre uma carteira com `100.00 BRL` e prepara uma BET válida de `25.00 BRL`.
2. Instala no schema de teste um trigger AFTER INSERT do ledger que lança um erro
   PostgreSQL com SQLSTATE 58000. O erro ocorre após as escritas de operação,
   inbox, carteira e ledger, dentro da transação financeira.
3. Envia a mensagem uma única vez, com `MessageSignature` JWS
   destacado EdDSA válido de `provider-a`, assinado sobre os bytes exatos do corpo. A cada
   tentativa, compara o estado financeiro ao snapshot inicial para comprovar o
   rollback completo. Uma sequence, que não sofre rollback, comprova cinco
   execuções reais do trigger; os logs registram o motivo `unavailable`.
4. Aguarda o redrive automático do broker para a DLQ. O corpo e o atributo `MessageSignature`
   são preservados, e nenhuma inbox é marcada como concluída.
5. Remove o trigger e a função de falha. Confirma que a assinatura original ainda
   é válida com o registro de chaves públicas da fixture e reenvia exatamente o mesmo corpo e atributos com um novo
   MessageDeduplicationId de transporte.
6. Um novo subprocesso recebe a mensagem e executa o handler de produção com
   conexões e memória próprias. O teste confere o commit antes de apagar a cópia
   na DLQ, preservando a possibilidade de nova tentativa caso haja outra queda.
7. Envia outra cópia com um terceiro ID de deduplicação. Outro subprocesso recebe
   essa duplicata real e comprova o replay da aplicação, sem outro débito.

Ao final, o teste exige saldo `75.00 BRL`, versão `2`, abertura e uma única BET,
um único débito de `25.00`, uma inbox concluída com o hash original e os dois
eventos financeiros da BET. Os snapshots antes/depois da duplicata permanecem
iguais. A sequence continua em cinco, e as filas de origem e DLQ terminam vazias.

O failpoint é exclusivo do schema de teste, sem interromper o banco compartilhado
ou alterar o binário de produção. Ele comprova rollback após erro de escrita;
não simula perda de rede, corrupção de disco ou resultado incerto de COMMIT. O
reenvio da DLQ é explícito pelo SDK e não usa StartMessageMoveTask. O reprocessamento
preserva o messageId do envelope, a chave de idempotência e o payload financeiro;
somente a identidade de deduplicação do transporte muda. A assinatura não tem
expiração temporal: a chave pública precisa continuar confiável durante a espera
e o reprocessamento. Remover essa chave impediria autenticar a mensagem original.

Prepare as dependências conforme o [README](../../README.md#testes) e execute:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/wagering/worker \
  -run '^TestDatabaseFailureRedriveAndRecovery$' -count=1 -v
```

A execução leva aproximadamente um a dois minutos, incluindo o backoff real,
subprocessos e verificação das filas. O auxiliar TestConsumerRecoveryProcess tem
modos de primeiro processamento e replay; seu skip na execução normal é esperado.
