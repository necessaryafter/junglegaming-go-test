# ADR 0007 — Outbox com entrega pelo menos uma vez

## Decisão

Persistir eventos no mesmo commit financeiro e publicar somente depois dele.
O repositório `postgres/outbox.go` reserva trabalho com `FOR UPDATE SKIP LOCKED`.
O lease vem de `OUTBOX_LEASE_DURATION`, com padrão de 60 segundos e mínimo de
20 segundos para cobrir os prazos de claim e envio. Receber a duração em
milissegundos inteiros mantém a consulta independente da configuração.
O envio ocorre fora da transação SQL; a confirmação exige o mesmo identificador
de lease. Após uma queda, outra instância pode assumir a publicação.

A seleção nomeia as regras `awaiting_publication`, `retry_due` e
`lease_available`, usando um único instante `claim_clock`. Contar reivindicações
em `processing_attempts` e falhas confirmadas de envio em `publication_failures`;
somente o segundo contador determina o backoff. A migration 2 preserva os claims
anteriores e inicia falhas em zero, sem alterar a migration 1.

Preservar `eventId` e snapshot do payload em retries. Usar tipos concretos,
envelopes versionados, UTC/RFC 3339 e dinheiro em strings. No SQS FIFO, agrupar
por carteira e deduplicar por `eventId`. Consumidores precisam deduplicar de forma
persistente; múltiplos publishers podem alterar a ordem de chegada.
`WalletBalanceChanged` inclui a versão da carteira.

## Motivo

A outbox impede a publicação de uma operação que não foi confirmada. Uma queda
entre envio e confirmação pode causar duplicidade, por isso o evento mantém seu ID
e o consumidor precisa deduplicar. Enviar fora da transação evita segurar locks
durante a chamada de rede.

Uma reivindicação abandonada não prova que houve uma falha de publicação.
Separar os contadores evita tratar quedas de processo como erros de envio.
