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

## Reprocessamento após falha de escrita

O [teste de recuperação do banco após DLQ](../../internal/wagering/worker/database_recovery_integration_test.go) provoca
um erro PostgreSQL após o INSERT do ledger. A transação inteira faz rollback,
incluindo carteira, operação e inbox, e a mensagem chega à DLQ após cinco tentativas.
Após remover a falha, o mesmo corpo e credencial são reenviados com outro ID de
deduplicação do transporte. A cópia na DLQ só é removida após verificar o commit.
Outro processo recebe uma duplicata real e reconhece o replay persistido, sem
novos movimentos ou eventos. Esgotar retries não transforma uma indisponibilidade
recuperável em falha financeira definitiva nem exige persistir FAILED.

## Reinício de três aplicações

A [matriz de restart](../../internal/app/cluster_rejected_integration_test.go)
substitui três aplicações depois de receber mensagens inválidas ou sem autorização.
O contador de recebimentos pertence ao SQS e não reinicia com a aplicação; chamadas
em andamento podem ser abandonadas antes de sua resposta ser observada.
A [recuperação do banco](../../internal/app/cluster_pending_integration_test.go)
retoma uma mensagem da DLQ com outro grupo de processos e confere duplicatas por
HTTP e SQS, preservando saldo, ledger, inbox e os dados imutáveis dos eventos.
