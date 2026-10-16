# Recuperação do consumidor após commit

`TestConsumerRecoversAfterCommitBeforeDelete`, em
`internal/wagering/worker/consumer_recovery_integration_test.go`, verifica a queda
na janela entre o commit financeiro e a remoção da mensagem de entrada.

1. Cria schema PostgreSQL e filas FIFO exclusivos, aplica migrations e abre uma
   carteira com `100.00 BRL`.
2. Envia uma única mensagem BET de `25.00 BRL`, com `MessageSignature`
   JWS destacado EdDSA, assinado com a chave Ed25519 de `provider-a`.
3. Inicia um subprocesso do executável de testes, com suas próprias conexões e
   memória. Ele recebe a mensagem real e executa o handler de produção.
4. Um proxy de teste encaminha as chamadas ao SQS real, mas bloqueia `DeleteMessage`.
   Nesse ponto, o processo principal confirma por outra conexão que operação,
   saldo, ledger, inbox e eventos já foram persistidos.
5. Encerra o consumidor à força, sem shutdown gracioso ou delete no broker.
6. Inicia outro subprocesso, conectado diretamente ao SQS. Após expirar a
   visibilidade, ele recebe a mesma mensagem com `ApproximateReceiveCount=2`,
   reconhece o replay e a remove.
7. Confere saldo `75.00`, versão `2`, abertura e um único débito no ledger,
   uma inbox concluída com o hash original e os dois eventos financeiros da BET.
   Compara também snapshots completos das tabelas antes/depois e verifica a fila vazia.

O teste usa PostgreSQL e SQS reais no MiniStack; o proxy apenas interrompe o
delete. A fixture compartilhada exige também `TEST_OIDC_ISSUER`, mas a mensagem
não usa token do IdP. `internal/testsupport/sqsidentity` gera chaves Ed25519
exclusivas e o registro de chaves públicas, herdado pelos subprocessos. A
assinatura autentica os bytes exatos da mensagem e permanece válida após a queda,
enquanto a chave pública estiver configurada.
Não há failpoint, variável de ambiente ou endpoint de falha no binário de produção.
O subprocesso recebe pelo SDK com visibilidade de dois segundos para exercitar a
expiração real sem esperar os 60 segundos usados pela aplicação. A lógica de
validação, autenticação, submissão, replay e delete é o handler de produção.
Publishers não são iniciados nesse cenário, para comparar a outbox ainda pendente.

Prepare as dependências conforme o [README](../../README.md#testes) e execute:

```sh
TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
TEST_OIDC_ISSUER='http://localhost:8080/realms/jungle' \
TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -tags=integration -race ./internal/wagering/worker \
  -run '^TestConsumerRecoversAfterCommitBeforeDelete$' -count=1 -v
```

O auxiliar `TestConsumerRecoveryProcess` só roda quando invocado pelo teste pai;
seu skip na execução normal é esperado. Os subprocessos usam o mesmo executável,
inclusive a instrumentação de `-race`, e os recursos temporários são removidos.

Este cenário cobre dois processos consumidores consecutivos. A
[matriz com três aplicações](recuperacao-tres-processos.md) cobre instâncias
simultâneas, rejeições e pendências recuperadas após restart. O
[redrive para DLQ](redrive-dlq.md) e a
[recuperação após falha de banco](recuperacao-banco-dlq.md) também estão
automatizados. As interrupções do publisher são cobertas nos
[cenários de recuperação da outbox](recuperacao-publisher.md).
