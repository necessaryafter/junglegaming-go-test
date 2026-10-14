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

## Verificação de queda após commit

O [teste de recuperação do consumidor](../../internal/wagering/worker/consumer_recovery_integration_test.go) interrompe
um processo na chamada de delete, depois de conferir o commit por outra conexão.
Outro processo recebe a mesma mensagem após a expiração de visibilidade do SQS e
a remove após reconhecer o replay. Carteira, transações, ledger, inbox e snapshots
da outbox permanecem iguais, incluindo IDs e payloads dos eventos.

## Mensagens inválidas e redrive

Entradas rejeitadas antes do caso de uso não são marcadas como concluídas na inbox.
O consumidor mantém a mensagem, aplica backoff de visibilidade e deixa o broker
movê-la para a DLQ após o limite configurado de cinco recebimentos. O
[teste de redrive](../../internal/wagering/worker/dlq_integration_test.go) comprova esse caminho para um envelope com
campo desconhecido: corpo e atributo de credencial são preservados na DLQ, sem
alterações em carteira, transações, ledger, inbox ou eventos.
