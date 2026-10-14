# ADR 0009 — Diagnóstico e reconciliação sem correção automática

## Decisão

Emitir logs JSON com os identificadores disponíveis e sem credenciais ou payloads
financeiros completos. Expor resultados, duplicatas, retries, publicação,
divergências, duração acumulada e backlog/idade de outbox e referências.

Comparar saldo com a soma do ledger no snapshot de uma única instrução SQL.
Reportar, registrar e contabilizar divergências, sem alterar os dados automaticamente.
Histogramas, métricas completas de SQS/DLQ e conflitos, dashboards e tracing ainda faltam.

## Motivo

Uma única leitura consistente evita divergências causadas por operações entre
duas consultas. Preservar os dados ao detectar uma diferença permite investigar
a causa sem apagar evidências. Identificadores ajudam a rastrear o processamento
sem expor tokens ou payloads financeiros completos.
