# ADR 0003 — Transação financeira e locks

## Decisão

Usar pgx e uma transação PostgreSQL por operação. Bloquear a carteira com
`SELECT FOR UPDATE` e confirmar resultado, saldo, versão, ledger, inbox e outbox
juntos. Carteiras distintas podem avançar simultaneamente. Não há commit
intermediário de aceite em `PENDING`; a espera durável usa `PENDING_REFERENCE`.

Constraints e triggers protegem unicidade, saldo não negativo, imutabilidade do
ledger e consistência financeira no commit. Abertura positiva gera crédito e
eventos; abertura com zero não gera movimento. `LOSS` não altera saldo nem versão.
As agregações de validação no banco têm custo que precisa ser medido em carga.

## Motivo

O lock no PostgreSQL coordena todas as instâncias; um mutex em Go protegeria
apenas um processo. Gravar saldo e ledger no mesmo commit impede que uma queda
deixe os dados inconsistentes. Antes do commit, toda a operação pode ser desfeita.

## Verificação entre processos

A [matriz de recuperação](../../internal/app/cluster_recovery_integration_test.go)
inicia três aplicações Fx simultâneas, identifica seus PIDs e confirma pools
independentes pelo application_name do PostgreSQL. Exercita 50 duplicatas,
a disputa das duas apostas de 80 sobre saldo 100 e o replay depois de substituir
todo o grupo. Um lock de carteira mantido pelo teste comprova que outra carteira
avança em outra aplicação, enquanto a primeira espera por seu próprio lock.
