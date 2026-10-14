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
