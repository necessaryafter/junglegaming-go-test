# ADR 0009 — Diagnóstico e reconciliação sem correção automática

## Decisão

Emitir logs JSON com os identificadores disponíveis e sem credenciais ou payloads
financeiros completos. Registrar no consumidor o resultado durável e o delete
separadamente; na outbox, distinguir aceite pelo SQS da conclusão SQL. Selecionar
apenas metadados de identificação do evento, sem consultas financeiras adicionais.
Registrar tentativas de referência junto dos dados selecionados, sem duplicar o
erro no agendador. IDs indisponíveis são omitidos; o provedor do consumidor vem
da identidade verificada. O [catálogo de diagnóstico](../observabilidade.md)
detalha campos e limites de correlação.

Expor resultados, duplicatas, retries, publicação,
divergências, duração acumulada e backlog/idade de outbox e referências.

Comparar saldo com a soma do ledger no snapshot de uma única instrução SQL.
Reportar, registrar e contabilizar divergências, sem alterar os dados automaticamente.
Expor histogramas de HTTP, processamento financeiro e chamadas SQS, resultados
por transporte e conflitos da aplicação e do banco. Usar o cliente oficial
Prometheus com registry por processo e labels limitados, sem IDs ou credenciais.

Coletar backlog, profundidade das filas e idade do mais antigo em background;
`/metrics` consulta apenas memória. Descobrir DLQs pelo redrive configurado e ler
idade pelo CloudWatch, sem receber mensagens. Coleta indisponível produz `NaN`,
com indicadores de sucesso, disponibilidade e timestamps para distinguir zero
real de ausência de dados. Veja o [catálogo de métricas](../observabilidade.md).
Dashboards, alertas provisionados e tracing ainda faltam.

## Motivo

Uma única leitura consistente evita divergências causadas por operações entre
duas consultas. Preservar os dados ao detectar uma diferença permite investigar
a causa sem apagar evidências. Identificadores ajudam a rastrear o processamento
sem expor tokens ou payloads financeiros completos.
