# Matriz de reversões e referências

A política financeira está no [ADR 0005](../adr/0005-referencias-e-reversoes.md).
Os testes de domínio estão em `internal/wagering/domain/wager/reference_test.go`;
os de integração, em `internal/wagering/postgres/reversals_integration_test.go`.

| Grupo | Cenários verificados |
| --- | --- |
| Tipos | REFUND aceita BET; ROLLBACK aceita BET, WIN e REFUND. Todas as demais combinações com os seis tipos são rejeitadas no domínio. |
| Identidade | Divergência de ID externo, provedor, jogador, carteira, rodada ou moeda; lookup SQL respeita o provedor. |
| Valor | Reversão parcial ou superior ao original é rejeitada; uma tentativa inválida não impede a reversão integral posterior. |
| Duplicidade | REFUND repetido, ROLLBACK repetido e as duas ordens de REFUND/ROLLBACK sobre uma BET. |
| Ciclos | ROLLBACK de REFUND é permitido, mas não libera a BET original para nova reversão. ROLLBACK de ROLLBACK é rejeitado. |
| Saldo | ROLLBACK de WIN ou REFUND sem saldo recebe REVERSAL_INSUFFICIENT_BALANCE; BET usa INSUFFICIENT_BALANCE. Repor o saldo permite outra operação, preservando a rejeição original. |
| Replay | Após outras movimentações, cada operação preserva seu resultado e saldo original, sem novos efeitos financeiros. |
| Concorrência | Dois REFUNDs, dois ROLLBACKs e REFUND contra ROLLBACK disputam a mesma BET em pools independentes: apenas um vence. |
| Espera | REFUND e ROLLBACK chegam antes da BET; retry resolve quando ela chega ou mantém a espera se estiver ausente ou pendente. |
| Referência sem sucesso | REJECTED e FAILED são rejeitados no domínio; integração comprova uma BET rejeitada recebida depois da reversão pendente. |
| Expiração | Referência ausente ou pendente depois de 24 horas resulta em REFERENCE_NOT_FOUND. Uma referência já processada na retomada permite conclusão. Chegada posterior à rejeição não reabre a operação. |
| Backoff | Tentativas e próximo retry são persistidos; a segunda tentativa agenda dois segundos e não duplica o evento de pendência. Trabalho ainda não vencido não é reivindicado. |

Os cenários de integração conferem estado persistido, reconciliação, ausência de
lançamento em rejeições, direção do ledger e eventos de conclusão/rejeição e saldo
na outbox. As sequências financeiras também conferem a versão da carteira.
Fixtures históricas de expiração são inseridas com criação 25 horas no passado,
sem sleeps ou desativação de triggers. Elas não simulam um restart de processo.

Com PostgreSQL real iniciado conforme o [README](../../README.md#testes):

```sh
go test ./internal/wagering/domain/wager -run '^TestReference' -count=1

TEST_DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable' \
go test -tags=integration -race ./internal/wagering/postgres \
  -run '^Test(ReversalMatrix|CompetingReversals|ReferenceRetryMatrix|ReversalReferenceMismatch)$' \
  -count=1
```

A recuperação após interrupção está coberta na
[matriz com três processos](recuperacao-tres-processos.md). Os cenários de
transporte HTTP/SQS, redrive e reprocessamento estão documentados nos testes de
[DLQ](redrive-dlq.md) e de [recuperação após falha de banco](recuperacao-banco-dlq.md).
