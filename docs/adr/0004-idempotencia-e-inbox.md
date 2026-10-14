# ADR 0004 — Idempotência persistente e inbox

## Decisão

Deduplicar por provedor, chave de idempotência e ID externo. Persistir o resultado
original, inclusive o saldo retornado. Usar SHA-256 dos campos de negócio em JSON
canônico, com dinheiro normalizado; chave e metadados de transporte ficam fora do hash.
Mesmo ID externo com outra chave é conflito. Constraints resolvem corridas;
após conflito de unicidade, o repositório tenta novamente uma vez em outra transação.

A inbox identifica a entrega por `(consumerName, messageId)` e compara o hash dos
bytes do corpo. Ela é confirmada junto com o tratamento durável. Uma referência
pendente persistida permite apagar a mensagem: o worker assume a continuidade.

## Motivo

A deduplicação no banco sobrevive a reinícios e funciona entre instâncias e
transportes. O hash detecta a reutilização de uma identidade com conteúdo diferente.
Persistir o resultado evita que um replay devolva o saldo de uma operação posterior.
